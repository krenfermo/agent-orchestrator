package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// work_test.go — `ao work report`, the transport a worker uses to declare what
// it says it did.
//
// The tests are mostly about refusals, and deliberately so. The command's whole
// risk is an agent coming to believe that reporting a pass is the same as
// having one, so the parsing has to be strict about what it will accept and the
// wire format has to keep saying "claimed" at every level.

func TestWorkReportSendsFlagsToTheSessionRoute(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, 200, `{"workflowRunId":"wf-1","workflowStepId":"wfs-1","version":"work-report/v1"}`)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, aliveDeps(),
		"work", "report", "sess-1",
		"--summary", "Renamed the helper",
		"--test", "go test ./pkg/...=passed",
		"--criterion", "handle empty input=unaddressed",
		"--limitation", "windows path untested",
		"--risk", "may change the cache key",
		"--follow-up", "add a benchmark")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.method != "POST" || capture.path != "/api/v1/sessions/sess-1/work-report" {
		t.Fatalf("request = %s %s", capture.method, capture.path)
	}
	var req submitWorkReportRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if req.Summary != "Renamed the helper" {
		t.Errorf("summary = %q", req.Summary)
	}
	// The CLI never asks an agent to type "claimed"; it adds the prefix, so the
	// wire format cannot carry anything that reads as a verified result.
	if len(req.TestsReported) != 1 || req.TestsReported[0].ClaimedOutcome != "claimed_passed" {
		t.Fatalf("testsReported = %+v", req.TestsReported)
	}
	if req.TestsReported[0].Command != "go test ./pkg/..." {
		t.Errorf("command = %q; the split must keep a command's own '='", req.TestsReported[0].Command)
	}
	if len(req.Criteria) != 1 || req.Criteria[0].Addressed {
		t.Fatalf("criteria = %+v, want one unaddressed", req.Criteria)
	}
	if len(req.Limitations) != 1 || len(req.Risks) != 1 || len(req.FollowUp) != 1 {
		t.Errorf("limitations/risks/followUp = %v / %v / %v", req.Limitations, req.Risks, req.FollowUp)
	}
	if !strings.Contains(out, "recorded work report") || !strings.Contains(out, "wf-1") {
		t.Errorf("stdout = %q", out)
	}
}

func TestWorkReportReadsJSONFromStdin(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, 200, `{"workflowRunId":"wf-2","version":"work-report/v1","superseded":true,"truncated":["summary"]}`)
	writeRunFileFor(t, cfg, srv)

	deps := aliveDeps()
	deps.In = strings.NewReader(`{"summary":"did the thing","testsReported":[{"command":"go build ./...","claimedOutcome":"claimed_failed"}]}`)

	out, errOut, err := executeCLI(t, deps, "work", "report", "sess-2", "--json", "-")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	var req submitWorkReportRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if req.Summary != "did the thing" || len(req.TestsReported) != 1 {
		t.Fatalf("request = %+v", req)
	}
	// A replaced report and a clipped one are both said out loud: a worker that
	// believes it delivered a full report and did not must be able to find out.
	if !strings.Contains(out, "replacing an earlier report") {
		t.Errorf("stdout does not report the supersession: %q", out)
	}
	if !strings.Contains(out, "AO shortened these fields") || !strings.Contains(out, "summary") {
		t.Errorf("stdout does not report the truncation: %q", out)
	}
}

func TestWorkReportDefaultsTheSessionFromTheEnvironment(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, 200, `{"workflowRunId":"wf-3","version":"work-report/v1"}`)
	writeRunFileFor(t, cfg, srv)
	// AO sets this in every pane it launches, which is what lets the worker
	// prompt say "ao work report" with no session argument.
	t.Setenv("AO_SESSION_ID", "sess-from-env")

	if _, errOut, err := executeCLI(t, aliveDeps(), "work", "report", "--summary", "done"); err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.path != "/api/v1/sessions/sess-from-env/work-report" {
		t.Fatalf("path = %q", capture.path)
	}
}

// Every way of asking for something the command must not do.
func TestWorkReportRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{
			name: "no session anywhere",
			args: []string{"work", "report", "--summary", "done"},
			want: "session id is required",
		},
		{
			name: "nothing to report",
			env:  "sess-1",
			args: []string{"work", "report"},
			want: "nothing to report",
		},
		{
			name: "json mixed with flags",
			env:  "sess-1",
			args: []string{"work", "report", "--json", "-", "--summary", "done"},
			want: "--json cannot be combined",
		},
		{
			name: "an outcome that is not one of the three",
			env:  "sess-1",
			args: []string{"work", "report", "--test", "go test ./...=probably fine"},
			want: "must be passed, failed or skipped",
		},
		{
			name: "a test claim with no outcome at all",
			env:  "sess-1",
			args: []string{"work", "report", "--test", "go test ./..."},
			want: "must be 'command=outcome'",
		},
		{
			name: "a criterion state that is not one of the two",
			env:  "sess-1",
			args: []string{"work", "report", "--criterion", "handle empty input=mostly"},
			want: "must be addressed or unaddressed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv, capture := reviewServer(t, 200, `{}`)
			writeRunFileFor(t, cfg, srv)
			if tc.env != "" {
				t.Setenv("AO_SESSION_ID", tc.env)
			} else {
				t.Setenv("AO_SESSION_ID", "")
			}
			_, errOut, err := executeCLI(t, aliveDeps(), tc.args...)
			if err == nil {
				t.Fatal("the command succeeded; it must refuse")
			}
			if !strings.Contains(err.Error()+errOut, tc.want) {
				t.Fatalf("error = %v / %s, want it to mention %q", err, errOut, tc.want)
			}
			// A refused command must not have submitted anything. The CLI
			// posts its own telemetry on every invocation, so the assertion is
			// against the work-report route rather than against "any request".
			if strings.Contains(capture.path, "work-report") {
				t.Fatalf("a refused command still sent %s %s", capture.method, capture.path)
			}
		})
	}
}

// Underscored flag spellings resolve to the same flags, exactly as they do for
// `ao review submit`: agents write --follow_up more often than --follow-up.
func TestWorkReportAcceptsUnderscoredFlags(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := reviewServer(t, 200, `{"workflowRunId":"wf-4","version":"work-report/v1"}`)
	writeRunFileFor(t, cfg, srv)

	if _, errOut, err := executeCLI(t, aliveDeps(),
		"work", "report", "sess-1", "--follow_up", "add a benchmark"); err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	var req submitWorkReportRequest
	if err := json.Unmarshal([]byte(capture.body), &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(req.FollowUp) != 1 {
		t.Fatalf("followUp = %v", req.FollowUp)
	}
}

// A daemon that refuses the report surfaces as a failure the agent can see. The
// report is advisory to AO's policy, but a transport failure must never be
// silent — a worker that believes it reported and did not is the one state this
// command must not produce.
func TestWorkReportSurfacesADaemonRefusal(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := reviewServer(t, 409,
		`{"error":"conflict","code":"WORK_REPORT_WINDOW_CLOSED","message":"This run has already decided how deeply to review the change"}`)
	writeRunFileFor(t, cfg, srv)

	_, errOut, err := executeCLI(t, aliveDeps(), "work", "report", "sess-1", "--summary", "late")
	if err == nil {
		t.Fatal("a 409 was reported as success")
	}
	if !strings.Contains(err.Error()+errOut, "already decided") {
		t.Fatalf("error = %v / %s", err, errOut)
	}
}
