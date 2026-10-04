package webdast_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
	"github.com/aoagents/agent-orchestrator/backend/internal/webdast"
)

// vulnerableServer is a deliberately-misconfigured target: no security headers,
// a flagless session cookie, permissive CORS, TRACE enabled, an exposed
// .git/config, and a versioned Server banner. It exists only in-process for the
// checker's unit tests; it is never a real system.
func vulnerableServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.18.0")
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodTrace {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s3cr3t-value-1234"})
		_, _ = w.Write([]byte("<html>hello</html>"))
	})
	mux.HandleFunc("/.git/config", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[core]\n\trepositoryformatversion = 0\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func targetFor(t *testing.T, srv *httptest.Server) webdast.Target {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	return webdast.Target{Scheme: "http", Host: u.Hostname(), Port: port}
}

func baseConfig(t *testing.T, srv *httptest.Server) webdast.Config {
	return webdast.Config{
		SchemaVersion:    webdast.ConfigSchemaVersion,
		ProjectID:        "proj-1",
		SkillID:          "security-audit",
		SkillVersion:     "0.4.0",
		RunID:            "skr-000000000000000000000001",
		AuthorizationID:  "auth-1",
		Target:           targetFor(t, srv),
		RequestedBy:      "ada",
		AuthorizationRef: "TICKET-42",
		Limits:           webdast.Limits{MaxDurationMs: 20000},
	}
}

func TestRun_DetectsMisconfigurationsAndValidatesAgainstSchema(t *testing.T) {
	srv := vulnerableServer(t)
	report, err := webdast.Run(context.Background(), baseConfig(t, srv), []string{"egress-allowlist", "arbitrary-process-execution"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The report the checker emits must be exactly what the daemon will accept.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := skillreport.PentestSchema().Validate(raw); err != nil {
		t.Fatalf("emitted report is not schema-valid: %v\n%s", err, raw)
	}

	byCheck := map[string]bool{}
	for _, f := range report.Findings {
		byCheck[f.Check] = true
	}
	for _, want := range []string{
		"security-headers", "cookies", "cors", "http-methods",
		"sensitive-path-exposure", "info-disclosure",
	} {
		if !byCheck[want] {
			t.Errorf("expected a %s finding, got none; findings=%+v", want, report.Findings)
		}
	}

	// The CORS finding on this fixture is the high-severity credentialed-wildcard
	// OR reflected-origin; either way it must be present and non-info.
	var corsSev string
	for _, f := range report.Findings {
		if f.Check == "cors" {
			corsSev = f.Severity
		}
	}
	if corsSev == "" || corsSev == "info" {
		t.Errorf("CORS finding severity = %q, want a real severity", corsSev)
	}

	// Coverage must state what was NOT attempted, so an empty list never reads
	// as a clean bill of health.
	if len(report.Coverage.NotAttempted) == 0 {
		t.Errorf("coverage.notAttempted is empty")
	}
	if report.RequestsMade == 0 {
		t.Errorf("requestsMade is 0")
	}
}

func TestRun_SensitivePathFindingCarriesNoContents(t *testing.T) {
	srv := vulnerableServer(t)
	report, err := webdast.Run(context.Background(), baseConfig(t, srv), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, f := range report.Findings {
		if f.Check != "sensitive-path-exposure" {
			continue
		}
		for _, step := range f.Reproduction.Steps {
			if strings.Contains(step, "repositoryformatversion") {
				t.Fatalf("sensitive-path finding leaked file contents: %q", step)
			}
		}
	}
}

func TestRun_RequestBudgetIsEnforced(t *testing.T) {
	srv := vulnerableServer(t)
	cfg := baseConfig(t, srv)
	cfg.Limits = webdast.Limits{MaxRequests: 2, MaxDurationMs: 20000}
	report, err := webdast.Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.RequestsMade > 2 {
		t.Fatalf("request budget not enforced: made %d, cap 2", report.RequestsMade)
	}
	if report.Limits.MaxRequests != 2 {
		t.Fatalf("effective limit not reported: %+v", report.Limits)
	}
}

func TestRun_LimitsAreClampedToSafeCeilings(t *testing.T) {
	srv := vulnerableServer(t)
	cfg := baseConfig(t, srv)
	// A caller asking for a load-test's worth of traffic is clamped back down.
	cfg.Limits = webdast.Limits{MaxRequests: 1000000, RequestsPerSecond: 100000, Concurrency: 999, MaxDurationMs: 999999999, MaxResponseBytes: 1 << 40, MaxEndpoints: 100000}
	report, err := webdast.Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d := webdast.DefaultLimits()
	if report.Limits.MaxRequests != d.MaxRequests || report.Limits.RequestsPerSecond != d.RequestsPerSecond ||
		report.Limits.Concurrency != d.Concurrency || report.Limits.MaxResponseBytes != d.MaxResponseBytes {
		t.Fatalf("limits not clamped to defaults: %+v", report.Limits)
	}
}

func TestRun_RefusesUnknownConfigVersion(t *testing.T) {
	srv := vulnerableServer(t)
	cfg := baseConfig(t, srv)
	cfg.SchemaVersion = "ao.webdast-config/v2"
	if _, err := webdast.Run(context.Background(), cfg, nil); err == nil {
		t.Fatalf("run accepted an unknown config schemaVersion")
	}
}
