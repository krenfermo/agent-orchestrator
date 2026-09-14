// Package workerownership reads a worker session's runtime identity back and
// classifies it against what the session row recorded (P9).
//
// It is deliberately a small pure-ish package between domain and the runtime
// port: the session manager wraps it for the daemon, the workflow engine
// consumes its answer through a narrow port, and a real-tmux test can exercise
// it without a daemon. It never destroys, never launches, and never writes.
package workerownership

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Classify proves (or fails to prove) that the runtime answering for rec is the
// one AO launched for it.
//
// reader nil means the runtime cannot read identity back (conpty): the answer is
// WorkerRuntimeUnsupported, which every recovery path treats as fail-closed.
// installationID empty disables the installation check (a daemon that could not
// resolve its own identity must not invent mismatches).
//
// The order of the checks is the proof:
//
//  1. the row must carry complete provenance — owner token, launch id,
//     incarnation, and a token that is exactly SessionRuntimeOwnerToken(id,
//     launch). Anything less is provenance_missing, decided before the runtime
//     is even asked, because nothing it could answer would be comparable;
//  2. the recorded incarnation is read FIRST, by `$N`, never by name;
//  3. only if that incarnation is gone is the NAME consulted, and only to tell
//     "nothing is there" (absent) from "a different incarnation holds the
//     name" (instance_mismatch) — a name never proves ownership;
//  4. owner token equality, then installation stamp equality;
//  5. workload liveness last: it decides owned vs owned_exited, never ownership.
func Classify(
	ctx context.Context,
	rec domain.SessionRecord,
	reader ports.SessionFactsReader,
	installationID string,
) domain.WorkerRuntimeObservation {
	md := rec.Metadata
	obs := domain.WorkerRuntimeObservation{
		SessionID:         rec.ID,
		RuntimeHandleID:   md.RuntimeHandleID,
		RuntimeInstanceID: md.RuntimeInstanceID,
		RuntimeLaunchID:   md.RuntimeLaunchID,
	}
	if reader == nil {
		obs.Proof = domain.WorkerRuntimeUnsupported
		obs.Detail = "this runtime cannot read session identity back, so ownership cannot be proven"
		return obs
	}
	switch {
	case strings.TrimSpace(md.RuntimeHandleID) == "",
		strings.TrimSpace(md.RuntimeInstanceID) == "",
		strings.TrimSpace(md.RuntimeLaunchID) == "",
		strings.TrimSpace(md.RuntimeOwnerToken) == "":
		obs.Proof = domain.WorkerRuntimeProvenanceMissing
		obs.Detail = fmt.Sprintf("session %s has no complete runtime provenance recorded (%s)", rec.ID, missingFields(md))
		return obs
	case md.RuntimeOwnerToken != domain.SessionRuntimeOwnerToken(rec.ID, md.RuntimeLaunchID):
		// The row contradicts itself: its token names another session or launch.
		obs.Proof = domain.WorkerRuntimeOwnerMismatch
		obs.Detail = fmt.Sprintf("session %s records an ownership token that does not belong to its own launch", rec.ID)
		return obs
	}

	facts, exists, err := reader.SessionFacts(ctx, ports.RuntimeHandle{ID: md.RuntimeHandleID, InstanceID: md.RuntimeInstanceID})
	if errors.Is(err, ports.ErrRuntimeUnavailable) {
		// The runtime SERVER for this installation is not running at all ("no
		// server running" / "error connecting" on AO's own, data-dir-scoped tmux
		// socket): no session of this installation can exist. This is the same
		// conclusion session_manager's boot reconciliation already draws after a
		// machine reboot, and it is what lets a launch interrupted by a reboot
		// take the bounded retry instead of parking as unprovable.
		obs.Proof = domain.WorkerRuntimeAbsent
		obs.Detail = "no runtime server is running for this installation, so no session of it can exist"
		return obs
	}
	if err != nil {
		obs.Proof = domain.WorkerRuntimeUnavailable
		obs.Detail = fmt.Sprintf("reading runtime %s failed: %s", md.RuntimeInstanceID, boundedError(err))
		return obs
	}
	if !exists {
		// The recorded incarnation is gone. Who, if anyone, holds the name?
		byName, nameExists, nerr := reader.SessionFacts(ctx, ports.RuntimeHandle{ID: md.RuntimeHandleID})
		switch {
		case errors.Is(nerr, ports.ErrRuntimeUnavailable):
			obs.Proof = domain.WorkerRuntimeAbsent
			obs.Detail = "no runtime server is running for this installation, so no session of it can exist"
		case nerr != nil:
			obs.Proof = domain.WorkerRuntimeUnavailable
			obs.Detail = fmt.Sprintf("incarnation %s is gone and its name could not be resolved: %s",
				md.RuntimeInstanceID, boundedError(nerr))
		case !nameExists:
			obs.Proof = domain.WorkerRuntimeAbsent
			obs.Detail = fmt.Sprintf("runtime incarnation %s no longer exists and nothing holds its name", md.RuntimeInstanceID)
		default:
			obs.Proof = domain.WorkerRuntimeInstanceMismatch
			obs.ObservedInstanceID = byName.InstanceID
			obs.Detail = fmt.Sprintf("runtime incarnation %s is gone; its name is now held by incarnation %s",
				md.RuntimeInstanceID, byName.InstanceID)
		}
		return obs
	}
	obs.ObservedInstanceID = facts.InstanceID
	obs.WorkloadKnown, obs.WorkloadAlive = facts.WorkloadKnown, facts.WorkloadAlive
	if facts.InstallationKnown {
		obs.ObservedInstallationID = facts.Installation
	}
	switch {
	case facts.InstanceID != md.RuntimeInstanceID:
		obs.Proof = domain.WorkerRuntimeInstanceMismatch
		obs.Detail = fmt.Sprintf("the runtime answered as incarnation %s, recorded %s", facts.InstanceID, md.RuntimeInstanceID)
	case !facts.OwnerKnown:
		obs.Proof = domain.WorkerRuntimeProvenanceMissing
		obs.Detail = fmt.Sprintf("runtime incarnation %s carries no ownership token", facts.InstanceID)
	case facts.Owner != md.RuntimeOwnerToken:
		obs.Proof = domain.WorkerRuntimeOwnerMismatch
		obs.Detail = fmt.Sprintf("runtime incarnation %s carries an ownership token that is not this launch's", facts.InstanceID)
	case installationID != "" && facts.InstallationKnown && facts.Installation != installationID:
		obs.Proof = domain.WorkerRuntimeInstallationMismatch
		obs.Detail = fmt.Sprintf("runtime incarnation %s was created by another AO installation", facts.InstanceID)
	case facts.WorkloadKnown && !facts.WorkloadAlive:
		obs.Proof = domain.WorkerRuntimeOwnedExited
		obs.Detail = fmt.Sprintf("runtime incarnation %s is this launch's and its workload has exited", facts.InstanceID)
	default:
		obs.Proof = domain.WorkerRuntimeOwned
		obs.Detail = fmt.Sprintf("runtime incarnation %s is this launch's", facts.InstanceID)
	}
	return obs
}

func missingFields(md domain.SessionMetadata) string {
	var missing []string
	if strings.TrimSpace(md.RuntimeHandleID) == "" {
		missing = append(missing, "handle")
	}
	if strings.TrimSpace(md.RuntimeInstanceID) == "" {
		missing = append(missing, "incarnation")
	}
	if strings.TrimSpace(md.RuntimeLaunchID) == "" {
		missing = append(missing, "launch")
	}
	if strings.TrimSpace(md.RuntimeOwnerToken) == "" {
		missing = append(missing, "owner token")
	}
	return strings.Join(missing, ", ")
}

// maxErrorDetail bounds what an error contributes to a durable sentence. Runtime
// errors are tmux/process messages, never prompts, but a bound keeps a pathological
// one from landing whole in a checkpoint.
const maxErrorDetail = 240

func boundedError(err error) string {
	msg := err.Error()
	if len(msg) > maxErrorDetail {
		msg = msg[:maxErrorDetail] + "…"
	}
	return msg
}
