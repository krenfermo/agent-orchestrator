package skillcatalog

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// builtin.go ships the packages AO itself publishes inside the binary, so an
// installation has them available without anybody copying a directory around.
//
// Available is not enabled. Installing a builtin creates an install row and
// nothing else: no project gains a capability because AO was upgraded, and
// activation and its grant stay the separate, audited act they have always
// been (ADR 0003).

//go:embed all:packages/security-audit
var builtinFS embed.FS

// BuiltinPackageIDs are the packages embedded in this build.
var BuiltinPackageIDs = []string{"security-audit"}

// MaterializeBuiltin writes the embedded package id into dest (which must not
// exist yet) with private permissions, so it can be installed through the same
// verified path as any local package.
func MaterializeBuiltin(id, dest string) error {
	known := false
	for _, b := range BuiltinPackageIDs {
		if b == id {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("skillcatalog: %q is not a builtin package", id)
	}
	root := path.Join("packages", id)
	return fs.WalkDir(builtinFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.FromSlash(p))
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

// MatchesBuiltin reports whether the package at dir is, byte for byte, the
// builtin package id that this binary embeds -- every file, the manifest
// included, and nothing more.
//
// It is the ONLY evidence of "builtin" a host agent may act on (ADR 0010).
// The manifest's origin.type is a claim any package can make about itself, and
// the package digest deliberately excludes skill.yaml, so a digest match alone
// would accept a builtin's guides under a rewritten manifest. Comparing the
// installed bytes against the embedded ones leaves nothing to claim.
func MatchesBuiltin(id, dir string) (bool, error) {
	known := false
	for _, b := range BuiltinPackageIDs {
		if b == id {
			known = true
		}
	}
	if !known {
		return false, nil
	}
	root := path.Join("packages", id)
	want := map[string][]byte{}
	err := fs.WalkDir(builtinFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		want[strings.TrimPrefix(p, root+"/")] = b
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("skillcatalog: read embedded %s: %w", id, err)
	}
	seen := 0
	match := true
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
		exp, ok := want[rel]
		if !ok || !d.Type().IsRegular() {
			match = false
			return fs.SkipAll
		}
		got, err := os.ReadFile(p) //nolint:gosec // path derived from the walked package root.
		if err != nil {
			return err
		}
		if !bytes.Equal(got, exp) {
			match = false
			return fs.SkipAll
		}
		seen++
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("skillcatalog: read installed %s: %w", id, err)
	}
	return match && seen == len(want), nil
}

// CanonicalFindingsSchemaRef is the package-relative path of the findings
// contract in the builtin security-audit package.
const CanonicalFindingsSchemaRef = "schemas/findings.v1.json"

// CanonicalFindingsSchema returns the findings.v1 JSON Schema as this binary
// embeds it. An agent mode's report is validated against the package's own
// schemaRef, and AO additionally requires that file to BE this one: the
// persistence of findings is written against this shape, so a package with a
// different schema has no report AO knows how to store.
func CanonicalFindingsSchema() []byte {
	b, err := builtinFS.ReadFile(path.Join("packages", "security-audit", CanonicalFindingsSchemaRef))
	if err != nil {
		// The file is embedded at compile time; its absence is a broken build.
		panic(fmt.Sprintf("skillcatalog: embedded findings schema missing: %v", err))
	}
	return b
}
