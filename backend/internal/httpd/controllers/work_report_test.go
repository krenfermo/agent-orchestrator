package controllers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// work_report_test.go — the HTTP surface of P5-A phase 2B.
//
// The route reuses ReviewsController's session scoping, so what these tests
// pin is the part that is this route's own: that a declaration reaches the
// service unedited, that each refusal maps to the status a client can act on,
// and that a build without the capability says so rather than pretending.

// workReportService is a Manager that also implements WorkReportManager.
type workReportService struct {
	fakeWorkflowService
	got     []domain.WorkReport
	gotSess []string
	receipt workflowcore.WorkReportReceipt
	err     error
}

func (s *workReportService) SubmitWorkReportForSession(
	_ context.Context, sessionID string, report domain.WorkReport,
) (workflowcore.WorkReportReceipt, error) {
	s.gotSess = append(s.gotSess, sessionID)
	s.got = append(s.got, report)
	if s.err != nil {
		return workflowcore.WorkReportReceipt{}, s.err
	}
	return s.receipt, nil
}

func TestWorkReportRouteForwardsTheDeclarationUnedited(t *testing.T) {
	svc := &workReportService{receipt: workflowcore.WorkReportReceipt{
		WorkflowRunID:  "wf-1",
		WorkflowStepID: "wfs-1",
		Superseded:     true,
		Report:         domain.WorkReport{Version: domain.WorkReportVersion, Truncated: []string{"summary"}},
	}}
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/sess-1/work-report", `{
		"summary":"Renamed the helper",
		"claimedChangedPaths":["pkg/helper.go"],
		"criteria":[{"criterion":"handle empty input","addressed":false,"note":"ran out of time"}],
		"testsReported":[{"command":"go test ./pkg/...","claimedOutcome":"claimed_passed"}],
		"limitations":["windows path untested"],
		"risks":["may change the cache key"],
		"followUp":["add a benchmark"],
		"commit":"abc123"
	}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(svc.got) != 1 || svc.gotSess[0] != "sess-1" {
		t.Fatalf("service saw sessions=%v reports=%d", svc.gotSess, len(svc.got))
	}
	got := svc.got[0]
	if got.Summary != "Renamed the helper" {
		t.Errorf("summary = %q", got.Summary)
	}
	// The claim arrives as a CLAIM, at every layer.
	if len(got.TestsReported) != 1 || got.TestsReported[0].ClaimedOutcome != domain.WorkReportOutcomeClaimedPassed {
		t.Fatalf("testsReported = %+v", got.TestsReported)
	}
	if len(got.Criteria) != 1 || got.Criteria[0].Addressed {
		t.Fatalf("criteria = %+v", got.Criteria)
	}
	if got.Reference.Commit != "abc123" {
		t.Errorf("commit = %q", got.Reference.Commit)
	}
	// The controller must NOT stamp a fingerprint: only AO's own reading of the
	// worktree may fill that in, and the controller has not read one.
	if got.Reference.FingerprintAtSubmission != "" {
		t.Errorf("the controller invented a fingerprint: %q", got.Reference.FingerprintAtSubmission)
	}

	var res struct {
		WorkflowRunID string   `json:"workflowRunId"`
		Superseded    bool     `json:"superseded"`
		Version       string   `json:"version"`
		Truncated     []string `json:"truncated"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.WorkflowRunID != "wf-1" || !res.Superseded || res.Version != domain.WorkReportVersion {
		t.Fatalf("response = %+v", res)
	}
	if len(res.Truncated) != 1 {
		t.Errorf("the response does not report the truncation: %+v", res)
	}
}

// An outcome the daemon does not recognise must never arrive as a pass. The
// domain normalizer turns it into "unstated"; what this pins is that the
// controller passes it through verbatim rather than guessing at it first.
func TestWorkReportRouteDoesNotRepairAnUnknownOutcome(t *testing.T) {
	svc := &workReportService{receipt: workflowcore.WorkReportReceipt{WorkflowRunID: "wf-1"}}
	srv := newWorkflowTestServer(t, svc)

	_, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/sess-1/work-report",
		`{"testsReported":[{"command":"go test ./...","claimedOutcome":"definitely fine"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	got := svc.got[0].TestsReported[0].ClaimedOutcome
	if got == domain.WorkReportOutcomeClaimedPassed {
		t.Fatal("an unrecognised outcome was upgraded to a pass at the transport")
	}
	if got != "definitely fine" {
		t.Fatalf("claimedOutcome = %q; the controller must forward it verbatim", got)
	}
}

// Each refusal maps to the status that tells a client what to do about it.
func TestWorkReportRouteStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{"the window has closed", workflowcore.ErrWorkReportWindowClosed, http.StatusConflict, "WORK_REPORT_WINDOW_CLOSED"},
		{"no work step in this session", workflowcore.ErrNotFound, http.StatusNotFound, "WORK_REPORT_NO_ACTIVE_STEP"},
		{"the request is unusable", workflowcore.ErrInvalid, http.StatusBadRequest, "WORK_REPORT_INVALID"},
		// A durable-write failure: the report did not land, and the caller is
		// told so rather than getting a success it cannot rely on.
		{"the store would not write it", errors.New("database is locked"), http.StatusInternalServerError, "WORK_REPORT_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &workReportService{err: tc.err}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/sess-1/work-report", `{"summary":"x"}`)
			if status != tc.want {
				t.Fatalf("status=%d want=%d body=%s", status, tc.want, body)
			}
			if !strings.Contains(string(body), tc.code) {
				t.Fatalf("body does not name %q: %s", tc.code, body)
			}
		})
	}
}

func TestWorkReportRouteRejectsInvalidJSON(t *testing.T) {
	svc := &workReportService{}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/sess-1/work-report", `{not json`)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(svc.got) != 0 {
		t.Fatal("an unparseable body still reached the service")
	}
}

// A deployment whose workflow service predates the capability answers 501
// rather than 500 or a silent success — the same contract every other
// type-asserted manager on this controller family has.
func TestWorkReportRouteWithoutTheCapability(t *testing.T) {
	srv := newWorkflowTestServer(t, &fakeWorkflowService{})
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/sess-1/work-report", `{"summary":"x"}`)
	if status != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s", status, body)
	}
}
