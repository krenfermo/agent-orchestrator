package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workflow_objective_test.go — P2-E B6, the limit's own contract.
//
// The property that matters is not "long text is accepted" but "nothing is
// ever silently changed": a specification either arrives byte-for-byte or is
// refused with both sizes named. Truncation is the failure mode this whole
// change exists to remove, so most of these assert the ABSENCE of it.

func TestObjectiveAcceptsLongMultilineSpecificationsUnchanged(t *testing.T) {
	spec := strings.Join([]string{
		"OBJETIVO",
		"",
		"Añadir un índice de documentación.",
		"",
		"ALCANCE",
		"",
		"- docs/README.md",
		"- No tocar el código de la aplicación",
		"",
		"CRITERIOS DE ACEPTACIÓN",
		"",
		"1. El archivo existe",
		"2. Enlaza los documentos reales",
		"",
		"NO HACER",
		"",
		"No inventar documentos que no existan.",
	}, "\n")

	for _, size := range []int{len(spec), 10 << 10, 50 << 10, domain.MaxWorkflowObjectiveBytes - 1024} {
		body := spec
		for len(body) < size {
			body += "\n\n" + spec
		}
		body = body[:size]
		got, err := domain.ValidateWorkflowObjective(body)
		if err != nil {
			t.Fatalf("%d bytes was refused: %v", size, err)
		}
		if got != strings.TrimSpace(body) {
			t.Fatalf("%d bytes was altered in transit", size)
		}
		if strings.Count(got, "\n") == 0 {
			t.Fatalf("%d bytes lost every newline", size)
		}
	}
}

func TestObjectiveOverTheMaximumIsRefusedNotTruncated(t *testing.T) {
	over := strings.Repeat("a", domain.MaxWorkflowObjectiveBytes+1)
	got, err := domain.ValidateWorkflowObjective(over)
	if !errors.Is(err, domain.ErrObjectiveTooLong) {
		t.Fatalf("err = %v, want ErrObjectiveTooLong", err)
	}
	if got != "" {
		t.Fatalf("a refused objective still returned %d bytes -- truncation", len(got))
	}
	msg := domain.ObjectiveTooLongMessage(over)
	for _, want := range []string{"supera el máximo permitido", "131072"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal does not name %q: %s", want, msg)
		}
	}
}

// TestObjectiveLimitIsBytesNotCharacters is the multibyte case B6 asks for.
//
// A limit counted in runes would let a Spanish or Japanese specification carry
// two to three times the bytes an English one does, past exactly the resources
// the limit protects. The boundary is therefore asserted in bytes on text that
// is not ASCII.
func TestObjectiveLimitIsBytesNotCharacters(t *testing.T) {
	// Three bytes per rune, so rune count is a third of byte count.
	unit := "日"
	runes := domain.MaxWorkflowObjectiveBytes / len(unit)

	atLimit := strings.Repeat(unit, runes)
	if len(atLimit) > domain.MaxWorkflowObjectiveBytes {
		atLimit = strings.Repeat(unit, runes-1)
	}
	if _, err := domain.ValidateWorkflowObjective(atLimit); err != nil {
		t.Fatalf("%d bytes (%d runes) was refused: %v", len(atLimit), len([]rune(atLimit)), err)
	}

	over := atLimit + strings.Repeat(unit, 16)
	if _, err := domain.ValidateWorkflowObjective(over); !errors.Is(err, domain.ErrObjectiveTooLong) {
		t.Fatalf("%d bytes (%d runes) was accepted; the limit is counting runes",
			len(over), len([]rune(over)))
	}
}

func TestObjectivePreservesMarkdownAndInteriorWhitespace(t *testing.T) {
	spec := "# Title\n\n```go\nfunc main() {}\n```\n\n- a\n- b\n\n\tindented\n"
	got, err := domain.ValidateWorkflowObjective("   " + spec + "   ")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimSpace(spec) {
		t.Fatalf("markdown was altered:\n%q\nwant\n%q", got, strings.TrimSpace(spec))
	}
	if !strings.Contains(got, "```go") || !strings.Contains(got, "\n\tindented") {
		t.Fatal("a fenced block or an indented line did not survive")
	}
}

func TestEmptyObjectiveIsItsOwnAnswer(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t\n"} {
		if _, err := domain.ValidateWorkflowObjective(in); !errors.Is(err, domain.ErrObjectiveEmpty) {
			t.Fatalf("%q gave %v, want ErrObjectiveEmpty", in, err)
		}
	}
}
