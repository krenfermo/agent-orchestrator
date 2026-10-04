package backup

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerownership"
)

// futureRuntime answers for one live runtime incarnation, stamped by a launch
// that happened AFTER the backup was taken.
type futureRuntime struct {
	instanceID, owner, installation string
}

func (r futureRuntime) SessionFacts(_ context.Context, h ports.RuntimeHandle) (ports.SessionFacts, bool, error) {
	if h.InstanceID != "" && h.InstanceID != r.instanceID {
		return ports.SessionFacts{}, false, nil
	}
	return ports.SessionFacts{InstanceID: r.instanceID, Owner: r.owner, OwnerKnown: true,
		WorkloadAlive: true, WorkloadKnown: true, Installation: r.installation, InstallationKnown: true}, true, nil
}

func (futureRuntime) DestroyInstance(context.Context, string) error { return nil }

// §20/§80/§81 — an old backup restored while a runtime created after it is
// still alive must never let AO adopt that runtime. The restore brings back
// the installation identity, so the installation stamp alone proves nothing;
// what fences it is P9's per-launch ownership token, which the restored
// database does not contain.
func TestRestoredStateNeverOwnsARuntimeLaunchedAfterTheBackup(t *testing.T) {
	f := newFixture(t)
	installation := f.backupA.Manifest.Source.InstallationID
	session := domain.SessionID("sess-1")

	// What state A recorded for the session: launch L1.
	recA := domain.SessionRecord{ID: session}
	recA.Metadata.RuntimeHandleID = "ao-sess-1"
	recA.Metadata.RuntimeInstanceID = "$1"
	recA.Metadata.RuntimeLaunchID = "launch-L1"
	recA.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(session, "launch-L1")

	// After the backup (state B), AO relaunched the session as L2, same name,
	// same installation. That runtime is still alive when A is restored.
	live := futureRuntime{instanceID: "$7", owner: domain.SessionRuntimeOwnerToken(session, "launch-L2"), installation: installation}

	if _, err := f.restore(t, nil); err != nil {
		t.Fatal(err)
	}
	if ids := liveProjectIDs(t, f.dataDir); slices.Contains(ids, "state-b") {
		t.Fatal("state B survived the restore")
	}
	restoredID, err := readIdentity(f.dataDir)
	if err != nil || restoredID != installation {
		t.Fatalf("identity after restore %q (%v)", restoredID, err)
	}

	obs := workerownership.Classify(context.Background(), recA, live, restoredID)
	if obs.Proof == domain.WorkerRuntimeOwned || obs.Proof == domain.WorkerRuntimeOwnedExited {
		t.Fatalf("the restored state claims a runtime launched after the backup: %+v", obs)
	}

	// Even if the future runtime reuses the recorded incarnation id, its owner
	// token is L2's, not L1's.
	reused := futureRuntime{instanceID: "$1", owner: live.owner, installation: installation}
	obs = workerownership.Classify(context.Background(), recA, reused, restoredID)
	if obs.Proof != domain.WorkerRuntimeOwnerMismatch {
		t.Fatalf("a reused incarnation with a future launch's token: %+v", obs)
	}
}

// Independent review §14 — both explicit identity modes against P9's
// installation stamp. --identity=destination keeps D: a runtime stamped by the
// backup's installation A is another installation's. --identity=backup adopts
// A: a runtime this data dir launched as D before the restore is now another
// installation's. Neither is ever owned -- not even with the recorded owner
// token and incarnation.
func TestExplicitIdentityModesNeverOwnTheOtherInstallationsRuntimes(t *testing.T) {
	const other = "aoi-d0d0d0d0-d0d0-4d0d-8d0d-d0d0d0d0d0d0"
	session := domain.SessionID("sess-1")
	rec := domain.SessionRecord{ID: session}
	rec.Metadata.RuntimeHandleID = "ao-sess-1"
	rec.Metadata.RuntimeInstanceID = "$1"
	rec.Metadata.RuntimeLaunchID = "launch-L1"
	rec.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(session, "launch-L1")

	for _, tc := range []struct {
		policy                 IdentityPolicy
		wantOnDisk, runtimesOf func(backupID string) string
	}{
		{IdentityKeepDestination, func(string) string { return other }, func(b string) string { return b }},
		{IdentityFromBackup, func(b string) string { return b }, func(string) string { return other }},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			f := newFixture(t)
			backupID := f.backupA.Manifest.Source.InstallationID
			writeFile(t, filepath.Join(f.dataDir, IdentityAsset), other+"\n", 0o600)
			if _, err := f.restore(t, func(o *RestoreOptions) { o.Identity = tc.policy }); err != nil {
				t.Fatal(err)
			}
			onDisk, err := readIdentity(f.dataDir)
			if err != nil || onDisk != tc.wantOnDisk(backupID) {
				t.Fatalf("identity after restore %q (%v)", onDisk, err)
			}
			runtime := futureRuntime{instanceID: "$1", owner: rec.Metadata.RuntimeOwnerToken, installation: tc.runtimesOf(backupID)}
			if obs := workerownership.Classify(context.Background(), rec, runtime, onDisk); obs.Proof != domain.WorkerRuntimeInstallationMismatch {
				t.Fatalf("a runtime of the other installation classified as %+v", obs)
			}
		})
	}
}
