package webdast

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrRequestBudget is returned once a run has made its maximum number of
// requests. It is not an error in the run — it is the limit working — so the
// caller records it as a limitation and stops, rather than failing.
var ErrRequestBudget = errors.New("webdast: request budget exhausted")

// response is the bounded result of one request: status, headers, and at most
// MaxResponseBytes of body. The body is kept only long enough for a check to
// look at its shape; it is never placed in the report.
type response struct {
	Status     int
	Header     http.Header
	Body       []byte
	BodyTrunc  bool
	FinalURL   string
	DurationMs int64
}

// client is the checker's one way to reach the target. It goes through the
// egress proxy (HTTP_PROXY/HTTPS_PROXY), never follows a redirect, enforces the
// request budget and the rate limit, and records every attempt or redirect the
// boundary blocked. It is the only component that makes a network call.
type client struct {
	http    *http.Client
	target  Target
	limits  Limits
	minGap  time.Duration
	baseURL *url.URL

	mu               sync.Mutex
	requests         int
	nextAt           time.Time
	blockedAttempts  map[string]*BlockedAttempt
	blockedRedirects []BlockedRedirect
	runtimeErrors    []string
}

func newClient(cfg Config, limits Limits) (*client, error) {
	base, err := url.Parse(cfg.Target.BaseURL())
	if err != nil {
		return nil, err
	}
	c := &client{
		target:          cfg.Target,
		limits:          limits,
		baseURL:         base,
		blockedAttempts: map[string]*BlockedAttempt{},
	}
	if limits.RequestsPerSecond > 0 {
		c.minGap = time.Second / time.Duration(limits.RequestsPerSecond)
	}
	// The redirect policy is the boundary made explicit in the client too: a
	// Location off the authorized target is recorded and NOT followed, and no
	// redirect is ever followed (MaxRedirects is 0 in 2F). The proxy would
	// refuse an off-target CONNECT anyway; recording it here makes the report
	// say so even when the target itself is the one issuing the redirect.
	c.http = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// A bounded dialer so a single off-target probe (the boundary
			// self-check) cannot hang the whole run; the real target is local
			// and the proxy refuses an off-target CONNECT immediately.
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 15 * time.Second,
			}).DialContext,
			DisableKeepAlives:     false,
			MaxIdleConns:          8,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			from := via[len(via)-1].URL.String()
			to := req.URL.String()
			reason := "redirects_not_followed"
			if !c.onTarget(req.URL) {
				reason = "out_of_scope"
			}
			c.mu.Lock()
			c.blockedRedirects = append(c.blockedRedirects, BlockedRedirect{From: from, To: to, Reason: reason})
			c.mu.Unlock()
			return http.ErrUseLastResponse
		},
		Timeout: 20 * time.Second,
	}
	return c, nil
}

// onTarget reports whether u is exactly the authorized scheme+host+port.
func (c *client) onTarget(u *url.URL) bool {
	if !strings.EqualFold(u.Scheme, c.target.Scheme) {
		return false
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.EqualFold(host, c.target.Host) && port == fmt.Sprintf("%d", c.target.Port)
}

// get is the only request verb the safe checks need beyond an explicit method.
func (c *client) get(ctx context.Context, absURL string) (*response, error) {
	return c.do(ctx, http.MethodGet, absURL, nil)
}

// do makes one bounded request, honoring the budget and the rate limit. It
// records a blocked attempt when the boundary refuses the connection, and
// returns ErrRequestBudget once the run's request cap is reached.
func (c *client) do(ctx context.Context, method, absURL string, header http.Header) (*response, error) {
	if err := c.reserve(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, absURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ao-web-dast/1 (+authorized-pentest)")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		// A transport error against a destination the proxy refuses is the
		// boundary working. Record it as a blocked attempt rather than a run
		// failure, so the report proves what could not be reached.
		if u, perr := url.Parse(absURL); perr == nil && !c.onTarget(u) {
			c.recordBlocked(absURL, classifyBlock(err))
			return nil, nil
		}
		c.recordRuntimeError(fmt.Sprintf("%s %s: %v", method, redactURL(absURL), err))
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, c.limits.MaxResponseBytes+1)
	body, _ := io.ReadAll(limited)
	trunc := int64(len(body)) > c.limits.MaxResponseBytes
	if trunc {
		body = body[:c.limits.MaxResponseBytes]
	}
	return &response{
		Status:     resp.StatusCode,
		Header:     resp.Header,
		Body:       body,
		BodyTrunc:  trunc,
		FinalURL:   resp.Request.URL.String(),
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// reserve enforces the request budget and the rate limit before a request is
// built. It blocks until the next slot or the context ends.
func (c *client) reserve(ctx context.Context) error {
	c.mu.Lock()
	if c.requests >= c.limits.MaxRequests {
		c.mu.Unlock()
		return ErrRequestBudget
	}
	c.requests++
	var wait time.Duration
	now := time.Now()
	if c.minGap > 0 {
		if c.nextAt.IsZero() || now.After(c.nextAt) {
			c.nextAt = now.Add(c.minGap)
		} else {
			wait = c.nextAt.Sub(now)
			c.nextAt = c.nextAt.Add(c.minGap)
		}
	}
	c.mu.Unlock()
	if wait <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func (c *client) recordBlocked(dest, reason string) {
	key := dest + "\x00" + reason
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.blockedAttempts[key]; ok {
		b.Count++
		return
	}
	c.blockedAttempts[key] = &BlockedAttempt{Destination: redactURL(dest), Reason: reason, Count: 1}
}

func (c *client) recordRuntimeError(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.runtimeErrors) < 50 {
		c.runtimeErrors = append(c.runtimeErrors, msg)
	}
}

func (c *client) requestsMade() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *client) blocked() ([]BlockedAttempt, []BlockedRedirect, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	attempts := make([]BlockedAttempt, 0, len(c.blockedAttempts))
	for _, b := range c.blockedAttempts {
		attempts = append(attempts, *b)
	}
	redirs := append([]BlockedRedirect(nil), c.blockedRedirects...)
	errs := append([]string(nil), c.runtimeErrors...)
	return attempts, redirs, errs
}

// classifyBlock maps a transport error against an off-target destination to a
// stable reason for the report.
func classifyBlock(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "forbidden") || strings.Contains(s, "403"):
		return "proxy_denied"
	case strings.Contains(s, "refused"):
		return "connection_refused"
	case strings.Contains(s, "no route") || strings.Contains(s, "unreachable"):
		return "network_unreachable"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return "timeout"
	default:
		return "blocked"
	}
}

// redactURL removes any userinfo from a URL before it enters the report. The
// daemon redacts again, but the checker should not hand up a credential in the
// first place.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User != nil {
		u.User = url.UserPassword("[REDACTED]", "")
	}
	return u.String()
}
