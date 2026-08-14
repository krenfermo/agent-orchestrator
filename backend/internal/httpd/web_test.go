package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

func TestHeadlessWebStaticAndSPAFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<main>AO web</main>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("window.AO=true"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Host:           config.LoopbackHost,
		Port:           config.DefaultPort,
		RequestTimeout: config.DefaultRequestTimeout,
		AllowedOrigins: config.DefaultAllowedOrigins,
	}
	cfg.WebRoot = root
	handler := NewRouterWithControl(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{}, ControlDeps{})

	for _, tc := range []struct {
		name        string
		path        string
		status      int
		contains    string
		contentType string
	}{
		{name: "index", path: "/", status: http.StatusOK, contains: "AO web", contentType: "text/html"},
		{name: "asset", path: "/assets/app.js", status: http.StatusOK, contains: "window.AO", contentType: "javascript"},
		{name: "deep link", path: "/workflows/run-123", status: http.StatusOK, contains: "AO web", contentType: "text/html"},
		{name: "missing asset", path: "/assets/missing.js", status: http.StatusNotFound, contains: "404", contentType: "text/plain"},
		{name: "api not swallowed", path: "/api/v1/not-a-route", status: http.StatusNotFound, contains: `"code":"ROUTE_NOT_FOUND"`, contentType: "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", res.Code, tc.status, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), tc.contains) {
				t.Fatalf("body %q does not contain %q", res.Body.String(), tc.contains)
			}
			if got := res.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
				t.Fatalf("Content-Type = %q, want %q", got, tc.contentType)
			}
		})
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(res.Body.String(), `"frontendAvailable":true`) {
		t.Fatalf("health body = %s", res.Body.String())
	}
}
