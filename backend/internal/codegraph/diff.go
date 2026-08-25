package codegraph

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ChangeStatus is how a git diff describes one path.
type ChangeStatus string

// The change statuses an incremental update understands. Git's other statuses
// (copied, type-changed) are normalized onto these by ParseGitNameStatus.
const (
	// ChangeAdded is a new file.
	ChangeAdded ChangeStatus = "added"
	// ChangeModified is an edited file.
	ChangeModified ChangeStatus = "modified"
	// ChangeDeleted is a removed file.
	ChangeDeleted ChangeStatus = "deleted"
	// ChangeRenamed is a moved file; OldPath holds where it came from.
	ChangeRenamed ChangeStatus = "renamed"
)

// FileChange is one path-level change from a diff.
type FileChange struct {
	// Status is what happened to the path.
	Status ChangeStatus `json:"status"`
	// Path is the project-relative, slash-separated path after the change.
	// For a deletion it is the path that disappeared.
	Path string `json:"path"`
	// OldPath is the pre-rename path; it is set only for ChangeRenamed.
	OldPath string `json:"oldPath,omitempty"`
}

// Diff is the set of file changes an incremental update applies. It carries
// paths only: a provider re-reads and re-hashes the working tree rather than
// trusting hunk text, so a diff that is stale in its contents but right about
// which paths moved still produces a correct graph.
type Diff struct {
	// Changes are the file-level changes, in any order.
	Changes []FileChange `json:"changes"`
}

// IsEmpty reports whether the diff asks for no work.
func (d Diff) IsEmpty() bool { return len(d.Changes) == 0 }

// ParseGitNameStatus builds a Diff from the output of
// `git diff --name-status -M -z`-style porcelain in its plain (newline and
// tab separated) form, i.e. `git diff --name-status -M <base>..<head>`.
//
// Each line is a status letter followed by one path, or — for renames and
// copies — a similarity-scored status followed by the old and the new path.
// Unknown status letters are an error rather than a silent skip: quietly
// dropping a change would leave a stale entry in the graph that looks
// authoritative.
func ParseGitNameStatus(out string) (Diff, error) {
	var diff Diff
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimRight(strings.TrimSpace(rawLine), "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			return Diff{}, fmt.Errorf("codegraph: malformed git name-status line %q", rawLine)
		}
		code := strings.TrimSpace(fields[0])
		if code == "" {
			return Diff{}, fmt.Errorf("codegraph: malformed git name-status line %q", rawLine)
		}
		change, err := changeFromStatus(code, fields[1:])
		if err != nil {
			return Diff{}, err
		}
		diff.Changes = append(diff.Changes, change)
	}
	return diff, nil
}

func changeFromStatus(code string, paths []string) (FileChange, error) {
	switch code[0] {
	case 'A':
		return FileChange{Status: ChangeAdded, Path: normalizeRel(paths[0])}, nil
	case 'M', 'T':
		return FileChange{Status: ChangeModified, Path: normalizeRel(paths[0])}, nil
	case 'D':
		return FileChange{Status: ChangeDeleted, Path: normalizeRel(paths[0])}, nil
	case 'R':
		if len(paths) < 2 {
			return FileChange{}, fmt.Errorf("codegraph: rename status %q needs an old and a new path", code)
		}
		return FileChange{
			Status:  ChangeRenamed,
			Path:    normalizeRel(paths[1]),
			OldPath: normalizeRel(paths[0]),
		}, nil
	case 'C':
		// A copy leaves the source in place, so only the new path is new.
		if len(paths) < 2 {
			return FileChange{}, fmt.Errorf("codegraph: copy status %q needs a source and a destination path", code)
		}
		return FileChange{Status: ChangeAdded, Path: normalizeRel(paths[1])}, nil
	default:
		return FileChange{}, fmt.Errorf("codegraph: unsupported git status %q", code)
	}
}

// normalizeRel puts a path into the one spelling the graph keys by:
// slash-separated, no leading "./", no surrounding quotes.
func normalizeRel(p string) string {
	trimmed := strings.TrimSpace(p)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		trimmed = trimmed[1 : len(trimmed)-1]
	}
	trimmed = filepath.ToSlash(trimmed)
	trimmed = strings.TrimPrefix(trimmed, "./")
	return strings.TrimPrefix(trimmed, "/")
}
