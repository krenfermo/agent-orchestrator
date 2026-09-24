package webdast

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// checks.go — the closed set of safe, non-destructive, reproducible checks.
//
// Every check is read-only: a GET, an OPTIONS, a TRACE (which only echoes the
// request), or a header inspection. None sends an injection payload, none
// mutates state, none brute-forces anything. A finding is a rule and a location
// plus the minimal request that demonstrates it — never exfiltrated data.

// checkName identifies a check in coverage.
type checkName = string

const (
	checkSecurityHeaders checkName = "security-headers"
	checkCookies         checkName = "cookies"
	checkCORS            checkName = "cors"
	checkHTTPMethods     checkName = "http-methods"
	checkRedirects       checkName = "insecure-redirects"
	checkSensitivePaths  checkName = "sensitive-path-exposure"
	checkInfoDisclosure  checkName = "info-disclosure"
	checkBoundary        checkName = "egress-boundary-selfcheck"
)

// notAttempted names what this checker deliberately does NOT do in 2F, so an
// empty findings list is never read as a clean bill of health.
var notAttempted = []string{
	"active-injection (SQLi/XSS with executable payloads)",
	"fuzzing",
	"brute-force / credential-stuffing",
	"denial-of-service / load / stress",
	"deep TLS cipher/cert inspection (proxy CONNECT tunnel hides the handshake)",
	"authenticated authorization testing (no controlled fixture credentials supplied)",
}

// runChecks runs every check and returns the findings plus the checks that ran.
func runChecks(ctx context.Context, c *client, cfg Config) ([]Finding, []checkName) {
	var findings []Finding
	var ran []checkName
	add := func(name checkName, fs []Finding) {
		ran = append(ran, name)
		findings = append(findings, fs...)
	}

	base, _ := cfg.resolve("/")

	root, _ := c.get(ctx, base)
	add(checkSecurityHeaders, checkSecurityHeadersOn(cfg, base, root))
	add(checkCookies, checkCookiesOn(base, root))
	add(checkCORS, checkCORSOn(ctx, c, base))
	add(checkHTTPMethods, checkMethodsOn(ctx, c, base))
	add(checkRedirects, checkRedirectsOn(ctx, c, cfg))
	add(checkSensitivePaths, checkSensitiveOn(ctx, c, cfg))
	add(checkInfoDisclosure, checkInfoOn(base, root))
	runBoundarySelfCheck(ctx, c, cfg)
	ran = append(ran, checkBoundary)

	return findings, ran
}

func checkSecurityHeadersOn(cfg Config, endpoint string, r *response) []Finding {
	if r == nil {
		return nil
	}
	var out []Finding
	miss := func(header, title, sev, rec string) {
		if r.Header.Get(header) == "" {
			out = append(out, Finding{
				Severity: sev, Confidence: "confirmed", Check: checkSecurityHeaders,
				Title: title, Endpoint: endpoint, Method: http.MethodGet, Recommendation: rec,
				Reproduction: &Reproduction{Steps: []string{"GET " + endpoint, "response is missing the " + header + " header"}},
			})
		}
	}
	if cfg.Target.Scheme == "https" {
		miss("Strict-Transport-Security", "HSTS header missing", "medium",
			"Send Strict-Transport-Security with a long max-age on all HTTPS responses.")
	}
	miss("Content-Security-Policy", "Content-Security-Policy header missing", "medium",
		"Define a Content-Security-Policy to constrain script and resource origins.")
	miss("X-Content-Type-Options", "X-Content-Type-Options header missing", "low",
		"Send X-Content-Type-Options: nosniff.")
	miss("X-Frame-Options", "Clickjacking protection missing", "low",
		"Send X-Frame-Options: DENY or a frame-ancestors CSP directive.")
	miss("Referrer-Policy", "Referrer-Policy header missing", "info",
		"Send a Referrer-Policy such as strict-origin-when-cross-origin.")
	return out
}

func checkCookiesOn(endpoint string, r *response) []Finding {
	if r == nil {
		return nil
	}
	var out []Finding
	for _, sc := range r.Header.Values("Set-Cookie") {
		name := cookieName(sc)
		low := strings.ToLower(sc)
		var missing []string
		if !strings.Contains(low, "secure") {
			missing = append(missing, "Secure")
		}
		if !strings.Contains(low, "httponly") {
			missing = append(missing, "HttpOnly")
		}
		if !strings.Contains(low, "samesite") {
			missing = append(missing, "SameSite")
		}
		if len(missing) == 0 {
			continue
		}
		out = append(out, Finding{
			Severity: cookieSeverity(missing), Confidence: "confirmed", Check: checkCookies,
			Title:    fmt.Sprintf("Cookie %q missing %s", name, strings.Join(missing, "/")),
			Endpoint: endpoint, Method: http.MethodGet,
			Recommendation: "Set the " + strings.Join(missing, ", ") + " attribute(s) on this cookie.",
			// The value is left to the daemon's redactor; the step keeps the
			// name and the flags, not the value.
			Reproduction: &Reproduction{Steps: []string{"GET " + endpoint, "Set-Cookie: " + sc}},
		})
	}
	return out
}

func cookieSeverity(missing []string) string {
	for _, m := range missing {
		if m == "HttpOnly" || m == "Secure" {
			return "medium"
		}
	}
	return "low"
}

func cookieName(setCookie string) string {
	if i := strings.IndexByte(setCookie, '='); i > 0 {
		return strings.TrimSpace(setCookie[:i])
	}
	return "(unnamed)"
}

func checkCORSOn(ctx context.Context, c *client, endpoint string) []Finding {
	probeOrigin := "https://evil.example"
	r, err := c.do(ctx, http.MethodGet, endpoint, http.Header{"Origin": []string{probeOrigin}})
	if err != nil || r == nil {
		return nil
	}
	acao := r.Header.Get("Access-Control-Allow-Origin")
	acac := strings.EqualFold(r.Header.Get("Access-Control-Allow-Credentials"), "true")
	var out []Finding
	switch {
	case acao == "*" && acac:
		out = append(out, Finding{
			Severity: "high", Confidence: "confirmed", Check: checkCORS,
			Title:    "CORS allows any origin with credentials",
			Endpoint: endpoint, Method: http.MethodGet,
			Recommendation: "Never combine Access-Control-Allow-Origin: * with Allow-Credentials: true; allowlist explicit origins.",
			Reproduction:   &Reproduction{Steps: []string{"GET " + endpoint + " with Origin: " + probeOrigin, "response: Access-Control-Allow-Origin: *, Access-Control-Allow-Credentials: true"}},
		})
	case acao == probeOrigin:
		out = append(out, Finding{
			Severity: "medium", Confidence: "confirmed", Check: checkCORS,
			Title:    "CORS reflects an arbitrary Origin",
			Endpoint: endpoint, Method: http.MethodGet,
			Recommendation: "Do not reflect the request Origin; match it against an allowlist.",
			Reproduction:   &Reproduction{Steps: []string{"GET " + endpoint + " with Origin: " + probeOrigin, "response: Access-Control-Allow-Origin: " + probeOrigin}},
		})
	}
	return out
}

func checkMethodsOn(ctx context.Context, c *client, endpoint string) []Finding {
	var out []Finding
	// TRACE only echoes the request back; enabling it is a misconfiguration
	// (Cross-Site Tracing) but the request itself changes nothing.
	if r, err := c.do(ctx, http.MethodTrace, endpoint, nil); err == nil && r != nil && r.Status == http.StatusOK {
		out = append(out, Finding{
			Severity: "low", Confidence: "confirmed", Check: checkHTTPMethods,
			Title:    "HTTP TRACE method enabled",
			Endpoint: endpoint, Method: http.MethodTrace,
			Recommendation: "Disable the TRACE method at the server or proxy.",
			Reproduction:   &Reproduction{Steps: []string{"TRACE " + endpoint, fmt.Sprintf("response status %d", r.Status)}},
		})
	}
	return out
}

func checkRedirectsOn(ctx context.Context, c *client, cfg Config) []Finding {
	var out []Finding
	base, _ := cfg.resolve("/")
	r, err := c.do(ctx, http.MethodGet, base, nil)
	if err != nil || r == nil {
		return nil
	}
	if r.Status >= 300 && r.Status < 400 {
		loc := r.Header.Get("Location")
		if strings.HasPrefix(strings.ToLower(loc), "http://") && cfg.Target.Scheme == "https" {
			out = append(out, Finding{
				Severity: "medium", Confidence: "confirmed", Check: checkRedirects,
				Title:    "Redirect downgrades HTTPS to HTTP",
				Endpoint: base, Method: http.MethodGet,
				Recommendation: "Never redirect from HTTPS to HTTP; keep the scheme or upgrade it.",
				Reproduction:   &Reproduction{Steps: []string{"GET " + base, fmt.Sprintf("%d redirect to %s", r.Status, loc)}},
			})
		}
	}
	return out
}

func checkSensitiveOn(ctx context.Context, c *client, cfg Config) []Finding {
	var out []Finding
	for _, p := range sensitivePaths {
		abs, err := cfg.resolve(p)
		if err != nil {
			continue
		}
		r, err := c.get(ctx, abs)
		if err != nil || r == nil {
			continue
		}
		if r.Status == http.StatusOK && len(r.Body) > 0 {
			out = append(out, Finding{
				Severity: "high", Confidence: "probable", Check: checkSensitivePaths,
				Title:    fmt.Sprintf("Sensitive path %s is readable", p),
				Endpoint: abs, Method: http.MethodGet,
				Recommendation: "Block access to this path at the server; it should never be web-reachable.",
				// Status and length only. The contents are never stored.
				Reproduction: &Reproduction{Steps: []string{"GET " + abs, fmt.Sprintf("response status 200, %d bytes (contents not recorded)", len(r.Body))}},
			})
		}
	}
	return out
}

func checkInfoOn(endpoint string, r *response) []Finding {
	if r == nil {
		return nil
	}
	server := r.Header.Get("Server")
	// A version in the Server banner is information disclosure. Presence of a
	// digit after a slash is the heuristic.
	if server != "" && strings.ContainsAny(server, "0123456789") && strings.Contains(server, "/") {
		return []Finding{{
			Severity: "info", Confidence: "confirmed", Check: checkInfoDisclosure,
			Title:    "Server version disclosed in banner",
			Endpoint: endpoint, Method: http.MethodGet,
			Recommendation: "Suppress the version in the Server header.",
			Reproduction:   &Reproduction{Steps: []string{"GET " + endpoint, "Server: " + server}},
		}}
	}
	return nil
}

// runBoundarySelfCheck deliberately attempts destinations OFF the authorized
// target — the cloud metadata endpoint and an admin sibling of the target — to
// demonstrate that the egress boundary blocks them. It expects every attempt to
// fail; a success would be caught by the daemon's evidence check, not here. The
// attempts are recorded in the report as proof the boundary is enforced.
func runBoundarySelfCheck(ctx context.Context, c *client, cfg Config) {
	offTarget := []string{
		"http://169.254.169.254/latest/meta-data/",
		cfg.Target.Scheme + "://admin." + cfg.Target.Host + "/",
	}
	for _, dest := range offTarget {
		r, _ := c.do(ctx, http.MethodGet, dest, nil)
		if r == nil {
			// A transport error against an off-target destination is the proxy
			// refusing the CONNECT (or the topology refusing the route); do()
			// already recorded it as a blocked attempt.
			continue
		}
		// A forward proxy denies an off-allowlist HTTP request with a 4xx/5xx
		// FROM THE PROXY (typically 403). That is the boundary blocking it, not
		// a reach — record it as blocked. Only a 2xx/3xx means something
		// actually answered as the destination, which is a boundary FAILURE.
		if r.Status >= 200 && r.Status < 400 {
			c.recordRuntimeError("egress boundary did not block " + redactURL(dest) + fmt.Sprintf(" (status %d)", r.Status))
			continue
		}
		c.recordBlocked(dest, fmt.Sprintf("proxy_denied_%d", r.Status))
	}
}
