package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
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
	// InsideAgent records that this request came from inside an AO-launched
	// agent's runtime. It changes only the hint on a 401: telling a reviewer
	// pane to "run ao auth login" is advice it cannot take -- there is no
	// browser and no person -- and would send whoever reads the transcript
	// chasing the wrong problem.
	InsideAgent bool
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
		if e.InsideAgent {
			msg += "\n  This agent's own credential is missing, expired or revoked, so AO could not" +
				"\n  identify it. Do not sign in as a person from here: report this and let the" +
				"\n  workflow relaunch the agent, which mints a fresh credential for it."
		} else {
			msg += "\n  This installation requires sign-in. Run `ao auth login` to sign in as yourself, then retry."
		}
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
		return apiResponseError{
			StatusCode:  resp.StatusCode,
			ErrorBody:   e,
			InsideAgent: agentcred.InsideAgentRuntime(c.deps.LookupEnv),
		}
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
	// P4-I: inside an AO-launched agent, the agent's OWN credential is the only
	// identity this process may present.
	//
	// Both halves matter. Presenting the agent credential is what lets a
	// reviewer record its verdict at all on an SSO installation. Refusing to
	// fall back on the operator's credential is what keeps that from being an
	// escalation: the file under <AO_DATA_DIR> is readable by this user, so a
	// reviewer that fell back would act with the operator's authority over
	// every project in every organization -- to record one verdict. An agent
	// that cannot authenticate as itself sends nothing and is told so.
	if agentcred.InsideAgentRuntime(c.deps.LookupEnv) {
		c.attachAgentCredential(req)
		return
	}
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
	// The attributes gosec looks for are a SERVER's Set-Cookie policy; this is
	// a client presenting a credential over loopback on a request it built
	// itself, so there is no browser to instruct and nothing to secure here
	// beyond the 0600 file the token came from.
	//nolint:gosec // G124: outbound request cookie, not a Set-Cookie policy.
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cred.Token})
}

// attachAgentCredential presents the credential the daemon minted for this
// agent's own launch, when the launch was given one.
//
// Silent about every failure for the same reason attachCredential is: an agent
// launched by a build that could not mint a credential, or one whose credential
// has been revoked because its work was closed out, must reach the daemon's own
// actionable 401 rather than fail before the request is sent.
func (c *commandContext) attachAgentCredential(req *http.Request) {
	lookup := c.deps.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	path, ok := lookup(agentcred.EnvCredentialFile)
	if !ok || strings.TrimSpace(path) == "" {
		return
	}
	file, ok, err := agentcred.Read(path)
	if err != nil || !ok {
		return
	}
	req.Header.Set(agentTokenHeader, file.Token)
}
