package workerownership

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// scriptedReader answers SessionFacts from two tables: one keyed by exact
// incarnation, one by name (a handle with no instance). It records every call so
// a test can prove the NAME was consulted only after the incarnation was gone.
type scriptedReader struct {
	byInstance map[string]ports.SessionFacts
	byName     map[string]ports.SessionFacts
	errInst    error
	errName    error
	calls      []ports.RuntimeHandle
}

func (r *scriptedReader) SessionFacts(_ context.Context, h ports.RuntimeHandle) (ports.SessionFacts, bool, error) {
	r.calls = append(r.calls, h)
	if h.InstanceID != "" {
		if r.errInst != nil {
			return ports.SessionFacts{}, true, r.errInst
		}
		f, ok := r.byInstance[h.InstanceID]
		return f, ok, nil
	}
	if r.errName != nil {
		return ports.SessionFacts{}, false, r.errName
	}
	f, ok := r.byName[h.ID]
	return f, ok, nil
}

func (r *scriptedReader) DestroyInstance(context.Context, string) error {
	panic("Classify must never destroy anything")
}

const installation = "aoi-00000000-0000-4000-8000-000000000001"

func provenRecord() domain.SessionRecord {
	id := domain.SessionID("sess-7")
	return domain.SessionRecord{
		ID: id,
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   "sess-7",
			RuntimeInstanceID: "$4",
			RuntimeLaunchID:   "launch-B",
			RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(id, "launch-B"),
			WorkspacePath:     "/wt/sess-7",
			Branch:            "ao/sess-7",
		},
	}
}

func ownedFacts(rec domain.SessionRecord) ports.SessionFacts {
	return ports.SessionFacts{
		InstanceID: rec.Metadata.RuntimeInstanceID,
		Owner:      rec.Metadata.RuntimeOwnerToken, OwnerKnown: true,
		WorkloadAlive: true, WorkloadKnown: true,
		Installation: installation, InstallationKnown: true,
	}
}

func TestClassifyTable(t *testing.T) {
	rec := provenRecord()
	legacy := rec
	legacy.Metadata.RuntimeOwnerToken = ""
	legacy.Metadata.RuntimeLaunchID = ""
	selfContradicting := rec
	selfContradicting.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(rec.ID, "launch-A")

	exited := ownedFacts(rec)
	exited.WorkloadAlive = false
	unknownWorkload := ownedFacts(rec)
	unknownWorkload.WorkloadKnown, unknownWorkload.WorkloadAlive = false, false
	staleOwner := ownedFacts(rec)
	staleOwner.Owner = domain.SessionRuntimeOwnerToken(rec.ID, "launch-A")
	noOwner := ownedFacts(rec)
	noOwner.Owner, noOwner.OwnerKnown = "", false
	foreignInstall := ownedFacts(rec)
	foreignInstall.Installation = "aoi-00000000-0000-4000-8000-000000000002"
	unstamped := ownedFacts(rec)
	unstamped.Installation, unstamped.InstallationKnown = "", false
	wrongIncarnation := ownedFacts(rec)
	wrongIncarnation.InstanceID = "$9"

	cases := []struct {
		name   string
		rec    domain.SessionRecord
		reader *scriptedReader
		nilRdr bool
		want   domain.WorkerRuntimeProof
	}{
		{name: "owned", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": ownedFacts(rec)}}, want: domain.WorkerRuntimeOwned},
		{name: "owned exited", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": exited}}, want: domain.WorkerRuntimeOwnedExited},
		{name: "owned, workload unreadable stays owned", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": unknownWorkload}}, want: domain.WorkerRuntimeOwned},
		{name: "unstamped runtime is not an installation mismatch", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": unstamped}}, want: domain.WorkerRuntimeOwned},
		{name: "absent: incarnation gone, name free", rec: rec, reader: &scriptedReader{}, want: domain.WorkerRuntimeAbsent},
		{name: "I5: name reused by another incarnation", rec: rec, reader: &scriptedReader{byName: map[string]ports.SessionFacts{"sess-7": ownedFacts(rec)}}, want: domain.WorkerRuntimeInstanceMismatch},
		{name: "defensive: runtime answers as another incarnation", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": wrongIncarnation}}, want: domain.WorkerRuntimeInstanceMismatch},
		{name: "I2/I14: token of an earlier launch", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": staleOwner}}, want: domain.WorkerRuntimeOwnerMismatch},
		{name: "runtime carries no token", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": noOwner}}, want: domain.WorkerRuntimeProvenanceMissing},
		{name: "I6: another installation", rec: rec, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": foreignInstall}}, want: domain.WorkerRuntimeInstallationMismatch},
		{name: "I17: legacy row", rec: legacy, reader: &scriptedReader{byInstance: map[string]ports.SessionFacts{"$4": ownedFacts(rec)}}, want: domain.WorkerRuntimeProvenanceMissing},
		{name: "row contradicts itself", rec: selfContradicting, reader: &scriptedReader{}, want: domain.WorkerRuntimeOwnerMismatch},
		{name: "unsupported runtime (conpty)", rec: rec, nilRdr: true, want: domain.WorkerRuntimeUnsupported},
		{name: "probe failure concludes nothing", rec: rec, reader: &scriptedReader{errInst: errors.New("tmux: exit 1")}, want: domain.WorkerRuntimeUnavailable},
		{name: "name probe failure concludes nothing", rec: rec, reader: &scriptedReader{errName: ports.ErrRuntimeUnavailable}, want: domain.WorkerRuntimeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reader ports.SessionFactsReader
			if !tc.nilRdr {
				reader = tc.reader
			}
			got := Classify(context.Background(), tc.rec, reader, installation)
			if got.Proof != tc.want {
				t.Fatalf("proof = %q (%s), want %q", got.Proof, got.Detail, tc.want)
			}
			if !got.Proof.Valid() {
				t.Fatalf("proof %q outside the closed vocabulary", got.Proof)
			}
		})
	}
}

// The name is a DISCOVERY aid, never evidence: it is consulted only after the
// recorded incarnation answered "gone", and never at all when the incarnation is
// there — so an owned verdict cannot depend on whatever holds the name.
func TestClassifyNeverConsultsTheNameWhileTheIncarnationAnswers(t *testing.T) {
	rec := provenRecord()
	r := &scriptedReader{
		byInstance: map[string]ports.SessionFacts{"$4": ownedFacts(rec)},
		byName:     map[string]ports.SessionFacts{"sess-7": {InstanceID: "$9"}},
	}
	if got := Classify(context.Background(), rec, r, installation); got.Proof != domain.WorkerRuntimeOwned {
		t.Fatalf("proof = %q", got.Proof)
	}
	for _, h := range r.calls {
		if h.InstanceID == "" {
			t.Fatalf("the name was resolved while the incarnation was answering: %+v", r.calls)
		}
	}
}

// A legacy row is decided before the runtime is ever asked: there is nothing the
// runtime could say that would be comparable.
func TestClassifyLegacyRowNeverProbesTheRuntime(t *testing.T) {
	rec := provenRecord()
	rec.Metadata.RuntimeInstanceID = ""
	r := &scriptedReader{}
	if got := Classify(context.Background(), rec, r, installation); got.Proof != domain.WorkerRuntimeProvenanceMissing {
		t.Fatalf("proof = %q", got.Proof)
	}
	if len(r.calls) != 0 {
		t.Fatalf("a row with no provenance still probed the runtime: %+v", r.calls)
	}
}

// Privacy (§30): the observation carries identities only. A row whose prompt and
// metadata hold secrets must not leak any of them into the durable detail.
func TestClassifyDetailCarriesNoPromptOrToken(t *testing.T) {
	rec := provenRecord()
	rec.Metadata.Prompt = "SECRET-PROMPT sk-live-abcdef"
	rec.Metadata.LatestUserPrompt = "export AWS_SECRET_ACCESS_KEY=XYZ"
	for _, r := range []*scriptedReader{
		{byInstance: map[string]ports.SessionFacts{"$4": ownedFacts(rec)}},
		{byInstance: map[string]ports.SessionFacts{"$4": {InstanceID: "$4", Owner: "ao-session:other:SECRET-TOKEN", OwnerKnown: true}}},
		{errInst: errors.New("tmux exit")},
	} {
		got := Classify(context.Background(), rec, r, installation)
		for _, needle := range []string{"SECRET", "sk-live", "AWS_SECRET", rec.Metadata.RuntimeOwnerToken} {
			if strings.Contains(got.Detail, needle) {
				t.Fatalf("detail %q leaked %q", got.Detail, needle)
			}
		}
	}
}
