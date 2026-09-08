package controllers_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// taskVerificationBody is the smallest verification plan a task-strategy create
// request can carry, as a JSON object member ready to be spliced into a literal
// request body. Every existing create test that means "a task run" now sends
// it, because a task without executable checks is refused -- see
// TestWorkflowCreateTaskRunWithoutVerificationIsRefused for why.
const taskVerificationBody = `"verification":{"commands":[{"command":"go","args":["test","./..."],"requiredExitCode":0,"retrySafe":true}]}`

// taskVerificationMap is the same plan for the tests that build their request
// body as a map rather than as a string.
func taskVerificationMap() map[string]any {
	return map[string]any{
		"commands": []map[string]any{
			{"command": "go", "args": []string{"test", "./..."}, "requiredExitCode": 0, "retrySafe": true},
		},
	}
}

func createErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Code
}

// The defect this closes: wf-aee38f69-081c-48d1-a3a4-53430ca70682 was created
// as a task whose plan artifact carried `verification: {}`, ran its worker to
// completion, passed review, and only then failed at verify with
// verify_ambiguous. Nothing about the run was recoverable at that point -- the
// missing checks could only have been supplied at creation. So creation is
// where the request is refused, and no run is created.
func TestWorkflowCreateTaskRunWithoutVerificationIsRefused(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"explicit task strategy", `{"objective":"add Farewell()","strategy":"task"}`},
		{"an empty plan object", `{"objective":"add Farewell()","strategy":"task","verification":{}}`},
		{"empty command and file lists", `{"objective":"add Farewell()","strategy":"task","verification":{"commands":[],"files":[]}}`},
		{"auto selection that lands on task", `{"objective":"typo","strategy":"auto","strategySignals":{"size":"small","expectedSteps":1}}`},
		{"a legacy client that names no strategy", `{"objective":"add Farewell()"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", status, body)
			}
			if code := createErrorCode(t, body); code != "VERIFICATION_REQUIRED" {
				t.Fatalf("error code = %q, want VERIFICATION_REQUIRED (body %s)", code, body)
			}
			if svc.taskReq != nil || svc.objectiveSel != nil || svc.legacyPlanCall {
				t.Fatal("a task with no executable verification still created a run")
			}
		})
	}
}

// A command the verify step would refuse to execute is refused at creation
// under the SAME rule, rather than being accepted here and discovered six steps
// later. These are workflow.ValidateVerifyCommand's own refusals, reached
// through VerificationPlan.Validate.
func TestWorkflowCreateTaskRunWithAnUnrunnableCommandIsRefused(t *testing.T) {
	for _, tt := range []struct{ name, verification string }{
		{"a blank command", `{"commands":[{"command":"   ","requiredExitCode":0}]}`},
		{"a shell", `{"commands":[{"command":"bash","args":["-c","go test ./..."],"requiredExitCode":0}]}`},
		{"a destructive command", `{"commands":[{"command":"rm","args":["-rf","."],"requiredExitCode":0}]}`},
		{"a git subcommand that writes", `{"commands":[{"command":"git","args":["push"],"requiredExitCode":0}]}`},
		{"a working directory outside the workspace", `{"commands":[{"command":"go","args":["test"],"workingDirectory":"../../etc","requiredExitCode":0}]}`},
		{"an absolute working directory", `{"commands":[{"command":"go","args":["test"],"workingDirectory":"/etc","requiredExitCode":0}]}`},
		{"an out-of-range timeout", `{"commands":[{"command":"go","args":["test"],"timeoutSeconds":7200,"requiredExitCode":0}]}`},
		{"a file check with no path", `{"files":[{"path":"","exists":true}]}`},
		{"a file check that escapes the workspace", `{"files":[{"path":"../secrets.env","exists":true}]}`},
		{"a malformed sha256", `{"files":[{"path":"out.txt","exists":true,"sha256":"nothex"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
				`{"objective":"x","strategy":"task","verification":`+tt.verification+`}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", status, body)
			}
			if code := createErrorCode(t, body); code != "VERIFICATION_REQUIRED" {
				t.Fatalf("error code = %q, want VERIFICATION_REQUIRED (body %s)", code, body)
			}
			if svc.taskReq != nil {
				t.Fatal("an unrunnable verification plan still created a run")
			}
		})
	}
}

// The refusal is the task strategy's alone. A planned run's checks come from
// its planner, which has not run yet at creation, so requiring them here would
// make every autonomous and master objective uncreatable.
func TestWorkflowCreatePlannedRunNeedsNoVerification(t *testing.T) {
	for _, strategy := range []string{"autonomous", "master"} {
		svc := &strategyWorkflowService{}
		srv := newWorkflowTestServer(t, svc)
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
			`{"objective":"build the thing","strategy":"`+strategy+`"}`)
		if status != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", strategy, status, body)
		}
	}
}

// What the caller sent is what the coordinator receives: the create route is a
// pass-through for the verification plan, not a place that normalises,
// re-orders or invents checks. A run created with the wrong checks is as
// unverifiable as one created with none.
func TestWorkflowCreateTaskRunForwardsTheVerificationPlanVerbatim(t *testing.T) {
	svc := &strategyWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	exact := "Goodbye!\n"
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"add Farewell()","strategy":"task",`+
			`"acceptanceCriteria":["Farewell(\"\") returns \"Goodbye!\""],`+
			`"verification":{`+
			`"commands":[`+
			`{"command":"go","args":["test","./..."],"workingDirectory":"pkg","timeoutSeconds":120,"requiredExitCode":0,"retrySafe":true},`+
			`{"command":"go","args":["build","./..."],"requiredExitCode":0,"retrySafe":false}`+
			`],`+
			`"files":[{"path":"farewell.go","exists":true,"exactContent":"Goodbye!\n","sha256":"`+strings.Repeat("a", 64)+`"}]`+
			`}}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.taskReq == nil {
		t.Fatal("no task run was created")
	}
	got := svc.taskReq.Verification
	want := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: "pkg", TimeoutSeconds: 120, RequiredExitCode: 0, RetrySafe: true},
			{Command: "go", Args: []string{"build", "./..."}, RequiredExitCode: 0, RetrySafe: false},
		},
		Files: []workflowcore.VerificationFileCheck{
			{Path: "farewell.go", Exists: true, ExactContent: &exact, SHA256: strings.Repeat("a", 64)},
		},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("verification plan reached the coordinator as %s, want %s", gotJSON, wantJSON)
	}
	if len(svc.taskReq.AcceptanceCriteria) != 1 || svc.taskReq.AcceptanceCriteria[0] != `Farewell("") returns "Goodbye!"` {
		t.Fatalf("acceptance criteria = %q", svc.taskReq.AcceptanceCriteria)
	}
	// Whatever is accepted here must be executable by the verify step itself:
	// the create route applies that step's own rule, so the two can never
	// disagree about what "verifiable" means.
	if err := got.Validate(); err != nil {
		t.Fatalf("an accepted plan is not one verify can execute: %v", err)
	}
}
