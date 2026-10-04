package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// subject_collector.go — metering an execution surface that is not an AO session.
//
// WHY THIS IS A SEPARATE ENTRY POINT AND NOT A WIDENED RecordHook. RecordHook is
// SESSION-shaped in two ways that are actively dangerous for a runtime pane, and
// both were named as hazards before this code existed:
//
//  1. It OVERWRITES the signal's harness with the session's
//     (`signal.Harness = session.Harness`). A Codex reviewer attached to a Claude
//     worker's session would have its rollout registered as a claude_main
//     artifact — the wrong parser, the wrong root, the wrong everything.
//
//  2. On a finalizing event it calls finalizeSession, which finalizes EVERY
//     binding of that session. A reviewer's own `session-end` would therefore
//     cut the WORKER's ingestion short, mid-run.
//
// So a pane meters itself: its harness comes from its own hook, and it finalizes
// only its own subject. It never reads, writes, or ends anything belonging to a
// session.
//
// LIFENESS. A session binding's watchability is decided by the session row. A
// pane has no row, so its liveness is its own binding state: it is watchable
// until its own finalizing event settles it. That is why the storage-side
// watchable/discovery queries were relaxed with `session_id IS NULL OR ...`
// rather than given a second code path.

// SubjectHookSignal is one usage callback from a surface that is not an AO
// session. It carries its own harness because nothing else can supply one.
type SubjectHookSignal struct {
	Subject domain.UsageSubject
	Harness domain.AgentHarness
	Event   string
	// NativeSessionID is the harness's own conversation id, reported by the
	// pane's own hook. It is the whole reason a pane is meterable at all: AO
	// never guesses it from "the most recently modified transcript".
	NativeSessionID string
	ModelID         string
	TranscriptPath  string
}

// RecordSubjectHook binds a runtime pane's provider conversation to its subject
// and registers its transcript for ingestion.
//
// It is best-effort about DISCOVERY and strict about IDENTITY: a signal with no
// native id records nothing rather than binding the pane to a transcript AO
// merely believes is probably its own.
func (c *Collector) RecordSubjectHook(ctx context.Context, signal SubjectHookSignal) error {
	if !signal.Subject.Valid() {
		return fmt.Errorf("usage subject hook needs a valid subject, got %q", signal.Subject.String())
	}
	if signal.Subject.Kind == domain.UsageSubjectSession {
		// A session subject has a session row, an activity state and a launch
		// id to check against. Routing it here would skip all three.
		return fmt.Errorf("usage subject hook refuses a session subject; use RecordHook")
	}
	if !SupportedHarness(signal.Harness) {
		return nil
	}

	nativeID := boundedUsageMetadata(strings.TrimSpace(signal.NativeSessionID))
	path := strings.TrimSpace(signal.TranscriptPath)
	if nativeID == "" {
		nativeID = nativeIDFromTranscript(path)
	}
	if nativeID == "" {
		// Nothing to bind to. Recording a binding keyed on an empty root, or
		// adopting whichever transcript looks recent, is exactly the inference
		// this whole path exists to avoid.
		return nil
	}

	finalizing := finalizingEvent(signal.Event)
	if path == "" {
		discovered, err := c.discoverPath(ctx, signal.Harness, nativeID)
		if err != nil {
			return err
		}
		path = discovered
	}
	artifact, err := c.validateHookArtifact(ctx, signal.Harness, path)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()

	existing, exists, err := c.store.GetUsageBindingBySubject(ctx, signal.Subject, signal.Harness, nativeID)
	if err != nil {
		return err
	}
	state := domain.UsageBindingActive
	if exists {
		state = existing.State
	}
	if finalizing {
		state = domain.UsageBindingFinalizing
	}
	lastErrorCode := ""
	if signal.Harness == domain.HarnessCodex && path == "" {
		lastErrorCode = domain.UsageErrorSourceDiscoveryPending
	}

	binding, err := c.store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject:        signal.Subject,
		Harness:        signal.Harness,
		NativeRootID:   nativeID,
		InitialModelID: boundedUsageMetadata(signal.ModelID),
		State:          state,
		LastErrorCode:  lastErrorCode,
		UpdatedAt:      now,
	})
	if err != nil {
		return err
	}

	if artifact != nil {
		kind := domain.UsageSourceClaudeMain
		if signal.Harness == domain.HarnessCodex {
			kind = domain.UsageSourceCodexRollout
		}
		if _, err := c.registerHookSource(
			ctx, binding, kind, nativeID, "", *artifact, now,
			signal.Event == "session-start" || finalizing,
		); err != nil {
			c.notifySourceInventory(true)
			return err
		}
		if _, err := c.store.UpdateUsageBindingErrorCode(ctx, binding.ID, "", now); err != nil {
			return err
		}
	}

	if finalizing {
		// This subject's bindings ONLY. A pane ending never ends a session's
		// collection — the defect this whole file exists to make impossible.
		if _, err := c.store.FinalizeUsageBindingsForSubject(ctx, signal.Subject, now); err != nil {
			return err
		}
		if err := c.settleFinalizingBinding(ctx, binding.ID, now); err != nil {
			return err
		}
	}
	c.notifySourceInventory(artifact == nil)
	return nil
}

// DirectUsageReport is a provider-reported token vector that arrived in a
// RESPONSE rather than in a transcript.
//
// It exists for the planner, and the planner alone is why the shape is needed:
// AO invokes it as `claude --print --no-session-persistence`, so it writes no
// transcript at all and the only place its spend is ever stated is the JSON
// envelope the adapter already parses. There is nothing to discover, nothing to
// tail, and no cursor — just one fact, reported once.
type DirectUsageReport struct {
	Subject domain.UsageSubject
	Harness domain.AgentHarness
	ModelID string
	Tokens  domain.UsageTokenMetrics
	// EventKey is the caller's OWN exactly-once identity for this report,
	// derived from the durable invocation it describes and never from a clock.
	// Replaying the same invocation re-derives the same key, so the insert is a
	// no-op rather than a second charge.
	EventKey string
	// ObservedAt is when the call happened, used to place it in the right role
	// window. Zero leaves it unstated rather than inventing one.
	ObservedAt time.Time
}

// RecordDirectUsage stores one response-reported usage fact.
//
// Exactly-once comes from the same place it does everywhere else in this
// pipeline: the (binding, source_event_key) uniqueness. A retried recording of
// the same invocation writes nothing.
func (c *Collector) RecordDirectUsage(ctx context.Context, report DirectUsageReport) error {
	if !report.Subject.Valid() {
		return fmt.Errorf("direct usage report needs a valid subject, got %q", report.Subject.String())
	}
	if report.Subject.Kind == domain.UsageSubjectSession {
		return fmt.Errorf("direct usage report refuses a session subject; sessions are metered from their transcript")
	}
	if !SupportedHarness(report.Harness) {
		return nil
	}
	if strings.TrimSpace(report.EventKey) == "" {
		return fmt.Errorf("direct usage report needs a stable event key")
	}
	if report.Tokens.InputTokens == 0 && report.Tokens.OutputTokens == 0 {
		// Nothing reported is UNKNOWN, and unknown is not a zero worth storing:
		// a stored zero would render as "this call was free".
		return nil
	}
	model := boundedUsageMetadata(strings.TrimSpace(report.ModelID))
	if model == "" {
		model = "unknown"
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	binding, err := c.store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject: report.Subject,
		Harness: report.Harness,
		// A response-reported invocation has no native conversation of its own,
		// so the subject id doubles as the native root: it is durable, unique
		// per invocation, and never a timestamp.
		NativeRootID:   report.Subject.ID,
		InitialModelID: model,
		State:          domain.UsageBindingComplete,
		UpdatedAt:      now,
	})
	if err != nil {
		return err
	}
	observed := report.ObservedAt
	if !observed.IsZero() {
		observed = observed.UTC()
	}
	return c.store.RecordDirectUsageEvent(ctx, binding.ID, domain.ModelUsageEvent{
		ModelID:        model,
		Tokens:         report.Tokens,
		SourceEventKey: report.EventKey,
		ObservedAt:     observedOrNil(observed),
	}, now)
}

func observedOrNil(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
