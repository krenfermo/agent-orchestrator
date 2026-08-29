package questions

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Fingerprint computes the dedup key for a captured question:
// sha256(run_id, step_id, normalized_question_text, normalized_choices,
// policy_version_at_capture, workspace_fingerprint_at_capture). No
// timestamp is baked into the value, so the same question reappearing
// across poll cycles (or after a daemon restart, before the state
// transitions again) hashes identically and is deduped by the store's
// unique index / INSERT OR IGNORE.
func Fingerprint(runID domain.WorkflowRunID, stepID, questionText string, choices []domain.QuestionChoice, policyVersion, workspaceFingerprint string) string {
	h := sha256.New()
	parts := []string{
		string(runID),
		stepID,
		normalizeText(questionText),
		normalizeChoices(choices),
		policyVersion,
		workspaceFingerprint,
	}
	h.Write([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h.Sum(nil))
}

func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func normalizeChoices(choices []domain.QuestionChoice) string {
	labels := make([]string, 0, len(choices))
	for _, c := range choices {
		labels = append(labels, normalizeText(c.ID)+":"+normalizeText(c.Label))
	}
	return strings.Join(labels, "\x1e")
}
