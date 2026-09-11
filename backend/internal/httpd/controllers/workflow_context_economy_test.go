package controllers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
)

// contextEconomyWorkflowService records what the controller froze after
// creation AND writes it back into the run's policy snapshot, so these tests
// prove the whole wire round-trip: request field -> service call -> snapshot ->
// response view. What compaction then DOES with the flag is the coordinator's
// job and is covered in internal/workflow.
type contextEconomyWorkflowService struct {
	strategyWorkflowService

	compactionCalls []bool
	requestedBy     []string
	tokenCalls      []int64

	compactionErr error
	tokenErr      error
}

var _ workflowsvc.ContextEconomyManager = (*contextEconomyWorkflowService)(nil)

func (f *contextEconomyWorkflowService) ApplySessionCompactionPolicy(_ context.Context, _ string, enabled bool, requestedBy string) error {
	f.compactionCalls = append(f.compactionCalls, enabled)
	f.requestedBy = append(f.requestedBy, requestedBy)
	if f.compactionErr != nil {
		return f.compactionErr
	}
	f.rewrite(func(p *domain.WorkflowPolicy) {
		p.SessionCompactionEnabled = enabled
		p.CompactionProvenance = domain.SessionCompactionProvenance{
			Version: domain.SessionCompactionPolicyVersion,
			Source:  domain.SessionCompactionExplicit, RequestedBy: requestedBy,
		}
	})
	return nil
}

func (f *contextEconomyWorkflowService) ApplyContextPerCallWarnTokens(_ context.Context, _ string, tokens int64) error {
	f.tokenCalls = append(f.tokenCalls, tokens)
	if f.tokenErr != nil {
		return f.tokenErr
	}
	f.rewrite(func(p *domain.WorkflowPolicy) {
		usage := p.EffectiveUsageBudgetPolicy()
		usage.WorkflowContextPerCallWarnTokens = tokens
		p.Usage = usage
	})
	return nil
}

func (f *contextEconomyWorkflowService) rewrite(edit func(*domain.WorkflowPolicy)) {
	var p domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(f.detail.Run.PolicySnapshot), &p); err != nil {
		return
	}
	edit(&p)
	snapshot, err := json.Marshal(p)
	if err != nil {
		return
	}
	f.detail.Run.PolicySnapshot = string(snapshot)
}

func createdContextEconomyView(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Workflow struct {
			Run struct {
				ContextEconomy map[string]any `json:"contextEconomy"`
			} `json:"run"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp.Workflow.Run.ContextEconomy
}

func createContextEconomyRun(t *testing.T, svc *contextEconomyWorkflowService, body string) ([]byte, int) {
	t.Helper()
	srv := newWorkflowTestServer(t, svc)
	out, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", body)
	return out, status
}

// PHASE E, cases 1-3. The tri-state is a tri-state on the wire: an omitted
// field freezes nothing at all (the run keeps the global default, OFF), and
// BOTH explicit values reach the service as the decision they are. An explicit
// false is not the same as silence, which is the property a controlled A/B's
// control arm depends on.
func TestWorkflowCreateRunSessionCompactionTriState(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want []bool
	}{
		{"omitted freezes nothing", `{"objective":"x","strategy":"task",` + taskVerificationBody + `}`, nil},
		{"explicit false is recorded", `{"objective":"x","strategy":"task","sessionCompaction":false,` + taskVerificationBody + `}`, []bool{false}},
		{"explicit true is recorded", `{"objective":"x","strategy":"task","sessionCompaction":true,` + taskVerificationBody + `}`, []bool{true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &contextEconomyWorkflowService{}
			body, status := createContextEconomyRun(t, svc, tt.body)
			if status != http.StatusCreated {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if len(svc.compactionCalls) != len(tt.want) {
				t.Fatalf("compaction applied %v, want %v", svc.compactionCalls, tt.want)
			}
			for i := range tt.want {
				if svc.compactionCalls[i] != tt.want[i] {
					t.Fatalf("compaction applied %v, want %v", svc.compactionCalls, tt.want)
				}
			}
		})
	}
}

// PHASE E, case 1 stated as the property that actually matters: the default is
// OFF and nothing but an explicit `true` can change that. A request carrying
// every OTHER policy field AO understands must still produce a run whose
// compaction is off and whose provenance says nobody chose.
func TestWorkflowCreateRunLeavesCompactionOffWhenNobodyAsked(t *testing.T) {
	svc := &contextEconomyWorkflowService{}
	body, status := createContextEconomyRun(t, svc,
		`{"objective":"x","strategy":"task","autonomous":true,"repairPolicy":"automatic",`+
			`"autonomyPolicy":"full_autonomy","reviewDepth":"deep",`+taskVerificationBody+`}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(svc.compactionCalls) != 0 {
		t.Fatalf("compaction was frozen by a request that never mentioned it: %v", svc.compactionCalls)
	}
	view := createdContextEconomyView(t, body)
	if view == nil {
		t.Fatalf("response carries no contextEconomy: %s", body)
	}
	if view["sessionCompactionEnabled"] != false {
		t.Fatalf("sessionCompactionEnabled = %v, want false", view["sessionCompactionEnabled"])
	}
	if src, ok := view["compactionSource"]; ok {
		t.Fatalf("compactionSource = %v, want absent for a run nobody chose for", src)
	}
}

// PHASE E, case 4. Opting one run in leaves the NEXT run created against the
// same daemon at the default. There is no sticky state: the freeze writes to
// this run's policy snapshot and to nothing else.
func TestWorkflowCreateRunCompactionDoesNotLeakToTheNextRun(t *testing.T) {
	optedIn := &contextEconomyWorkflowService{}
	body, status := createContextEconomyRun(t, optedIn,
		`{"objective":"a","strategy":"task","sessionCompaction":true,`+taskVerificationBody+`}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if view := createdContextEconomyView(t, body); view["sessionCompactionEnabled"] != true {
		t.Fatalf("first run did not opt in: %s", body)
	}

	next := &contextEconomyWorkflowService{}
	body, status = createContextEconomyRun(t, next,
		`{"objective":"b","strategy":"task",`+taskVerificationBody+`}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(next.compactionCalls) != 0 {
		t.Fatalf("a later run froze compaction nobody asked for: %v", next.compactionCalls)
	}
	if view := createdContextEconomyView(t, body); view["sessionCompactionEnabled"] != false {
		t.Fatalf("compaction leaked into the next run: %s", body)
	}
}

// PHASE E, cases 6-8. Omitted keeps the strategy default; an in-range override
// is frozen and reported back resolved; an out-of-range one is refused BEFORE
// a run exists, with a stable code and no creation.
func TestWorkflowCreateRunContextPerCallWarnTokens(t *testing.T) {
	t.Run("omitted keeps the task profile default", func(t *testing.T) {
		svc := &contextEconomyWorkflowService{}
		body, status := createContextEconomyRun(t, svc,
			`{"objective":"x","strategy":"task",`+taskVerificationBody+`}`)
		if status != http.StatusCreated {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if len(svc.tokenCalls) != 0 {
			t.Fatalf("an omitted field froze %v", svc.tokenCalls)
		}
		view := createdContextEconomyView(t, body)
		want := float64(domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask).ContextPerCallTokens)
		if view["contextPerCallWarnTokens"] != want {
			t.Fatalf("contextPerCallWarnTokens = %v, want the task default %v", view["contextPerCallWarnTokens"], want)
		}
		if over, ok := view["contextPerCallOverridden"]; ok && over != false {
			t.Fatalf("contextPerCallOverridden = %v for a run with no override", over)
		}
	})

	t.Run("an in-range override is frozen and reported", func(t *testing.T) {
		svc := &contextEconomyWorkflowService{}
		body, status := createContextEconomyRun(t, svc,
			`{"objective":"x","strategy":"task","contextPerCallWarnTokens":70000,`+taskVerificationBody+`}`)
		if status != http.StatusCreated {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if len(svc.tokenCalls) != 1 || svc.tokenCalls[0] != 70_000 {
			t.Fatalf("froze %v, want [70000]", svc.tokenCalls)
		}
		view := createdContextEconomyView(t, body)
		if view["contextPerCallWarnTokens"] != float64(70_000) {
			t.Fatalf("contextPerCallWarnTokens = %v, want 70000", view["contextPerCallWarnTokens"])
		}
		if view["contextPerCallOverridden"] != true {
			t.Fatalf("contextPerCallOverridden = %v, want true", view["contextPerCallOverridden"])
		}
	})

	for _, bad := range []struct {
		name  string
		value string
	}{
		{"zero-adjacent", "1"},
		{"negative", "-70000"},
		{"just below the floor", "19999"},
		{"absurdly large", "9000000"},
	} {
		t.Run("refused: "+bad.name, func(t *testing.T) {
			svc := &contextEconomyWorkflowService{}
			body, status := createContextEconomyRun(t, svc,
				`{"objective":"x","strategy":"task","contextPerCallWarnTokens":`+bad.value+`,`+taskVerificationBody+`}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", status, body)
			}
			var env struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if env.Code != "INVALID_CONTEXT_PER_CALL_TOKENS" {
				t.Fatalf("code = %q, want INVALID_CONTEXT_PER_CALL_TOKENS (body %s)", env.Code, body)
			}
			if svc.taskReq != nil {
				t.Fatal("a run was created for a request that should have been refused before creation")
			}
			if len(svc.tokenCalls) != 0 {
				t.Fatalf("a rejected request still froze %v", svc.tokenCalls)
			}
		})
	}
}

// PHASE D. A caller that predates these fields sends exactly what it always
// sent and gets exactly what it always got: compaction off, the strategy's own
// threshold, and not one service call it did not ask for.
func TestWorkflowCreateRunBackwardCompatibleWithoutContextEconomyFields(t *testing.T) {
	svc := &contextEconomyWorkflowService{}
	body, status := createContextEconomyRun(t, svc,
		`{"objective":"x","masterPlan":false,`+taskVerificationBody+`}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(svc.compactionCalls) != 0 || len(svc.tokenCalls) != 0 {
		t.Fatalf("a pre-P7 request froze compaction=%v tokens=%v", svc.compactionCalls, svc.tokenCalls)
	}
	view := createdContextEconomyView(t, body)
	if view["sessionCompactionEnabled"] != false {
		t.Fatalf("sessionCompactionEnabled = %v, want false", view["sessionCompactionEnabled"])
	}
}

// A deployment whose service predates the capability must still serve every
// request that does not ask for it -- that is the compatibility half.
func TestWorkflowCreateRunWithoutContextEconomyCapabilityServesOldRequests(t *testing.T) {
	svc := &strategyWorkflowService{}
	if _, ok := any(svc).(workflowsvc.ContextEconomyManager); ok {
		t.Fatal("this fake must NOT implement ContextEconomyManager for this test to mean anything")
	}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"x","strategy":"task",`+taskVerificationBody+`}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want an unrelated request to be unaffected", status, body)
	}
}

// PHASE E, the failure half of case 3. A daemon that CANNOT honour an explicit
// choice says so instead of creating a run whose policy contradicts the
// request. Silently returning 201 here would hand back a control run labelled
// as a treatment run -- the measurement failure, not merely a lesser run.
func TestWorkflowCreateRunRefusesAnExplicitChoiceWithoutTheCapability(t *testing.T) {
	for _, tt := range []struct{ name, field string }{
		{"compaction true", `"sessionCompaction":true`},
		{"compaction false", `"sessionCompaction":false`},
		{"context threshold", `"contextPerCallWarnTokens":70000`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
				`{"objective":"x","strategy":"task",`+tt.field+`,`+taskVerificationBody+`}`)
			if status != http.StatusNotImplemented {
				t.Fatalf("status=%d body=%s, want 501", status, body)
			}
			var env struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if env.Code != "CONTEXT_ECONOMY_NOT_SUPPORTED" {
				t.Fatalf("code = %q, want CONTEXT_ECONOMY_NOT_SUPPORTED (body %s)", env.Code, body)
			}
			// And it refused BEFORE creating anything. A run left behind by a
			// refused request is not merely untidy: this one is autonomous, so
			// its own kickoff wake starts it executing while the caller holds
			// an error saying it does not exist. wf-88e71ef2 reached a
			// dispatched fix cycle that way.
			if svc.taskReq != nil {
				t.Fatal("a run was created for a request the daemon then refused with 501")
			}
		})
	}
}

// PHASE E, case 3 again, at the sharpest point: the freeze itself fails. The
// run exists and its compaction is OFF; the caller asked for ON. The response
// must be an error, because a 201 here is how an opted-in run and an
// opted-out one become indistinguishable after the fact.
func TestWorkflowCreateRunReportsAFailedContextEconomyFreeze(t *testing.T) {
	t.Run("compaction freeze failure is reported", func(t *testing.T) {
		svc := &contextEconomyWorkflowService{
			compactionErr: fmt.Errorf("%w: workflow run is already running; its session-compaction choice is frozen", workflowsvc.ErrInvalid),
		}
		body, status := createContextEconomyRun(t, svc,
			`{"objective":"x","strategy":"task","sessionCompaction":true,`+taskVerificationBody+`}`)
		if status == http.StatusCreated {
			t.Fatalf("a failed opt-in freeze returned 201: %s", body)
		}
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", status, body)
		}
		// And the response must not carry a run view claiming a policy the
		// run does not have.
		if view := createdContextEconomyView(t, body); view != nil {
			t.Fatalf("a failed freeze still returned a run view: %v", view)
		}
	})

	t.Run("an explicit false is protected the same way", func(t *testing.T) {
		svc := &contextEconomyWorkflowService{
			compactionErr: fmt.Errorf("%w: store unavailable", workflowsvc.ErrInvalid),
		}
		body, status := createContextEconomyRun(t, svc,
			`{"objective":"x","strategy":"task","sessionCompaction":false,`+taskVerificationBody+`}`)
		if status == http.StatusCreated {
			t.Fatalf("a failed control-arm freeze returned 201: %s", body)
		}
	})

	t.Run("threshold freeze failure is reported", func(t *testing.T) {
		svc := &contextEconomyWorkflowService{
			tokenErr: fmt.Errorf("%w: contextPerCallWarnTokens rejected", workflowsvc.ErrInvalid),
		}
		body, status := createContextEconomyRun(t, svc,
			`{"objective":"x","strategy":"task","contextPerCallWarnTokens":70000,`+taskVerificationBody+`}`)
		if status == http.StatusCreated {
			t.Fatalf("a failed threshold freeze returned 201: %s", body)
		}
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", status, body)
		}
	})

	t.Run("an unclassified failure is still an error, not a 201", func(t *testing.T) {
		svc := &contextEconomyWorkflowService{compactionErr: errors.New("disk on fire")}
		body, status := createContextEconomyRun(t, svc,
			`{"objective":"x","strategy":"task","sessionCompaction":true,`+taskVerificationBody+`}`)
		if status != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s, want 500", status, body)
		}
	})
}
