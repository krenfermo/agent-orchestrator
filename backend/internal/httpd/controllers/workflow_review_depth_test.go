package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
)

// reviewDepthWorkflowService records the review-depth request the controller
// applied after creation, so these tests can assert the wire contract and the
// routing decision. What the depth then MEANS is the coordinator's job and is
// covered in internal/workflow.
type reviewDepthWorkflowService struct {
	strategyWorkflowService
	applied  []domain.ReviewDepth
	applyErr error
	frozen   domain.ReviewDepth
}

var _ workflowsvc.ReviewDepthManager = (*reviewDepthWorkflowService)(nil)

func (f *reviewDepthWorkflowService) ApplyReviewDepthPolicy(_ context.Context, _ string, depth domain.ReviewDepth) error {
	f.applied = append(f.applied, depth)
	return f.applyErr
}

func (f *reviewDepthWorkflowService) RunReviewDepthPolicy(context.Context, string) (domain.ReviewDepthPolicySnapshot, error) {
	return domain.ReviewDepthPolicySnapshot{
		Version: domain.ReviewDepthPolicyVersion, Requested: f.frozen, Source: domain.ReviewDepthPolicy,
	}, nil
}

func createdReviewDepthView(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Workflow struct {
			Run struct {
				ReviewDepth map[string]any `json:"reviewDepth"`
			} `json:"run"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp.Workflow.Run.ReviewDepth
}

// Every accepted depth reaches the coordinator; "auto" and an omitted field
// reach it as nothing at all, leaving creation's own strategy default in place.
func TestWorkflowCreateRunReviewDepthRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want []domain.ReviewDepth
	}{
		{"none", `{"objective":"x","strategy":"task","reviewDepth":"none"}`, []domain.ReviewDepth{domain.ReviewDepthNone}},
		{"light", `{"objective":"x","strategy":"task","reviewDepth":"light"}`, []domain.ReviewDepth{domain.ReviewDepthLight}},
		{"deep", `{"objective":"x","strategy":"task","reviewDepth":"deep"}`, []domain.ReviewDepth{domain.ReviewDepthDeep}},
		{"mixed case and spacing is normalized", `{"objective":"x","strategy":"task","reviewDepth":" LIGHT "}`, []domain.ReviewDepth{domain.ReviewDepthLight}},
		{"auto applies nothing", `{"objective":"x","strategy":"task","reviewDepth":"auto"}`, nil},
		{"omitted applies nothing", `{"objective":"x","strategy":"task"}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &reviewDepthWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", tt.body)
			if status != http.StatusCreated {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if len(svc.applied) != len(tt.want) {
				t.Fatalf("applied %v, want %v", svc.applied, tt.want)
			}
			for i := range tt.want {
				if svc.applied[i] != tt.want[i] {
					t.Fatalf("applied %v, want %v", svc.applied, tt.want)
				}
			}
		})
	}
}

// A depth outside the vocabulary is refused BEFORE a run exists, with the
// stable error code, rather than silently normalised into something nobody
// asked for.
func TestWorkflowCreateRunRejectsAnUnknownReviewDepth(t *testing.T) {
	svc := &reviewDepthWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"x","strategy":"task","reviewDepth":"skim"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", status, body)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if env.Code != "INVALID_REVIEW_DEPTH" {
		t.Fatalf("error code = %q, want INVALID_REVIEW_DEPTH (body %s)", env.Code, body)
	}
	if len(svc.applied) != 0 {
		t.Fatalf("a rejected request still applied %v", svc.applied)
	}
	if svc.taskReq != nil {
		t.Fatal("a run was created for a request that should have been refused before creation")
	}
}

// The response states the run's frozen request, and a snapshot from before the
// model reads as deep rather than as something cheaper.
func TestWorkflowRunViewCarriesTheFrozenReviewDepth(t *testing.T) {
	svc := &reviewDepthWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"x","strategy":"task"}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	view := createdReviewDepthView(t, body)
	if view == nil {
		t.Fatalf("response carries no reviewDepth: %s", body)
	}
	// strategyWorkflowService.created() builds its snapshot from
	// DefaultWorkflowPolicy, which predates the depth model — so this is
	// exactly the legacy-snapshot case, and it must read as deep.
	if view["requestedDepth"] != string(domain.ReviewDepthDeep) {
		t.Fatalf("requestedDepth = %v, want deep for a snapshot with no recorded depth", view["requestedDepth"])
	}
	if view["source"] != string(domain.ReviewDepthRecovered) {
		t.Fatalf("source = %v, want recovered", view["source"])
	}
}

// A deployment whose service predates the capability must still create the run
// on its frozen strategy default rather than failing the request.
func TestWorkflowCreateRunWithoutReviewDepthCapability(t *testing.T) {
	svc := &strategyWorkflowService{}
	if _, ok := any(svc).(workflowsvc.ReviewDepthManager); ok {
		t.Fatal("this fake must NOT implement ReviewDepthManager for this test to mean anything")
	}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"x","strategy":"task","reviewDepth":"light"}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want the run to be created anyway", status, body)
	}
}
