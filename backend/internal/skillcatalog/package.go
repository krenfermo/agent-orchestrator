package skillcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	yaml "gopkg.in/yaml.v3"
)

// Package is a loaded, validated skill package on disk.
type Package struct {
	Manifest Manifest
	// Dir is the package root (the directory holding skill.yaml).
	Dir string
	// Digest is the digest computed from the package's actual contents, not
	// the one the manifest claims.
	Digest string
}

// DecodeManifest parses a manifest with unknown fields rejected. A typo in a
// security-relevant key must be an error, not a silently dropped restriction:
// `capabilties: []` accepted as "no capabilities declared" would be the worst
// possible reading of a misspelling.
func DecodeManifest(r io.Reader) (Manifest, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, invalidf("parse manifest: %v", err)
	}
	// A second document in the file would be silently ignored, and a manifest
	// is a single declaration.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return Manifest{}, invalidf("manifest must contain exactly one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return Manifest{}, invalidf("parse manifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// LoadPackage reads and validates the package rooted at dir: the manifest
// parses strictly, the digest matches the actual bytes on disk, and every
// package-relative path the manifest points at exists.
func LoadPackage(dir string) (Package, error) {
	manifestPath := filepath.Join(dir, ManifestFileName)
	f, err := os.Open(manifestPath) //nolint:gosec // caller-supplied package root.
	if err != nil {
		return Package{}, fmt.Errorf("skillcatalog: open %s: %w", ManifestFileName, err)
	}
	defer func() { _ = f.Close() }()

	m, err := DecodeManifest(f)
	if err != nil {
		return Package{}, err
	}

	digest, err := ComputePackageDigest(dir)
	if err != nil {
		return Package{}, err
	}
	if digest != m.Integrity.Digest {
		return Package{}, fmt.Errorf("%w: integrity.digest %s does not match package contents %s",
			ErrInvalidManifest, m.Integrity.Digest, digest)
	}

	for _, ref := range m.packageRefs() {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(ref))); err != nil {
			return Package{}, invalidf("package file %q referenced by the manifest is missing", ref)
		}
	}

	return Package{Manifest: m, Dir: dir, Digest: digest}, nil
}

// packageRefs are every package-relative file the manifest points at.
func (m Manifest) packageRefs() []string {
	refs := make([]string, 0, 1+len(m.Modes))
	refs = append(refs, m.Outputs.SchemaRef)
	for _, mode := range m.Modes {
		refs = append(refs, mode.Guide)
	}
	return refs
}

// ComputePackageDigest hashes every file in the package except the manifest
// itself, which cannot cover the digest it carries. Paths are included in the
// hash so moving a file changes the digest, and entries are sorted so the
// result does not depend on directory iteration order.
func ComputePackageDigest(dir string) (string, error) {
	h := sha256.New()
	var entries []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFileName {
			return nil
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("skillcatalog: walk package %q: %w", dir, err)
	}
	sort.Strings(entries)
	for _, rel := range entries {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // path derived from the walked root.
		if err != nil {
			return "", fmt.Errorf("skillcatalog: read %q: %w", rel, err)
		}
		// Hash writers never fail, so the length prefix cannot short-write.
		_, _ = fmt.Fprintf(h, "%s\n%d\n", rel, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyPackage copies a validated package tree into destDir. It refuses
// anything that is not a regular file: a symlink inside a package could point
// at a credential outside it and the copy would follow it into the catalog.
func copyPackage(srcDir, destDir string) error {
	if err := os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("skillcatalog: clear %q: %w", destDir, err)
	}
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		target := destDir
		if rel != "." {
			target = filepath.Join(destDir, rel)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("skillcatalog: package entry %q is not a regular file", filepath.ToSlash(rel))
		}
		b, err := os.ReadFile(p) //nolint:gosec // path derived from the walked root.
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}
