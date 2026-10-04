package domain_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// BoundLatestUserPrompt is a cross-package contract, not a formatting detail:
// session_manager applies it when it records what it wrote into a session, and
// workflow's fix-delivery recovery re-derives it to recognise, after a restart,
// whether the prompt in that session is the one it sent. A change to any of the
// three rules below breaks that recognition silently, so each is pinned here.
func TestBoundLatestUserPromptIsStableForBothSides(t *testing.T) {
	t.Run("trims, and is idempotent", func(t *testing.T) {
		got := domain.BoundLatestUserPrompt("  fix the thing\n\n")
		if got != "fix the thing" {
			t.Fatalf("BoundLatestUserPrompt = %q, want the trimmed text", got)
		}
		if again := domain.BoundLatestUserPrompt(got); again != got {
			t.Fatalf("not idempotent: %q -> %q; the reader bounds an already-bounded value", got, again)
		}
	})

	t.Run("bounds to LatestUserPromptBytes", func(t *testing.T) {
		got := domain.BoundLatestUserPrompt(strings.Repeat("u", domain.LatestUserPromptBytes+512))
		if len(got) != domain.LatestUserPromptBytes {
			t.Fatalf("bounded length = %d, want %d", len(got), domain.LatestUserPromptBytes)
		}
	})

	t.Run("repairs a multi-byte rune split by the bound", func(t *testing.T) {
		// "é" is two bytes, so a prompt of odd length lands the bound mid-rune.
		got := domain.BoundLatestUserPrompt("x" + strings.Repeat("é", domain.LatestUserPromptBytes))
		if !utf8.ValidString(got) {
			t.Fatal("bounded value is not valid UTF-8: a split rune would make the writer's and reader's bytes differ")
		}
	})
}
