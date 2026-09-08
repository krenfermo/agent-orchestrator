package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestWriteSpawnAttachments(t *testing.T) {
	dir := t.TempDir()
	refs, err := writeSpawnAttachments(dir, []ports.SpawnAttachment{
		{Ext: ".html", Data: []byte("first")},
		{Ext: ".png", Data: []byte("second")},
		{Ext: "", Data: []byte("third")},
	})
	if err != nil {
		t.Fatalf("writeSpawnAttachments: %v", err)
	}

	want := []string{".ao/attachments/attachment-1.html", ".ao/attachments/attachment-2.png", ".ao/attachments/attachment-3.bin"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for i, ref := range refs {
		if ref != want[i] {
			t.Errorf("ref[%d] = %q, want %q", i, ref, want[i])
		}
		got, readErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref)))
		if readErr != nil {
			t.Fatalf("read %s: %v", ref, readErr)
		}
		if len(got) == 0 {
			t.Errorf("attachment %s is empty on disk", ref)
		}
	}
}

func TestStageAttachmentsUsesNeutralFileNames(t *testing.T) {
	dir := t.TempDir()
	st := newFakeStore()
	st.sessions["ao-1"] = domain.SessionRecord{
		ID:       "ao-1",
		Metadata: domain.SessionMetadata{WorkspacePath: dir},
	}
	m := New(Deps{Store: st, Workspace: &fakeWorkspace{}})

	refs, err := m.StageAttachments(context.Background(), "ao-1", []ports.SpawnAttachment{
		{Ext: ".html", Data: []byte("<main>hi</main>")},
	})
	if err != nil {
		t.Fatalf("StageAttachments: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want one", refs)
	}
	if !strings.HasPrefix(refs[0], ".ao/attachments/attachment-") || !strings.HasSuffix(refs[0], ".html") {
		t.Fatalf("ref = %q, want neutral attachment name with .html extension", refs[0])
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(refs[0]))); err != nil {
		t.Fatalf("staged attachment missing on disk: %v", err)
	}
}

func TestAppendAttachmentReferences(t *testing.T) {
	t.Run("appends after a brief", func(t *testing.T) {
		got := appendAttachmentReferences("Fix the button", []string{".ao/attachments/attachment-1.html"})
		if !strings.HasPrefix(got, "Fix the button\n\n") {
			t.Errorf("brief not preserved: %q", got)
		}
		if !strings.Contains(got, "- .ao/attachments/attachment-1.html") {
			t.Errorf("missing reference: %q", got)
		}
	})

	t.Run("handles empty brief", func(t *testing.T) {
		got := appendAttachmentReferences("", []string{".ao/attachments/attachment-1.html"})
		if strings.HasPrefix(got, "\n") {
			t.Errorf("leading blank line for empty brief: %q", got)
		}
		if !strings.Contains(got, "Attached files") {
			t.Errorf("missing header: %q", got)
		}
		if strings.Contains(got, "Attached images") {
			t.Errorf("header still describes attachments as images: %q", got)
		}
	})

	t.Run("no refs returns prompt unchanged", func(t *testing.T) {
		if got := appendAttachmentReferences("brief", nil); got != "brief" {
			t.Errorf("got %q, want %q", got, "brief")
		}
	})
}

// TestWriteSpawnAttachmentsPersistsALongSpecificationSafely covers the escape
// hatch for a specification past even the raised brief ceiling: it arrives as
// a Markdown file, and what lands on disk has to be the durable, private,
// byte-identical article — inside the workspace and nowhere else.
func TestWriteSpawnAttachmentsPersistsALongSpecificationSafely(t *testing.T) {
	dir := t.TempDir()
	spec := "# Especificación RBAC\n\n" + strings.Repeat("- criterio de aceptación ñ\n", 8000)

	refs, err := writeSpawnAttachments(dir, []ports.SpawnAttachment{{Ext: ".md", Data: []byte(spec)}})
	if err != nil {
		t.Fatalf("writeSpawnAttachments: %v", err)
	}
	if len(refs) != 1 || refs[0] != ".ao/attachments/attachment-1.md" {
		t.Fatalf("refs = %v, want the markdown specification under .ao/attachments", refs)
	}

	// The path is derived by AO, never by the caller: the reference stays
	// inside the workspace no matter what a client sent.
	full := filepath.Join(dir, filepath.FromSlash(refs[0]))
	rel, err := filepath.Rel(dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("attachment escaped the workspace: %s (rel %q, err %v)", full, rel, err)
	}

	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	if string(got) != spec {
		t.Fatalf("the specification was altered on disk: %d bytes in, %d bytes out", len(spec), len(got))
	}

	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat attachment: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("attachment mode = %o, want 600: a task specification is not world-readable", perm)
	}
	dirInfo, err := os.Stat(filepath.Join(dir, filepath.FromSlash(attachmentsDir)))
	if err != nil {
		t.Fatalf("stat attachments dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o750 {
		t.Fatalf("attachments dir mode = %o, want 750", perm)
	}

	// And the agent is told to read it: a durable file nobody is pointed at
	// is the same as no specification at all.
	prompt := appendAttachmentReferences("La especificación completa está adjunta.", refs)
	if !strings.Contains(prompt, "read these files") || !strings.Contains(prompt, refs[0]) {
		t.Fatalf("the prompt does not tell the agent to read the specification:\n%s", prompt)
	}
	if !strings.HasPrefix(prompt, "La especificación completa está adjunta.") {
		t.Fatalf("the human's brief was not preserved ahead of the references:\n%s", prompt)
	}
}
