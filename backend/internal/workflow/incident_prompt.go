package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_prompt.go — Checkpoint 8P-E.18.
//
// Two prompts, and the difference between them is the security model made
// visible: the diagnostic prompt describes something to look at and forbids
// touching it; the repair prompt describes something to change and forbids
// deciding whether the change was right.
//
// Neither prompt is the enforcement. The diagnostic agent is launched read-only
// by the launcher, the repair agent is launched in an isolated workspace, and
// the classification is validated on the way back in. The prompts exist so a
// competent agent does the right thing, not so an incompetent one is contained.

// BuildIncidentDiagnosticPrompt renders the diagnostic instruction over a pack.
func BuildIncidentDiagnosticPrompt(pack IncidentContextPack) string {
	var b strings.Builder
	b.WriteString(`You are AO's incident Diagnostic Agent.

An AO workflow run has stopped and is waiting for a person. Your job is to
explain what happened and to classify it. You are NOT fixing anything.

Hard rules:
- Diagnose ONLY from the context pack below. Do not read the repository, do not
  grep, do not open files, do not run git, do not run tests. The pack is
  deliberately bounded; if it does not contain what you need, that is a finding,
  not an obstacle to work around.
- Do not modify anything. You have no write mandate here.
- Do not approve your own conclusion. You propose; a person or AO's policy
  decides. Proposing an action is not permission to take it.

Classify the incident as exactly one of:

  auto_recoverable
      AO already knows how to recover from this and the evidence for it is
      present. The only action available for this class is continue_run.
  repair_ao
      The stop is caused by a defect in AO's own code. The remedy is a source
      change, which means a separate Repair Agent, an independent reviewer and
      deterministic verification. Propose repair_agent.
  human_decision_required
      The evidence is sufficient, and the remaining choice is genuinely a
      person's (accept a trade-off, spend more budget, overrule a reviewer).
      Offer concrete options. Do not pick one.
  unsafe_or_insufficient_evidence
      Acting would be unsafe, or the pack does not support a conclusion. Say
      precisely which evidence is missing. This is a correct answer, not a
      failure — prefer it over a confident guess.

`)
	b.WriteString(pack.Render())
	b.WriteString(`
When you are done, write ONE JSON object and submit it with:

    ao incident diagnose submit --run <runId> --file <path-to-json>

Schema:

{
  "incidentId":   "<echo exactly>",
  "packDigest":   "<echo exactly>",
  "classification": "auto_recoverable | repair_ao | human_decision_required | unsafe_or_insufficient_evidence",
  "summary":      "one sentence a tired operator can act on",
  "whatHappened": "the sequence of events, in AO's own vocabulary",
  "whatIsStuck":  "which run, step and session are frozen, and since when",
  "whyAOStopped": "the rule or guard that produced this stop",
  "evidence":     ["the specific facts from the pack you relied on"],
  "missingEvidence": ["what you would have needed; required when insufficient"],
  "risk":         "what could go wrong if the proposed action is taken",
  "options":      [{"id":"…","label":"…","detail":"…","consequence":"…"}],
  "proposedAction": {"kind":"none | continue_run | repair_agent | cancel_run | await_human","reason":"…","detail":"…"}
}

`)
	fmt.Fprintf(&b, "incidentId: %s\npackDigest: %s\n", pack.IncidentID, pack.Digest)
	return b.String()
}

// BuildIncidentRepairPrompt renders the repair instruction.
//
// It carries the diagnosis rather than the pack: the repairer is acting on a
// conclusion that has already been reached and approved, and re-litigating it
// is not its job. What it must not inherit is any sense that its own opinion of
// the result counts — hence the explicit hand-off to review and verify.
func BuildIncidentRepairPrompt(inc Incident) string {
	var b strings.Builder
	b.WriteString(`You are AO's incident Repair Agent.

A diagnosis has been made and approved by a person. Your job is to implement
the smallest correct fix for the defect described below, in AO's own source.

Hard rules:
- You are NOT the reviewer and you are NOT the verifier. Do not approve your own
  change, do not declare it correct, and do not skip review because you are
  confident. An independent reviewer reads it next and deterministic checks run
  after that.
- Never run a destructive git operation: no reset, no stash, no checkout that
  discards work, no force, no branch deletion, no history rewrite.
- Never modify the database directly.
- Fix the cause named in the diagnosis. If you conclude the diagnosis is wrong,
  stop and say so instead of fixing something else.
- Add a regression test that fails without your change.

`)
	fmt.Fprintf(&b, "Incident: %s\nStop reason: %s\n\n", inc.ID, inc.StopReason)
	if inc.Diagnosis != nil {
		d := inc.Diagnosis
		fmt.Fprintf(&b, "Summary: %s\n\nWhat happened:\n%s\n\nWhat is stuck:\n%s\n\nWhy AO stopped:\n%s\n\n",
			d.Summary, d.WhatHappened, d.WhatIsStuck, d.WhyStopped)
		if len(d.Evidence) > 0 {
			b.WriteString("Evidence the diagnosis relied on:\n")
			for _, e := range d.Evidence {
				b.WriteString("- " + e + "\n")
			}
			b.WriteString("\n")
		}
		if d.Risk != "" {
			fmt.Fprintf(&b, "Risk identified: %s\n\n", d.Risk)
		}
		if d.Action != nil && d.Action.Reason != "" {
			fmt.Fprintf(&b, "Why a repair was proposed: %s\n\n", d.Action.Reason)
		}
	}
	b.WriteString("When you are done, report what you changed and what you tested. AO verifies your work independently from the state of the worktree, not from what you say.\n")
	return b.String()
}

// ---- record codec -----------------------------------------------------------

func marshalIncidentRecord(rec IncidentRecord) (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeIncidentRecord(cp domain.WorkflowCheckpoint) (IncidentRecord, bool) {
	var rec IncidentRecord
	if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.IncidentID == "" {
		return IncidentRecord{}, false
	}
	return rec, true
}
