package skillcatalog

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
