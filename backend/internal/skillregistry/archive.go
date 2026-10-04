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

// archive.go -- unpacking a tarball a FORGE generated, which is a harder
// problem than unpacking one a registry published.
//
// # Why this is not just UnpackArtifact
//
// A registry publishes an artifact whose layout it chose to match AO's: the
// package root is the archive root. A forge generates an archive of a whole
// repository at a commit, and adds two things AO has to deal with without
// letting either become a decision the archive gets to make:
//
//  1. A SYNTHETIC TOP-LEVEL DIRECTORY, named after the owner, the repository
//     and the commit ("acme-thing-9f2c1a.../"). Every entry lives under it.
//  2. EVERYTHING ELSE IN THE REPOSITORY -- the CI config, the docs, the
//     .github directory, the other three skills in the monorepo.
//
// So this unpacker can strip one level and can root the result at a
// subdirectory. Both are bounded, both are checked against the stream rather
// than trusted from it, and neither widens what an entry is allowed to be.
//
// # What "strip one level" must not become
//
// A traversal. The strip is performed on the ALREADY-VALIDATED relative path,
// after safeArtifactPath has refused absolute paths, upward traversal,
// backslashes and NULs -- so the segment being removed is a plain name and
// what remains is still inside the destination. And exactly ONE root is
// permitted: an archive with two top-level directories is refused rather than
// merged, because merging is how two files called the same thing become one
// file chosen by whichever came last.
//
// # What "root at a subdirectory" must not become
//
// A way to reach outside the package. The prefix is validated by cleanRepoPath
// at CONFIGURATION time, entries that do not sit under it are skipped rather
// than relocated, and an empty result is a refusal -- a package root that
// matched nothing means the descriptor and the repository disagree, and
// installing "the empty directory" would pass every digest check on a tree
// that is not the package.
//
// # Every limit from artifact.go still applies
//
// Entry count, per-file size, total uncompressed size, compressed body size,
// duplicate entries, links, device nodes, FIFOs and sockets. A repository
// archive is larger and more varied than a published package, which makes the
// ceilings more likely to matter, not less.

// ArchiveOptions are how a forge-generated archive is reduced to a package
// tree. The zero value unpacks an archive whose root is already the package
// root, which is what UnpackArtifact does.
type ArchiveOptions struct {
	// StripTopLevel removes the single synthetic root directory a forge adds.
	// An archive with more than one top-level entry is refused.
	StripTopLevel bool
	// Subdir roots the result at this slash-separated path inside the archive,
	// AFTER the strip. Empty means the whole tree.
	Subdir string
	// ExcludeTop drops these top-level entries, by name, after the strip and
	// only when Subdir is empty.
	//
	// It exists for exactly one thing and is not a general filter: AO's own
	// release descriptor lives at .ao/release.json in the repository, and when
	// the package root IS the repository root the descriptor would otherwise
	// be package content. That is circular -- the descriptor declares the
	// digest of a tree that would contain the descriptor -- so the directory
	// AO reads its own metadata out of is not part of the package it measures.
	//
	// It is empty whenever Subdir is set, because a package rooted at a
	// subdirectory never contains .ao in the first place, and a filter that
	// applied there would be a filter that could silently drop a publisher's
	// real directory.
	ExcludeTop []string
}

// ArchiveReport is what an unpack actually did. It is returned rather than
// logged because the caller audits it: "which prefix was stripped" and "how
// many entries were skipped because they were outside the package" are the two
// facts that explain a digest mismatch nobody can otherwise account for.
type ArchiveReport struct {
	// Bytes is the uncompressed total written.
	Bytes int64
	// Files is how many regular files landed.
	Files int
	// Entries is how many entries the archive held, including skipped ones.
	Entries int
	// StrippedRoot is the top-level directory that was removed, or empty.
	StrippedRoot string
	// Skipped is how many entries fell outside Subdir.
	Skipped int
}

// UnpackArchive writes a gzipped tar stream into destDir under opts.
//
// It returns what it did and refuses everything artifact.go refuses. The
// caller owns destDir, has created it empty, and is responsible for hashing
// what landed -- whether these are the RIGHT bytes is not this function's
// question and never has been.
func UnpackArchive(r io.Reader, destDir string, opts ArchiveOptions) (ArchiveReport, error) {
	subdir, err := cleanRepoPath(opts.Subdir)
	if err != nil {
		return ArchiveReport{}, artifactRefusedf("package path: %v", err)
	}
	// The COMPRESSED ceiling, reported as itself. A plain io.LimitReader would
	// cut the stream mid-entry and the failure would surface as "the archive
	// is malformed", which sends somebody to look at the publisher's tarball
	// when the actual answer is that AO refused to download that much.
	body := &boundedReader{r: r, left: MaxArtifactDownloadBytes}
	gz, err := gzip.NewReader(body)
	if err != nil {
		if body.exceeded {
			return ArchiveReport{}, oversizedBodyErr()
		}
		return ArchiveReport{}, artifactRefusedf("the body is not gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return ArchiveReport{}, artifactRefusedf("create the destination: %v", err)
	}

	tr := tar.NewReader(gz)
	report := ArchiveReport{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if body.exceeded {
				return report, oversizedBodyErr()
			}
			return report, artifactRefusedf("the archive is malformed: %v", err)
		}
		report.Entries++
		if report.Entries > MaxArtifactEntries {
			return report, artifactRefusedf("the archive holds more than %d entries",
				MaxArtifactEntries)
		}
		// The SAME validation the published-artifact path uses, and it runs
		// FIRST -- before any stripping -- so the segment a strip removes is
		// already known to be a plain name.
		rel, err := safeArtifactPath(hdr.Name)
		if err != nil {
			return report, err
		}
		if rel == "" {
			continue
		}
		if err := checkArchiveEntryType(hdr, rel); err != nil {
			return report, err
		}
		if opts.StripTopLevel {
			stripped, err := stripRoot(rel, &report)
			if err != nil {
				return report, err
			}
			if stripped == "" {
				// The root directory entry itself.
				continue
			}
			rel = stripped
		}
		if subdir == "" && excludedTop(rel, opts.ExcludeTop) {
			report.Skipped++
			continue
		}
		if subdir != "" {
			within, ok := underPrefix(rel, subdir)
			if !ok {
				report.Skipped++
				continue
			}
			if within == "" {
				continue
			}
			rel = within
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return report, artifactRefusedf("create %s: %v", rel, err)
			}
		case tar.TypeReg:
			written, err := writeArtifactFile(tr, target, rel, report.Bytes)
			report.Bytes += written
			if err != nil {
				if body.exceeded {
					return report, oversizedBodyErr()
				}
				return report, err
			}
			report.Files++
		default:
			// Unreachable: checkArchiveEntryType already refused everything
			// that is not a directory or a regular file. It is here because a
			// switch whose default is "silently ignore" is a switch that grows
			// a case somebody forgets to handle.
			return report, artifactRefusedf("%s is not a regular file or directory", rel)
		}
	}
	if report.Files == 0 {
		if subdir != "" && report.Skipped > 0 { //nolint:nestif // three distinct refusals.
			// The specific, findable failure: the descriptor named a package
			// root the repository does not have. Installing an empty tree
			// would hash to something, match nothing, and produce a digest
			// mismatch nobody could explain.
			return report, artifactRefusedf("no file in the archive is under %q; the release names "+
				"a package root this commit does not contain", subdir)
		}
		return report, artifactRefusedf("the archive contains no files")
	}
	return report, nil
}

// boundedReader stops at a byte ceiling and REMEMBERS that it did.
//
// The memory is the whole point. Every reader downstream -- gzip, tar, the
// per-file copy -- reports a truncated stream as its own kind of corruption,
// and each of those messages sends an operator somewhere different. One flag
// read at each of those three points turns all of them back into the one true
// sentence: the body was larger than AO will download.
type boundedReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		b.exceeded = true
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left <= 0 {
		// One more byte would have arrived, so the body is over the ceiling
		// rather than exactly at it. Reading past the limit is what proves
		// the difference.
		var probe [1]byte
		if extra, _ := b.r.Read(probe[:]); extra > 0 {
			b.exceeded = true
		}
	}
	return n, err
}

func oversizedBodyErr() error {
	return artifactRefusedf("the archive body is larger than %d bytes, which is the most AO will "+
		"download for one package", MaxArtifactDownloadBytes)
}

// checkArchiveEntryType refuses every tar entry type a package has no use for.
//
// Each refusal names what it found, because "the archive is malformed" sends
// somebody to the wrong place. A forge archive legitimately contains symlinks
// -- plenty of repositories have them -- and AO still refuses one, because a
// symlink in AO's catalog is a read of this host through a path the publisher
// chose.
func checkArchiveEntryType(hdr *tar.Header, rel string) error {
	switch hdr.Typeflag {
	case tar.TypeDir, tar.TypeReg:
		return nil
	case tar.TypeSymlink, tar.TypeLink:
		kind := "symlink"
		if hdr.Typeflag == tar.TypeLink {
			kind = "hard link"
		}
		return artifactRefusedf("%s is a %s to %q, and a package is regular files only; a link in "+
			"an archive becomes a link on this host pointing wherever the publisher chose",
			rel, kind, hdr.Linkname)
	case tar.TypeChar, tar.TypeBlock:
		return artifactRefusedf("%s is a device node, which nothing in a package needs", rel)
	case tar.TypeFifo:
		return artifactRefusedf("%s is a FIFO, which nothing in a package needs", rel)
	}
	return artifactRefusedf("%s is not a regular file or directory (tar type %q)",
		rel, string(rune(hdr.Typeflag)))
}

// stripRoot removes the archive's single synthetic top-level directory.
//
// The first entry decides what that directory is, and every later entry must
// agree. An archive with two roots is refused rather than merged: merging
// would let "a/skill.yaml" and "b/skill.yaml" both land as "skill.yaml", and
// which one survives would be decided by the archive's own ordering.
func stripRoot(rel string, report *ArchiveReport) (string, error) {
	root, rest, _ := strings.Cut(rel, "/")
	if report.StrippedRoot == "" {
		report.StrippedRoot = root
	}
	if root != report.StrippedRoot {
		return "", artifactRefusedf("the archive has more than one top-level directory (%q and "+
			"%q); a repository archive has exactly one, and merging two would let two different "+
			"files land under one name", report.StrippedRoot, root)
	}
	return path.Clean(rest), nil
}

// excludedTop reports whether rel's first segment is one AO drops.
//
// It compares the SEGMENT, not a prefix: ".aother/file" must not be excluded
// by an entry of ".ao", and a string prefix test is how it would be.
func excludedTop(rel string, exclude []string) bool {
	if len(exclude) == 0 {
		return false
	}
	head, _, _ := strings.Cut(rel, "/")
	for _, name := range exclude {
		if head == name {
			return true
		}
	}
	return false
}

// underPrefix reports whether rel sits under prefix, and returns the remainder.
//
// It compares SEGMENTS rather than strings, because "skills/audit" must not
// match "skills/auditor": a prefix test written with strings.HasPrefix is how
// a neighbouring directory ends up inside the package.
func underPrefix(rel, prefix string) (string, bool) {
	if rel == prefix {
		return "", true
	}
	if !strings.HasPrefix(rel, prefix+"/") {
		return "", false
	}
	return rel[len(prefix)+1:], true
}

// GitHubDescriptorDir is the directory AO reads its own release descriptor
// out of, relative to the repository root.
const GitHubDescriptorDir = ".ao"

// ArchiveOptionsFor is the unpack configuration one external source calls for.
func ArchiveOptionsFor(src GitSource) ArchiveOptions {
	opts := ArchiveOptions{StripTopLevel: true, Subdir: strings.TrimSpace(src.Path)}
	if opts.Subdir == "" {
		opts.ExcludeTop = []string{GitHubDescriptorDir}
	}
	return opts
}

// DescribeArchive renders what an unpack did, for an audit line.
func DescribeArchive(report ArchiveReport) string {
	out := fmt.Sprintf("%d files, %d bytes uncompressed, from %d archive entries",
		report.Files, report.Bytes, report.Entries)
	if report.StrippedRoot != "" {
		out += fmt.Sprintf("; stripped the archive root %q", report.StrippedRoot)
	}
	if report.Skipped > 0 {
		out += fmt.Sprintf("; skipped %d entries outside the package root", report.Skipped)
	}
	return out
}
