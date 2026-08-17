package workflow

import (
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DecisionResolverPromptInput is everything BuildDecisionResolverPrompt needs
// to build the resolver's launch prompt text. Pure values only (no
// domain/ports types beyond simple structs), mirroring ReviewPromptInput's
// shape.
type DecisionResolverPromptInput struct {
	Objective          string
	AcceptanceCriteria []string
	QuestionText       string
	Choices            []domain.QuestionChoice
	Branch             string
	WorktreePath       string
	PolicyVersion      string
	AllowSameProvider  bool
	ResolverSessionID  string
	ResolutionRunID    string
	// ContextPack is Checkpoint 8M's optional rendered SessionContextPack
	// (decision_resolver role) — minimal-evidence facts (fingerprint, any
	// prior decisions/tests already recorded), never a transcript. Empty
	// when the caller could not build one (e.g. no plan artifact yet); the
	// prompt remains fully usable without it.
	ContextPack string
}

// BuildDecisionResolverPrompt deterministically builds the text handed to
// the read-only cross-provider Decision Resolver session (Checkpoint 8K-B,
// pass 2). Pure and deterministic: no IO, no model call.
//
// The context pack is intentionally small: the run objective/acceptance
// criteria, the captured question text/choices, and the branch/worktree the
// asking session was working in — everything already durable in RunDetail
// and the workflow_questions row. It deliberately never includes a
// transcript or the asking session's chain-of-thought (none is ever
// captured by this checkpoint in the first place). Checkpoint 8M adds an
// optional richer ContextPack field (fingerprint, any prior decisions/tests
// already recorded) built from BuildTaskCheckpointSummary — the import-cycle
// blocker this comment used to describe (workflow couldn't reuse
// controllers' summary builder) is resolved by that builder now living in
// this same package (task_checkpoint_summary.go).
//
// Both identifiers below (ResolverSessionID, ResolutionRunID) are baked into
// the prompt text by the daemon, mirroring BuildReviewPrompt's
// WorkerSessionID/ReviewRunID interpolation exactly: the resolver never
// derives its own identity, it only echoes these back as `ao decision
// resolve` CLI args.
func BuildDecisionResolverPrompt(in DecisionResolverPromptInput) string {
	var criteria string
	for _, c := range in.AcceptanceCriteria {
		criteria += "- " + c + "\n"
	}
	if criteria == "" {
		criteria = "- (none recorded)\n"
	}

	var choices string
	for _, c := range in.Choices {
		choices += fmt.Sprintf("- %s: %s\n", c.ID, c.Label)
	}
	if choices == "" {
		choices = "(no structured choices offered — free-form question)\n"
	}

	sameProviderNote := ""
	if in.AllowSameProvider {
		sameProviderNote = "\nNote: this resolver session may be the SAME provider that asked the " +
			"question. Answer only from objective repo evidence, not from any assumption about what " +
			"the asking session intended.\n"
	}
	contextPackBlock := ""
	if in.ContextPack != "" {
		contextPackBlock = "\n" + in.ContextPack + "\n"
	}

	return fmt.Sprintf(`You are a read-only Decision Resolver for one question raised by another AO-managed
worker session (Checkpoint 8K-B). You are a DIFFERENT provider/session than the one that
asked — you have no access to its conversation or reasoning, only the evidence below and
whatever you discover by inspecting the repository yourself.
%s
Objective of the overall run: %s

Acceptance criteria for the run's work: %s
Branch: %s
Worktree path (already your current checkout — do not clone or fetch elsewhere): %s
Policy version at capture: %s
%s
The question you must resolve:
%s

Structured choices offered (if any):
%s

Guardrails (follow all of these):
- Do NOT modify any file in this worktree.
- Do NOT stage, commit, push, merge, rebase, or switch branches.
- Do NOT open, create, or otherwise interact with a pull request.
- Do NOT output your chain-of-thought, reasoning transcript, or any narrative — only the
  final structured result via the command below.
- Only answer from objective, verifiable repo evidence (existing code, files, git history
  visible in this worktree) — never from assumption, preference, or a guess about what the
  asking session "probably" wants.
- If you cannot determine a safe, evidence-backed answer, use --requires-human instead of
  guessing.

When you are done, submit your result with exactly one of the following commands (this is
the ONLY way to record your result — AO reads it back from this resolution run, not from
anything else you output):

  ao decision resolve %s --run %s --answer "<answer>" --reason "<short reason>" \
    --evidence "<reference>" [--evidence "<reference2>" ...] --certainty actual|inferred|unknown

or, if you cannot determine a confident answer from repo evidence:

  ao decision resolve %s --run %s --requires-human --reason "<why you could not determine this>"

Where <reference> is a short pointer to the evidence you used (e.g. a file path, or a file
path plus line range) — never a full file dump or transcript.`,
		sameProviderNote,
		in.Objective, criteria, in.Branch, in.WorktreePath, in.PolicyVersion,
		contextPackBlock,
		strings.TrimSpace(in.QuestionText), choices,
		in.ResolverSessionID, in.ResolutionRunID,
		in.ResolverSessionID, in.ResolutionRunID,
	)
}
