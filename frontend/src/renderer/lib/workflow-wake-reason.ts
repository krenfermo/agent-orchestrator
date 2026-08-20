// Checkpoint 8P-E.3: run.waitReason is set from wake.Reason for ANY pending
// durable wake (see backend master_coordinator.go's getMasterRun /
// workflow.go's GetRun), not only provider-capacity waits. wake.ReasonAutonomousProgress
// in particular is the autonomous heartbeat that keeps GetRun/ContinueRun
// re-entered while a master run is progressing normally — it fires even
// when nothing is actually short on capacity, so it must never render as
// "Waiting for capacity" (a real E2E run reproduced this: Claude health was
// reported available/dispatch-succeeded, yet the UI still said "Waiting for
// capacity" because a routine autonomous_progress wake was pending).
//
// Deliberately a denylist, not an allowlist: every other reason
// wake.Reason defines today IS capacity/rate-limit-shaped, and any reason
// added later should default to rendering as a capacity wait (fail open)
// rather than silently going unlabeled until this list is updated (fail
// closed). autonomous_progress is the one documented exception.
const NON_CAPACITY_WAIT_REASONS = new Set(["autonomous_progress"]);

/** True when `reason` (a run's waitReason) represents an actual capacity/rate-limit wait. */
export function isCapacityWaitReason(reason: string | undefined | null): boolean {
	return Boolean(reason) && !NON_CAPACITY_WAIT_REASONS.has(reason as string);
}
