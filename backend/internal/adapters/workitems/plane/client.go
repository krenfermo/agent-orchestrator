package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// client.go — the bounded HTTP client (P4-E §4).

const (
	// DefaultBaseURL is Plane Cloud. A self-hosted installation configures its
	// own origin.
	DefaultBaseURL = "https://api.plane.so"
	// apiPrefix is appended by AO rather than configured, so an operator
	// cannot half-configure the version.
	apiPrefix = "/api/v1"

	defaultUserAgent = "ao-agent-orchestrator/workitems-plane"

	// DefaultTimeout bounds one request.
	//
	// It is short because every caller is either a person waiting on a button
	// or a background worker with other work to do. Plane being slow must cost
	// AO one deferred sync, never a held goroutine — which is the §4 rule that
	// Plane latency may not block AO indefinitely.
	DefaultTimeout = 15 * time.Second

	// pageSize is Plane's documented maximum.
	pageSize = 100
	// maxPages bounds a paginated read. At the max page size this still
	// enumerates 5,000 items before failing loud, which is far past any
	// workspace a person maps one AO project onto.
	maxPages = 50

	// maxErrorBody bounds how much of a provider error body is kept. Enough to
	// carry Plane's own message, short enough that a misconfigured server
	// returning an HTML page does not land in a log.
	maxErrorBody = 512
	// maxDescription bounds the plain-text description AO projects out of a
	// work item. A body is context, not a document.
	maxDescription = 8 * 1024
)

// TokenSource yields a Plane API token on demand.
//
// It is an interface rather than a string for the same reason the GitHub
// adapter's is: the token may come from an env var, from an encrypted settings
// row, or from a test literal, and the client should not learn which.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a literal token.
type StaticToken string

// Token returns the literal token.
func (s StaticToken) Token(context.Context) (string, error) {
	if strings.TrimSpace(string(s)) == "" {
		return "", &ports.WorkItemsError{Op: "token", Kind: ports.WorkItemsErrNotConfigured,
			Message: "no Plane API token is configured"}
	}
	return string(s), nil
}

// Options configures a Client.
type Options struct {
	// BaseURL is the Plane origin, without the /api/v1 prefix. Empty means
	// Plane Cloud.
	BaseURL string
	// Workspace is the workspace slug every request is scoped by. Required.
	Workspace string
	// Token yields the API key. Required.
	Token TokenSource
	// HTTPClient is injected by tests; production passes nil and gets a client
	// with DefaultTimeout.
	HTTPClient *http.Client
	UserAgent  string
	// Now is injected by tests that assert on retry timing.
	Now func() time.Time
}

// Client implements ports.WorkItems against Plane.
type Client struct {
	http      *http.Client
	base      string
	workspace string
	token     TokenSource
	userAgent string
	now       func() time.Time
}

// New builds a client. It performs no network call and no token read:
// construction must be free, because the daemon constructs one per configured
// project at wiring time and a token read that hit the disk (or failed) there
// would turn a misconfiguration into a boot failure.
func New(opts Options) (*Client, error) {
	ws := strings.Trim(strings.TrimSpace(opts.Workspace), "/")
	if ws == "" {
		return nil, &ports.WorkItemsError{Op: "new", Kind: ports.WorkItemsErrNotConfigured,
			Message: "no Plane workspace is configured"}
	}
	if opts.Token == nil {
		return nil, &ports.WorkItemsError{Op: "new", Kind: ports.WorkItemsErrNotConfigured,
			Message: "no Plane API token is configured"}
	}
	base, err := NormalizeBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		http: httpClient, base: base, workspace: ws,
		token: opts.Token, userAgent: ua, now: now,
	}, nil
}

// NormalizeBaseURL validates and canonicalises a configured origin.
//
// It rejects anything that is not http(s) and strips a trailing /api/v1 an
// operator may have pasted from the documentation — that specific paste is the
// most likely configuration mistake, and silently producing /api/v1/api/v1
// would fail with a 404 that looks like a permissions problem.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", &ports.WorkItemsError{Op: "config", Kind: ports.WorkItemsErrInvalid,
			Message: "Plane base URL is not a URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", &ports.WorkItemsError{Op: "config", Kind: ports.WorkItemsErrInvalid,
			Message: "Plane base URL must be http or https"}
	}
	if u.Host == "" {
		return "", &ports.WorkItemsError{Op: "config", Kind: ports.WorkItemsErrInvalid,
			Message: "Plane base URL has no host"}
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), apiPrefix)
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// Workspace reports the slug this client is scoped to.
func (c *Client) Workspace() string { return c.workspace }

// BaseURL reports the normalized origin, for display in settings.
func (c *Client) BaseURL() string { return c.base }

// do performs one request and decodes a JSON response into out.
//
// Every failure is classified here, once, while the HTTP status is still in
// scope — which is what lets the sync worker decide retryability without
// matching on message text.
func (c *Client) do(ctx context.Context, op, method, path string, query url.Values, body, out any) error {
	token, err := c.token.Token(ctx)
	if err != nil {
		return err
	}

	endpoint := c.base + apiPrefix + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, mErr := json.Marshal(body)
		if mErr != nil {
			return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrInvalid,
				Message: "request could not be encoded", Err: mErr}
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrInvalid,
			Message: "request could not be built", Err: err}
	}
	// The one place the credential is used. It is set as a header and is never
	// placed in the URL, so it cannot reach a log through a recorded endpoint.
	req.Header.Set("X-API-Key", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A cancelled context is the caller giving up, not the provider
		// failing; reporting it as unavailable would make a shutdown look like
		// an outage in the audit trail.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrUnavailable,
				Message: "request deadline exceeded", Err: err}
		}
		return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrUnavailable,
			Message: "Plane could not be reached", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		return c.classify(op, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if decErr := json.NewDecoder(resp.Body).Decode(out); decErr != nil {
		return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrUnavailable,
			Message: "Plane returned a response AO could not decode", Err: decErr}
	}
	return nil
}

// classify turns a non-2xx response into a typed error.
func (c *Client) classify(op string, resp *http.Response) *ports.WorkItemsError {
	msg := errorMessage(resp.Body)
	e := &ports.WorkItemsError{Op: op, Status: resp.StatusCode, Message: msg}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = ports.WorkItemsErrAuth
		// Plane distinguishes a bad key from a key without access to this
		// workspace only in the body, and the body is not stable enough to
		// branch on. Both are terminal for a background sync either way.
		if msg == "" {
			e.Message = "Plane rejected the API token"
		}
	case resp.StatusCode == http.StatusNotFound:
		e.Kind = ports.WorkItemsErrNotFound
		if msg == "" {
			e.Message = "not found in Plane"
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = ports.WorkItemsErrRateLimited
		if msg == "" {
			e.Message = "Plane rate limit reached"
		}
	case resp.StatusCode >= 500:
		e.Kind = ports.WorkItemsErrUnavailable
		if msg == "" {
			e.Message = "Plane returned a server error"
		}
	default:
		e.Kind = ports.WorkItemsErrInvalid
		if msg == "" {
			e.Message = "Plane refused the request"
		}
	}
	return e
}

// RetryAfter reports how long to wait before retrying, from Plane's own
// rate-limit headers. It is exported so the sync worker can honour the
// provider's hint rather than guessing.
//
// X-RateLimit-Reset is UTC epoch seconds. A reset in the past, or a header AO
// cannot parse, yields false and the caller falls back to its own backoff —
// never to zero, which would turn a rate limit into a hot loop.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	reset := strings.TrimSpace(h.Get("X-RateLimit-Reset"))
	if reset == "" {
		return 0, false
	}
	epoch, err := strconv.ParseInt(reset, 10, 64)
	if err != nil {
		return 0, false
	}
	d := time.Unix(epoch, 0).UTC().Sub(now.UTC())
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// errorMessage extracts Plane's own explanation from an error body, bounded.
//
// Plane returns {"error": "..."} for most refusals and DRF's field-keyed
// {"field": ["..."]} for validation failures, so both shapes are tried before
// falling back to the raw text.
func errorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) == nil {
		for _, key := range []string{"error", "detail", "message"} {
			if v, ok := envelope[key]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return truncate(s, 200)
				}
			}
		}
		// A DRF validation error: the first field's first message is the most
		// useful thing a person can act on.
		for field, v := range envelope {
			var msgs []string
			if json.Unmarshal(v, &msgs) == nil && len(msgs) > 0 {
				return truncate(field+": "+msgs[0], 200)
			}
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 200)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// paginate walks a cursor-paginated collection, calling visit for each page's
// raw results until the provider says there is no next page or the ceiling
// binds.
func (c *Client) paginate(ctx context.Context, op, path string, query url.Values, visit func(json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", strconv.Itoa(pageSize))

	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrUnavailable,
				Message: "cancelled while paginating", Err: err}
		}
		var envelope pageEnvelope
		if err := c.do(ctx, op, http.MethodGet, path, query, nil, &envelope); err != nil {
			return err
		}
		if err := visit(envelope.Results); err != nil {
			return err
		}
		if !envelope.NextPageResults || strings.TrimSpace(envelope.NextCursor) == "" {
			return nil
		}
		query.Set("cursor", envelope.NextCursor)
	}
	return &ports.WorkItemsError{Op: op, Kind: ports.WorkItemsErrUnavailable,
		Message: fmt.Sprintf("Plane returned more than %d pages", maxPages)}
}

// pageEnvelope is Plane's cursor-pagination wrapper.
type pageEnvelope struct {
	NextCursor      string          `json:"next_cursor"`
	PrevCursor      string          `json:"prev_cursor"`
	NextPageResults bool            `json:"next_page_results"`
	Count           int             `json:"count"`
	TotalPages      int             `json:"total_pages"`
	Results         json.RawMessage `json:"results"`
}

// Compile-time proof the client satisfies the port.
var _ ports.WorkItems = (*Client)(nil)
