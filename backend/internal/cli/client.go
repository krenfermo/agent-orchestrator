package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// commandTimeout bounds a mutating daemon call. Spawns do real work (git
// worktree add, tmux launch, hook install), so it is generous compared to the
// status probe timeout.
const commandTimeout = 2 * time.Minute

// apiError is the subset of the daemon's JSON error envelope the CLI surfaces.
// RequestID is surfaced so a failed command can be correlated with daemon logs.
type apiError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

type apiResponseError struct {
	StatusCode int
	ErrorBody  apiError
}

func (e apiResponseError) Error() string {
	if e.ErrorBody.Message == "" {
		return fmt.Sprintf("daemon returned HTTP %d", e.StatusCode)
	}
	msg := e.ErrorBody.String()
	if e.unauthenticated() {
		// The bare envelope ("authentication required (NOT_AUTHENTICATED)")
		// is true and useless: it names no next step, and on an SSO
		// installation the next step is not obvious. Say it here, once, so
		// every command inherits it instead of each one re-explaining.
		msg += "\n  This installation requires sign-in. Run `ao auth login` to sign in as yourself, then retry."
	}
	return msg
}

// unauthenticated reports the daemon's "no identity resolved" answer — the
// one failure the CLI can tell the user how to fix itself.
func (e apiResponseError) unauthenticated() bool {
	return e.StatusCode == http.StatusUnauthorized && e.ErrorBody.Code == "NOT_AUTHENTICATED"
}

// String renders the envelope for the user: "<message> (<code>) [request <id>]",
// omitting whichever parts the daemon left empty.
func (e apiError) String() string {
	msg := e.Message
	if e.Code != "" {
		msg = fmt.Sprintf("%s (%s)", msg, e.Code)
	}
	if e.RequestID != "" {
		msg = fmt.Sprintf("%s [request %s]", msg, e.RequestID)
	}
	return msg
}

// getJSON sends GET /api/v1/<path> to the running daemon and decodes a 2xx
// response into out. A missing daemon or non-2xx API envelope is rendered the
// same way as mutating calls.
func (c *commandContext) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

// postJSON sends body as JSON to POST /api/v1/<path> on the running daemon and
// decodes a 2xx response into out (out may be nil). A non-2xx response becomes
// an error built from the API error envelope. A missing run-file or a stale one
// (dead PID) yields a clear "not running" message rather than a
// connection-refused dump.
func (c *commandContext) postJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, body, out)
}

// patchJSON sends body as JSON to PATCH /api/v1/<path> on the running daemon
// and decodes a 2xx response into out.
func (c *commandContext) patchJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPatch, path, body, out)
}

// putJSON sends body as JSON to PUT /api/v1/<path> on the running daemon and
// decodes a 2xx response into out.
func (c *commandContext) putJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPut, path, body, out)
}

// deleteJSON sends DELETE /api/v1/<path> to the running daemon and decodes a
// 2xx response into out.
func (c *commandContext) deleteJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodDelete, path, nil, out)
}

func (c *commandContext) doJSON(ctx context.Context, method, path string, body, out any) error {
	return c.doJSONPath(ctx, method, "/api/v1/"+path, body, out)
}

func (c *commandContext) postLoopbackJSON(ctx context.Context, path string, body any) error {
	return c.doJSONPath(ctx, http.MethodPost, path, body, nil)
}

func (c *commandContext) doJSONPath(ctx context.Context, method, path string, body, out any) error {
	return c.doJSONPathWithHeaders(ctx, method, path, body, out, nil)
}

func (c *commandContext) doJSONPathWithHeaders(
	ctx context.Context,
	method, path string,
	body, out any,
	headers map[string]string,
) error {
	return c.doJSONPathWithHeadersAndTimeout(ctx, method, path, body, out, headers, commandTimeout)
}

func (c *commandContext) doJSONPathWithHeadersAndTimeout(
	ctx context.Context,
	method, path string,
	body, out any,
	headers map[string]string,
	timeout time.Duration,
) error {
	return c.doJSONPathFull(ctx, method, path, body, out, headers, timeout, nil)
}

// doJSONPathFull is the one request path every CLI daemon call goes through.
// respCookies, when non-nil, receives the response's Set-Cookie cookies —
// `ao auth login` is the only caller that needs them, because the daemon hands
// the CLI its session the same way it hands the desktop supervisor one: as a
// Set-Cookie on a loopback response, never as a token in a JSON body.
func (c *commandContext) doJSONPathFull(
	ctx context.Context,
	method, path string,
	body, out any,
	headers map[string]string,
	timeout time.Duration,
	respCookies *[]*http.Cookie,
) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("AO daemon is not running — start it with `ao start`")
	}
	if !c.deps.ProcessAlive(info.PID) {
		return fmt.Errorf("AO daemon is not running (stale run-file at %s) — start it with `ao start`", cfg.RunFilePath)
	}

	var reader io.Reader = http.NoBody
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	url := fmt.Sprintf("http://%s:%d%s", config.LoopbackHost, info.Port, path)
	req, err := http.NewRequestWithContext(ctx, method, url, reader) // #nosec G704 -- daemon host is fixed loopback; path is an internal API route.
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The CLI's identity, when it has one. Attached here rather than per
	// command so `ao send`, `ao workflow resume`, `ao hooks` and every other
	// route inherit exactly the same principal — there is one credential and
	// one place that presents it. See credentials.go.
	c.attachCredential(cfg.DataDir, req)
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	// Reuse the injected client's transport (keeps it stubbable in tests) but
	// give daemon API calls far more headroom than the 2s status-probe timeout.
	client := *c.deps.HTTPClient
	client.Timeout = timeout
	resp, err := client.Do(req) // #nosec G704 -- request target is the fixed loopback daemon URL above.
	if err != nil {
		return fmt.Errorf("call daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e apiError
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return apiResponseError{StatusCode: resp.StatusCode, ErrorBody: e}
	}
	if respCookies != nil {
		*respCookies = resp.Cookies()
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// attachCredential presents the stored session on this request, when there is
// a usable one. It is deliberately silent about every failure mode: a missing
// file is the normal trusted-local case, and an unreadable or expired
// credential must degrade to "no identity" — which the daemon answers with the
// same actionable 401 as sending nothing — rather than failing the command
// before it is even sent.
func (c *commandContext) attachCredential(dataDir string, req *http.Request) {
	cred, ok, err := readCredential(dataDir)
	if err != nil || !ok {
		return
	}
	if !cred.valid(c.deps.Now()) {
		return
	}
	// The same cookie the browser and the desktop supervisor present. Reusing
	// the session cookie rather than inventing a CLI-only header is what keeps
	// the CLI on the daemon's existing identity path: one resolver, one
	// revocation story, one audit trail.
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cred.Token})
}
