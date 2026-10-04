package projectmemory

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// bytesPerTokenEstimate is the divisor behind every token estimate this
// package produces. AO does not run the provider's tokenizer, so a token count
// derived from bytes is an approximation and is always labeled
// BasisEstimated. Four bytes per token is the conventional rule of thumb for
// English-plus-code text; it is deliberately a single named constant so a
// later phase can improve the heuristic in one place and see, from the
// recorded Method string, which baselines used the old one.
const bytesPerTokenEstimate = 4

// EstimateMethod is the Method string stamped on every metric this package
// estimates from a byte count. It names the heuristic so a later phase can see,
// from the record itself, which baselines predate an improved one.
const EstimateMethod = "utf8 bytes / 4 heuristic (no provider tokenizer)"

// EstimateTokensFromBytes converts a measured byte count into an estimated
// token count, rounding up so a non-empty payload never estimates to zero
// tokens. A negative input yields zero.
func EstimateTokensFromBytes(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return (bytes + bytesPerTokenEstimate - 1) / bytesPerTokenEstimate
}

// EstimatedTokensFor is the Metric form of EstimateTokensFromBytes.
func EstimatedTokensFor(bytes int64) Metric {
	return Estimated(EstimateTokensFromBytes(bytes), EstimateMethod)
}

// SourceScan is the measured size of a dispatch's declared source scope: how
// much the agent could have read, against which how much it actually read
// becomes meaningful.
type SourceScan struct {
	// Roots are the scanned paths, relative to the repository when they sit
	// under it and absolute otherwise.
	Roots []string
	// Files is the number of source files found.
	Files int64
	// Bytes is their measured total size.
	Bytes int64
	// Skipped counts entries the scan deliberately did not measure (excluded
	// directories, non-source extensions, unreadable entries). It is reported
	// so a suspiciously small scope is visible as a skip count rather than
	// looking like a small repository.
	Skipped int64
}

// scanExcludedDirs are directory names a source scan never descends into.
// They are dependency, build, and VCS trees: counting them would inflate
// "source available" with content no agent is expected to read.
var scanExcludedDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
	".next":        {},
	"target":       {},
	"__pycache__":  {},
	".venv":        {},
	".idea":        {},
	".ao":          {},
}

// scanSourceExtensions are the file extensions a source scan measures. The
// list is deliberately explicit rather than "everything that is not binary":
// an allowlist that misses a language shows up as a Skipped count, whereas a
// denylist that misses a binary format silently inflates the estimate.
var scanSourceExtensions = map[string]struct{}{
	".go": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".mjs": {}, ".cjs": {},
	".py": {}, ".rb": {}, ".rs": {}, ".java": {}, ".kt": {}, ".swift": {},
	".c": {}, ".h": {}, ".cc": {}, ".cpp": {}, ".hpp": {},
	".sh": {}, ".bash": {}, ".zsh": {},
	".sql": {}, ".proto": {}, ".graphql": {},
	".json": {}, ".yaml": {}, ".yml": {}, ".toml": {}, ".md": {},
	".css": {}, ".scss": {}, ".html": {},
}

// ScanSource walks roots and measures how much source text they contain. Both
// counts it returns are measured facts; the token figure derived from them is
// produced separately (and labeled estimated) by the recorder.
//
// A root that cannot be walked contributes to Skipped rather than failing the
// scan: a baseline that refuses to record anything because one directory was
// unreadable is less useful than one that records what it saw and says how
// much it missed.
func ScanSource(base string, roots []string) SourceScan {
	scan := SourceScan{Roots: make([]string, 0, len(roots))}
	seen := make(map[string]struct{})
	for _, root := range roots {
		abs := root
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(base, root)
		}
		scan.Roots = append(scan.Roots, root)
		walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				scan.Skipped++
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if _, excluded := scanExcludedDirs[d.Name()]; excluded {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				scan.Skipped++
				return nil
			}
			if _, ok := scanSourceExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
				scan.Skipped++
				return nil
			}
			if _, dup := seen[path]; dup {
				return nil
			}
			info, statErr := d.Info()
			if statErr == nil {
				seen[path] = struct{}{}
				scan.Files++
				scan.Bytes += info.Size()
				return nil
			}
			scan.Skipped++
			return nil
		})
		if walkErr != nil {
			scan.Skipped++
		}
	}
	sort.Strings(scan.Roots)
	return scan
}

// MeasureFile reports a file's size without reading it into memory. It is the
// cheap way for an instrumented dispatch to record "this path was inspected,
// and it is this big" for a file the pipeline itself read.
func MeasureFile(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
