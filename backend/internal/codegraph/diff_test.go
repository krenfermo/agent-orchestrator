package codegraph

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseGitNameStatus(t *testing.T) {
	out := strings.Join([]string{
		"M\tbackend/internal/codegraph/native.go",
		"A\tbackend/internal/codegraph/store.go",
		"D\tbackend/internal/old/gone.go",
		"R094\tbackend/internal/old/name.go\tbackend/internal/new/name.go",
		"C075\tbackend/internal/a.go\tbackend/internal/b.go",
		"T\tbackend/internal/link.go",
		"",
	}, "\n")

	diff, err := ParseGitNameStatus(out)
	if err != nil {
		t.Fatalf("ParseGitNameStatus: %v", err)
	}
	want := []FileChange{
		{Status: ChangeModified, Path: "backend/internal/codegraph/native.go"},
		{Status: ChangeAdded, Path: "backend/internal/codegraph/store.go"},
		{Status: ChangeDeleted, Path: "backend/internal/old/gone.go"},
		{Status: ChangeRenamed, Path: "backend/internal/new/name.go", OldPath: "backend/internal/old/name.go"},
		{Status: ChangeAdded, Path: "backend/internal/b.go"},
		{Status: ChangeModified, Path: "backend/internal/link.go"},
	}
	if !reflect.DeepEqual(diff.Changes, want) {
		t.Fatalf("changes =\n%+v\nwant\n%+v", diff.Changes, want)
	}
	if diff.IsEmpty() {
		t.Fatal("diff reported empty")
	}
}

func TestParseGitNameStatusRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"noPath":        "M",
		"unknownStatus": "X\tfile.go",
		"renameNoDest":  "R100\tonly-one-path.go",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGitNameStatus(line); err == nil {
				t.Fatalf("ParseGitNameStatus(%q) succeeded, want an error", line)
			}
		})
	}
}

func TestParseGitNameStatusIgnoresBlankLines(t *testing.T) {
	diff, err := ParseGitNameStatus("\n\n")
	if err != nil {
		t.Fatalf("ParseGitNameStatus: %v", err)
	}
	if !diff.IsEmpty() {
		t.Fatalf("blank input produced %+v", diff.Changes)
	}
}

func TestNormalizeRel(t *testing.T) {
	// git name-status always emits forward slashes, so normalizing is about
	// leading "./", a leading separator, and the quoting git applies to paths
	// with spaces.
	cases := map[string]string{
		`./a/b.go`:   "a/b.go",
		`"a/b c.go"`: "a/b c.go",
		`/a/b.go`:    "a/b.go",
		` a/b.go `:   "a/b.go",
	}
	for in, want := range cases {
		if got := normalizeRel(in); got != want {
			t.Fatalf("normalizeRel(%q) = %q, want %q", in, got, want)
		}
	}
}
