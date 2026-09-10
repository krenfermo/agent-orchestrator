package skillregistry

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// artifact.go -- turning a stream from a remote registry into a directory,
// without letting the stream decide anything.
//
// A local registry hands AO a directory somebody already put on this host. A
// remote one hands AO a tar.gz written by whoever controls the registry, and
// every field in it is attacker-controlled: the entry names, the entry types,
// the sizes, the count, and how well it compresses. So this file trusts none of
// them and bounds all of them.
//
// # What is refused, and why each one
//
//	an absolute or upward path      writes outside the quarantine
//	a symlink or hardlink           becomes a link in AO's catalog pointing at
//	                                a credential on this host; skillcatalog's
//	                                CopyPackage refuses one later, and refusing
//	                                it here means it never lands at all
//	a device, fifo or socket        a file type nothing in a package needs
//	a setuid/setgid mode bit        every file is written 0600 regardless
//	more than MaxArtifactEntries    an inode exhaustion bomb
//	one entry over MaxArtifactFileBytes, or a total over MaxArtifactBytes
//	                                a decompression bomb: 40 KB of gzip is
//	                                10 GB of zeros, and a limit on the
//	                                COMPRESSED body does not bound it
//
// The uncompressed budget is enforced with an io.LimitedReader per entry AND a
// running total, because a tar header's declared Size is also attacker-
// controlled: a header that says 10 bytes and streams 10 GB has to hit a wall
// that is not the header.

// The transport-level limits. They are constants rather than configuration on
// purpose: a per-registry override would be a per-registry way to turn the
// limit off, and the number that matters is "bigger than any real skill and
// smaller than anything that hurts".
const (
	// MaxArtifactDownloadBytes bounds the COMPRESSED body AO will read.
	MaxArtifactDownloadBytes int64 = 64 << 20 // 64 MiB
	// MaxArtifactBytes bounds the total UNCOMPRESSED size.
	MaxArtifactBytes int64 = 256 << 20 // 256 MiB
	// MaxArtifactFileBytes bounds one file inside the package.
	MaxArtifactFileBytes int64 = 32 << 20 // 32 MiB
	// MaxArtifactEntries bounds how many entries a package may contain.
	MaxArtifactEntries = 4096
	// MaxMetadataBytes bounds a metadata response. A search answer that needs
	// more than this is a registry answering a different question than the one
	// asked.
	MaxMetadataBytes int64 = 8 << 20 // 8 MiB
)

// ErrArtifactRefused marks an artifact stream AO would not unpack.
var ErrArtifactRefused = errors.New("skillregistry: artifact refused")

func artifactRefusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrArtifactRefused, fmt.Sprintf(format, args...))
}

// ArtifactMediaType is the content type an AO skill package is served as.
const ArtifactMediaType = "application/vnd.ao.skill-package.v1+tar+gzip"

// UnpackArtifact writes a gzipped tar stream into destDir, which the caller
// owns and has already created empty.
//
// It returns the number of uncompressed bytes written. Whether those bytes are
// the RIGHT bytes is deliberately not its job: the caller hashes what landed
// and compares against the resolved release, which is the only comparison that
// means anything.
func UnpackArtifact(r io.Reader, destDir string) (int64, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, MaxArtifactDownloadBytes+1))
	if err != nil {
		return 0, artifactRefusedf("the body is not gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var total int64
	entries := 0
	sawFile := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return total, artifactRefusedf("the archive is malformed: %v", err)
		}
		entries++
		if entries > MaxArtifactEntries {
			return total, artifactRefusedf("the archive holds more than %d entries", MaxArtifactEntries)
		}
		rel, err := safeArtifactPath(hdr.Name)
		if err != nil {
			return total, err
		}
		if rel == "" { // the archive's own root entry
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return total, artifactRefusedf("create %s: %v", rel, err)
			}
		case tar.TypeReg:
			written, err := writeArtifactFile(tr, target, rel, total)
			total += written
			if err != nil {
				return total, err
			}
			sawFile = true
		case tar.TypeSymlink, tar.TypeLink:
			// The one that matters. A link in a package becomes a link in AO's
			// catalog, and a link in AO's catalog is a read of this host.
			return total, artifactRefusedf("%s is a link, and a package is regular files only; "+
				"a link in a registry becomes a link on this host pointing wherever the registry chose", rel)
		default:
			return total, artifactRefusedf("%s is not a regular file or directory (tar type %q)",
				rel, string(rune(hdr.Typeflag)))
		}
	}
	if !sawFile {
		return total, artifactRefusedf("the archive contains no files")
	}
	return total, nil
}

// writeArtifactFile copies one entry under both budgets.
func writeArtifactFile(tr io.Reader, target, rel string, alreadyWritten int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, artifactRefusedf("create the directory for %s: %v", rel, err)
	}
	// The remaining budget is the smaller of "what this file may be" and "what
	// the whole package has left". Reading one byte past it is what proves the
	// stream lied, so the limit is +1.
	remaining := MaxArtifactBytes - alreadyWritten
	if remaining <= 0 {
		return 0, artifactRefusedf("the archive expands past %d bytes", MaxArtifactBytes)
	}
	if remaining > MaxArtifactFileBytes {
		remaining = MaxArtifactFileBytes
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Two entries for one path: whichever is read last would win, and
			// which one that is depends on the archive.
			return 0, artifactRefusedf("%s appears twice in the archive", rel)
		}
		return 0, artifactRefusedf("create %s: %v", rel, err)
	}
	written, copyErr := io.Copy(f, io.LimitReader(tr, remaining+1))
	closeErr := f.Close()
	if copyErr != nil {
		return written, artifactRefusedf("read %s: %v", rel, copyErr)
	}
	if closeErr != nil {
		return written, artifactRefusedf("write %s: %v", rel, closeErr)
	}
	if written > remaining {
		return written, artifactRefusedf("%s expands past the %d-byte budget left for this package; "+
			"a small download that expands without bound is a decompression bomb", rel, remaining)
	}
	return written, nil
}

// safeArtifactPath reduces a tar entry name to a relative path inside the
// destination, or refuses it.
//
// It returns "" for the archive's own root ("." or "./"), which is an ordinary
// entry and not an error.
func safeArtifactPath(name string) (string, error) {
	cleaned := path.Clean(strings.TrimSpace(name))
	switch cleaned {
	case "", ".", "/":
		return "", nil
	}
	if path.IsAbs(cleaned) || strings.HasPrefix(name, "/") {
		return "", artifactRefusedf("entry %q is an absolute path", name)
	}
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", artifactRefusedf("entry %q traverses above the package root", name)
	}
	if strings.ContainsAny(cleaned, `\:`) {
		// A backslash is a separator on Windows and a legal filename character
		// here, which is exactly the ambiguity a traversal hides in.
		return "", artifactRefusedf("entry %q is not a slash-separated relative path", name)
	}
	if strings.ContainsRune(cleaned, 0) {
		return "", artifactRefusedf("entry name contains a NUL byte")
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", artifactRefusedf("entry %q traverses upward", name)
		}
	}
	return cleaned, nil
}
