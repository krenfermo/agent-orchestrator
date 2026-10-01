package practical3d

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProviderProxyVersion versions the observation rules below (call-class
// derivation, outcome mapping, accounting). It is recorded in the manifest's
// instrument.provider_request_schema_version.
const ProviderProxyVersion = "ao.3d-practical.provider-proxy.v1"

// RoleResolver maps an AO usage subject to its role at a moment, from AO's
// durable control plane (usage_attribution_windows), never from agent text.
type RoleResolver interface {
	ResolveRole(ctx context.Context, subject string, at time.Time) (Role, error)
}

// ProxyObservation is what the proxy saw in one provider response: the billed
// message id and every tool_use block (id, name and, for Bash, the command).
type ProxyObservation struct {
	CallIndex int            `json:"call_index"`
	Subject   string         `json:"subject"`
	Role      Role           `json:"role"`
	MessageID string         `json:"message_id"`
	ToolUses  []ProxyToolUse `json:"tool_uses"`
	// InputTokens is the attempt's total input (for totals-based joins).
	InputTokens int64     `json:"input_tokens"`
	At          time.Time `json:"at"`
}

// ProxyToolUse is one tool call the model emitted, as the proxy saw it.
type ProxyToolUse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	// Target is the raw file/notebook/search path of a file tool, as the
	// provider returned it (3C's toolTargetOf fields).
	Target string `json:"target,omitempty"`
}

// ProviderProxy is the AO-owned provider boundary for one position
// (AO_OBSERVED_CLIENT_ONLY_V1 realized for CLI agents): agent CLIs reach the
// provider only through it (ANTHROPIC_BASE_URL), and the position sandbox
// denies every other network destination. It appends ATTEMPT_DISPATCHED
// before forwarding and ATTEMPT_FINALIZED after the response, through the
// position's ObservedClient, so retries and partial streams are attempts.
type ProviderProxy struct {
	Upstream *url.URL // Anthropic Messages origin
	OpenAI   *url.URL // OpenAI/ChatGPT Responses origin (Codex); nil disables
	// AccountRefs is the frozen per-provider account reference map whose
	// canonical digest is the manifest's provider.account_ref_sha256.
	AccountRefs map[string]string
	HTTPClient  *http.Client
	// Credentials injects the operator's provider credential into outgoing
	// requests (supervisor side); nil forwards the client's headers as-is
	// (fake-upstream tests only).
	Credentials ProviderCredentials
	Resolver    RoleResolver
	EvidenceDir string
	Now         func() time.Time

	mu           sync.Mutex
	client       *ObservedClient
	tokens       map[string]string // token -> subject
	observations []ProxyObservation
	results      map[string]bool // tool_use id -> is_error, as first sent to the provider
	conv         map[string]*convState
	ln, ctl      net.Listener
	srv, ctlSrv  *http.Server
	rejected     []string
}

// NewProviderProxy builds a proxy toward the real provider API. The HTTP
// client ignores HTTP(S)_PROXY from the environment.
func NewProviderProxy(upstream string, resolver RoleResolver, evidenceDir string) (*ProviderProxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("invalid upstream %q", upstream)
	}
	tr := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 16, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 20 * time.Second, DisableCompression: true}
	// Redirects are never followed: a followed redirect would replay the
	// request upstream behind the single dispatched attempt.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ProviderProxy{Upstream: u, HTTPClient: &http.Client{Transport: tr, CheckRedirect: noRedirect}, Resolver: resolver, EvidenceDir: evidenceDir, Now: time.Now, tokens: map[string]string{}}, nil
}

// Start listens on 127.0.0.1 (provider traffic) and on a private unix socket
// (token issuance for the trusted launch shim). It returns the provider port.
func (p *ProviderProxy) Start(controlSocket string) (int, error) {
	if err := os.MkdirAll(p.EvidenceDir, 0o700); err != nil {
		return 0, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	ctl, err := net.Listen("unix", controlSocket)
	if err != nil {
		_ = ln.Close()
		return 0, err
	}
	_ = os.Chmod(controlSocket, 0o600)
	p.ln, p.ctl = ln, ctl
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serveProvider), ReadHeaderTimeout: 30 * time.Second}
	p.ctlSrv = &http.Server{Handler: http.HandlerFunc(p.serveControl), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	go func() { _ = p.ctlSrv.Serve(ctl) }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("proxy listener has no TCP address")
	}
	return addr.Port, nil
}

// Close stops both listeners.
func (p *ProviderProxy) Close() error {
	var errs []error
	if p.srv != nil {
		errs = append(errs, p.srv.Close())
	}
	if p.ctlSrv != nil {
		errs = append(errs, p.ctlSrv.Close())
	}
	return errors.Join(errs...)
}

// Bind attaches the proxy to the running position; requests outside a
// binding are refused.
func (p *ProviderProxy) Bind(c *ObservedClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = c
}

// Unbind detaches the position and revokes every token.
func (p *ProviderProxy) Unbind() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = nil
	p.tokens = map[string]string{}
}

// ToolResults returns the tool results clients sent to the provider (tool_use
// id -> is_error): the wire's account of whether a tool call succeeded, which
// the agent cannot rewrite after the fact (its transcript it can).
func (p *ProviderProxy) ToolResults() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]bool, len(p.results))
	for k, v := range p.results {
		out[k] = v
	}
	return out
}

// Observations returns the tool/message inventory seen in responses.
func (p *ProviderProxy) Observations() []ProxyObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProxyObservation(nil), p.observations...)
}

// convState is a subject's committed primary-model conversation: the turns
// of its last successful request (without trailing system turns) and the
// tool ids that request's response issued.
type convState struct {
	core    []string
	toolIDs []string
}

// coreOf is a request's turn digests without its trailing system turns
// (Claude Code replaces its trailing reminder on every request).
func coreOf(turns []convTurn) []string {
	n := len(turns)
	for n > 0 && turns[n-1].role == "system" {
		n--
	}
	out := make([]string, n)
	for i := range out {
		out[i] = turns[i].digest
	}
	return out
}

// checkLinear enforces that a subject's primary-model conversation is one
// linear history the client extends by exactly the provider's last response:
// the committed turns are a byte-exact prefix, the new suffix's assistant
// turn issues exactly the tool ids the provider returned, and tool results
// answer only those. A side request the agent crafts on its tokenized proxy
// URL either fails this or breaks it for the client's next real request.
func (p *ProviderProxy) checkLinear(subject string, class CallClass, turns []convTurn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.conv[subject]
	if class == CallInitial || class == CallHelper {
		return nil
	}
	if st == nil {
		return fmt.Errorf("%s request with no committed conversation", class)
	}
	if len(turns) < len(st.core) {
		return errors.New("request drops committed conversation turns")
	}
	for i, d := range st.core {
		if turns[i].digest != d {
			return fmt.Errorf("request rewrites committed conversation turn %d", i)
		}
	}
	if class == CallRetry {
		return nil
	}
	issued := map[string]bool{}
	for _, id := range st.toolIDs {
		issued[id] = true
	}
	assistant, uses := 0, map[string]bool{}
	for _, t := range turns[len(st.core):] {
		if t.role == "assistant" {
			assistant++
			for _, id := range t.toolUses {
				uses[id] = true
			}
		}
		for _, id := range t.toolResults {
			if !issued[id] {
				return fmt.Errorf("tool result %s answers no tool call the provider issued", id)
			}
		}
	}
	if assistant == 0 {
		return errors.New("request does not extend the conversation by the provider's response")
	}
	if len(uses) != len(issued) {
		return errors.New("request's assistant turn differs from the provider's response")
	}
	for id := range uses {
		if !issued[id] {
			return fmt.Errorf("assistant turn carries tool call %s the provider never issued", id)
		}
	}
	return nil
}

// commitLinear records a successful primary-model request as the subject's
// conversation.
func (p *ProviderProxy) commitLinear(subject string, turns []convTurn, toolUses []ProxyToolUse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conv == nil {
		p.conv = map[string]*convState{}
	}
	st := &convState{core: coreOf(turns)}
	for _, tu := range toolUses {
		st.toolIDs = append(st.toolIDs, tu.ID)
	}
	p.conv[subject] = st
}

// recordResults keeps the first outcome the wire reported for each tool call;
// a later request contradicting it is a rewritten history.
func (p *ProviderProxy) recordResults(results map[string]bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.results == nil {
		p.results = map[string]bool{}
	}
	for id, isErr := range results {
		if prev, ok := p.results[id]; ok && prev != isErr {
			return fmt.Errorf("tool result %s changed between requests", id)
		}
		p.results[id] = isErr
	}
	return nil
}

// Rejected lists requests the proxy refused (each also malforms the position).
func (p *ProviderProxy) Rejected() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.rejected...)
}

// IssueToken binds a fresh random token to an AO usage subject.
func (p *ProviderProxy) IssueToken(subject string) (string, error) {
	if strings.TrimSpace(subject) == "" || strings.ContainsAny(subject, "\n\r") {
		return "", errors.New("invalid subject")
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		return "", errors.New("no position is running")
	}
	p.tokens[tok] = subject
	return tok, nil
}

func (p *ProviderProxy) serveControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/token" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		Subject string `json:"subject"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := p.IssueToken(req.Subject)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
}

func (p *ProviderProxy) reject(w http.ResponseWriter, c *ObservedClient, status int, reason string) {
	p.mu.Lock()
	p.rejected = append(p.rejected, reason)
	p.mu.Unlock()
	if c != nil {
		c.Violation("provider proxy refused a request: " + reason)
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "permission_error", "message": "ao 3d-practical proxy: " + reason}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

const maxRequestBody = 64 << 20

func (p *ProviderProxy) serveProvider(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	p.mu.Lock()
	c := p.client
	subject := ""
	if len(parts) == 3 && parts[0] == "t" {
		subject = p.tokens[parts[1]]
	}
	p.mu.Unlock()
	if c == nil {
		p.reject(w, nil, http.StatusServiceUnavailable, "no position bound")
		return
	}
	if subject == "" {
		p.reject(w, c, http.StatusForbidden, "request without a valid subject token")
		return
	}
	path := "/" + parts[2]
	if path == "/api/hello" && (r.Method == http.MethodHead || r.Method == http.MethodGet) {
		// Claude Code's connectivity check: answered here, never forwarded,
		// never a model call.
		w.WriteHeader(http.StatusOK)
		return
	}
	proto := protocolFor(path)
	if r.Method != http.MethodPost || proto == nil || (proto.name() == "openai" && p.OpenAI == nil) {
		// Only model inference is allowed; any other API surface (token
		// counting, models, files, batches, plugins...) is refused.
		p.reject(w, c, http.StatusForbidden, fmt.Sprintf("endpoint %s %s is not allowed", r.Method, path))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		p.reject(w, c, http.StatusRequestEntityTooLarge, "unreadable or oversized request body")
		return
	}
	parsed, err := proto.parse(body)
	if err != nil {
		p.reject(w, c, http.StatusBadRequest, "request is not a model inference request: "+err.Error())
		return
	}
	role, err := p.Resolver.ResolveRole(r.Context(), subject, p.now())
	if err != nil {
		p.reject(w, c, http.StatusForbidden, "role unresolvable from AO control plane: "+err.Error())
		return
	}
	if err := p.store(body); err != nil {
		p.reject(w, c, http.StatusInternalServerError, "evidence store: "+err.Error())
		return
	}
	baseClass := parsed.baseClass(c.m, c.p.TaskID, role)
	// An exact replay (retry) of a failed request passes against the same
	// committed conversation, since a failed attempt commits nothing.
	if err := p.checkLinear(subject, baseClass, parsed.turns); err != nil {
		p.reject(w, c, http.StatusForbidden, "conversation is not the client's linear history: "+err.Error())
		return
	}
	if err := p.recordResults(parsed.results); err != nil {
		p.reject(w, c, http.StatusForbidden, err.Error())
		return
	}
	attempt, err := c.BeginHTTPAttempt(HTTPAttemptRequest{Subject: subject, Role: role, BaseClass: baseClass, Model: parsed.model, Stream: parsed.stream, ToolNames: parsed.tools, Body: body})
	if err != nil {
		p.reject(w, nil, http.StatusServiceUnavailable, err.Error())
		return
	}
	// The frozen per-attempt deadline bounds every forwarded attempt; an
	// attempt it ends is finalized as TIMEOUT (06 §4).
	attemptCtx, cancel := context.WithTimeout(r.Context(), time.Duration(c.m.Deadlines.ProviderAttemptSeconds)*time.Second)
	res, obs := p.forward(attemptCtx, w, r, proto, path, body, parsed.stream)
	res.Expired = errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
	cancel()
	_ = attempt.Finish(res) // failures/malformations are recorded by the client
	if res.Outcome == OutcomeSuccess && baseClass != CallHelper {
		p.commitLinear(subject, parsed.turns, obs.ToolUses)
	}
	if baseClass == CallHelper && proto.name() == "openai" && len(obs.ToolUses) > 0 {
		// A Codex helper (thread title) is tool-less by construction; one
		// that issues a tool call is not what it claimed to be.
		c.Violation(fmt.Sprintf("helper call %d issued %d tool call(s)", attempt.base.CallIndex, len(obs.ToolUses)))
	}
	obs.CallIndex, obs.Subject, obs.Role, obs.At = attempt.base.CallIndex, subject, attempt.base.Role, p.now()
	if res.InputTokens != nil {
		obs.InputTokens = *res.InputTokens
	}
	p.mu.Lock()
	p.observations = append(p.observations, obs)
	p.mu.Unlock()
}

func (p *ProviderProxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *ProviderProxy) store(b []byte) error {
	path := filepath.Join(p.EvidenceDir, sha256Hex(b))
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return writeExclusive(path, b)
}

// hopHeaders are never forwarded.
var hopHeaders = map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Accept-Encoding": true, "Content-Length": true, "Host": true}

// forward sends the exact request bytes upstream and streams the response
// back while extracting outcome, accounting, the message id and tool uses.
func (p *ProviderProxy) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, proto providerProtocol, path string, body []byte, stream bool) (HTTPAttemptResult, ProxyObservation) {
	in := r.Header
	target := *p.Upstream
	if proto.name() == "openai" {
		target = *p.OpenAI
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return HTTPAttemptResult{TransportError: err.Error()}, ProxyObservation{}
	}
	// A body that cannot be rewound is never replayed by net/http (HTTP/1
	// retry on a reused connection, HTTP/2 GOAWAY retry): exactly one
	// upstream request per dispatched attempt. A replay is the client's own
	// next request, observed as a retry.
	req.GetBody = nil
	for k, vs := range in {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept-Encoding", "identity")
	// The operator's credential is added here, on the outgoing request only;
	// whatever the client sent (the agent's placeholder) is dropped.
	injectedRef := ""
	if p.Credentials != nil {
		ref, err := p.Credentials.Inject(proto.name(), req.Header)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return HTTPAttemptResult{TransportError: "provider credential: " + err.Error()}, ProxyObservation{}
		}
		injectedRef = ref
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return HTTPAttemptResult{TransportError: err.Error()}, ProxyObservation{}
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	var captured bytes.Buffer
	acc := proto.accumulator()
	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	var readErr error
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			captured.Write(line)
			if _, werr := w.Write(line); werr != nil && readErr == nil {
				readErr = werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			if stream && resp.StatusCode == http.StatusOK {
				acc.sseLine(line)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	if !stream || resp.StatusCode != http.StatusOK {
		acc.jsonBody(captured.Bytes())
	}
	_ = p.store(captured.Bytes())
	res := acc.result(resp.StatusCode, stream, readErr)
	meta := map[string]any{"http_status": fmt.Sprint(resp.StatusCode), "request_id": firstHeader(resp.Header, "Request-Id", "X-Request-Id"), "response_sha256": sha256Hex(captured.Bytes()), "proxy_version": ProviderProxyVersion, "provider_protocol": proto.name()}
	if injectedRef != "" {
		meta["account_ref_sha256"] = injectedRef
	} else if ref := proto.accountRef(in, resp.Header); ref != "" {
		meta["account_ref_sha256"] = ref
	}
	if len(p.AccountRefs) > 0 {
		meta["account_refs"] = p.AccountRefs
	}
	if readErr != nil {
		// Diagnostic only: a stream that already ended with its terminal
		// event stays whole; one that did not is a terminal failure anyway.
		meta["stream_error"] = readErr.Error()
	}
	res.ProviderMetadata, _ = json.Marshal(meta)
	res.TerminalMetadata, _ = json.Marshal(acc.terminal())
	return res, ProxyObservation{MessageID: acc.id(), ToolUses: acc.toolUses()}
}

type messagesRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// turns digests every message and records the tool ids it issues/answers.
func (m messagesRequest) turns() []convTurn {
	out := make([]convTurn, 0, len(m.Messages))
	for _, raw := range m.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &msg)
		t := convTurn{role: msg.Role, digest: turnDigest(raw)}
		if t.role != "assistant" && t.role != "system" {
			t.role = "user"
		}
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(msg.Content, &blocks) == nil {
			for _, b := range blocks {
				switch {
				case b.Type == "tool_use" && b.ID != "":
					t.toolUses = append(t.toolUses, b.ID)
				case b.Type == "tool_result" && b.ToolUseID != "":
					t.toolResults = append(t.toolResults, b.ToolUseID)
				}
			}
		}
		out = append(out, t)
	}
	return out
}

// toolResults maps each tool_result block in the conversation to whether the
// client reported it as an error.
func (m messagesRequest) toolResults() map[string]bool {
	out := map[string]bool{}
	for _, raw := range m.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				out[b.ToolUseID] = b.IsError
			}
		}
	}
	return out
}

func (m messagesRequest) toolNames() []string {
	out := make([]string, 0, len(m.Tools))
	for _, t := range m.Tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// baseClass derives the call class from the request structure (proxy rules
// v1): a model other than the role's frozen primary model is a `helper`
// call; a conversation with no assistant turn yet is `initial` (Claude Code
// may send several user blocks before the first answer); a turn answering tool_use blocks is
// `tool_result`; anything else continues the conversation. `retry` is decided
// by the ObservedClient from replay of the previous request's bytes.
func (m messagesRequest) baseClass(man Manifest, task string, role Role) CallClass {
	if cfg, ok := invocationConfig(man, task, role, CallInitial); ok && m.Model != cfg.ModelID {
		return CallHelper
	}
	assistant := false
	for _, raw := range m.Messages {
		var msg struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &msg) == nil && msg.Role == "assistant" {
			assistant = true
			break
		}
	}
	if !assistant {
		return CallInitial
	}
	// Claude Code may append a trailing system-role message (reminders);
	// the class is decided by the last conversational turn.
	var last struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	for i := len(m.Messages) - 1; i >= 0; i-- {
		last.Role, last.Content = "", nil
		if json.Unmarshal(m.Messages[i], &last) != nil || last.Role != "system" {
			break
		}
	}
	if last.Role == "user" {
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(last.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "tool_result" {
					return CallToolResult
				}
			}
		}
	}
	return CallContinuation
}

type responseAccumulator struct {
	messageID                    string
	input, cacheRead, cacheWrite *int64
	stopped                      bool
	stopReason, errType          string
	tools                        map[int]*toolAcc
	order                        []int
	// conflict: the response reported two different values for one usage
	// field, or started twice; its accounting is then MISSING (never the
	// last value seen).
	conflict bool
	starts   int
}

type toolAcc struct {
	id, name string
	input    strings.Builder
}

func newResponseAccumulator() *responseAccumulator {
	return &responseAccumulator{tools: map[int]*toolAcc{}}
}

type usageBlock struct {
	InputTokens              *int64 `json:"input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

func (a *responseAccumulator) setUsage(u *usageBlock) {
	if u == nil {
		return
	}
	set := func(dst **int64, v *int64) {
		if v == nil {
			return
		}
		if *dst != nil && **dst != *v {
			a.conflict = true
		}
		*dst = v
	}
	set(&a.input, u.InputTokens)
	set(&a.cacheWrite, u.CacheCreationInputTokens)
	set(&a.cacheRead, u.CacheReadInputTokens)
}

func (a *responseAccumulator) sseLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message *struct {
			ID    string      `json:"id"`
			Usage *usageBlock `json:"usage"`
		} `json:"message"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *usageBlock `json:"usage"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		a.starts++
		if a.starts > 1 {
			a.conflict = true
		}
		if ev.Message != nil {
			a.messageID = ev.Message.ID
			a.setUsage(ev.Message.Usage)
		}
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			a.tools[ev.Index] = &toolAcc{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			a.order = append(a.order, ev.Index)
		}
	case "content_block_delta":
		if t := a.tools[ev.Index]; t != nil && ev.Delta != nil && ev.Delta.Type == "input_json_delta" {
			t.input.WriteString(ev.Delta.PartialJSON)
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			a.stopReason = ev.Delta.StopReason
		}
		a.setUsage(ev.Usage)
	case "message_stop":
		a.stopped = true
	case "error":
		if ev.Error != nil {
			a.errType = ev.Error.Type
		}
	}
}

func (a *responseAccumulator) jsonBody(b []byte) {
	var body struct {
		ID         string      `json:"id"`
		Type       string      `json:"type"`
		StopReason string      `json:"stop_reason"`
		Usage      *usageBlock `json:"usage"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &body) != nil {
		return
	}
	if body.Type == "error" && body.Error != nil {
		a.errType = body.Error.Type
		a.setUsage(body.Usage) // taken only if the provider reports it
		return
	}
	a.messageID, a.stopReason = body.ID, body.StopReason
	a.setUsage(body.Usage)
	a.stopped = body.Type == "message"
	for i, c := range body.Content {
		if c.Type == "tool_use" {
			t := &toolAcc{id: c.ID, name: c.Name}
			t.input.Write(c.Input)
			a.tools[i] = t
			a.order = append(a.order, i)
		}
	}
}

func (a *responseAccumulator) toolUses() []ProxyToolUse {
	out := make([]ProxyToolUse, 0, len(a.order))
	for _, i := range a.order {
		t := a.tools[i]
		tu := ProxyToolUse{ID: t.id, Name: t.name}
		var in struct {
			Command      string `json:"command"`
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
			Path         string `json:"path"`
		}
		if json.Unmarshal([]byte(t.input.String()), &in) == nil {
			switch t.name {
			case "Bash":
				tu.Command = in.Command
			case "Read", "Edit", "MultiEdit", "Write":
				tu.Target = in.FilePath
			case "NotebookRead", "NotebookEdit":
				tu.Target = in.NotebookPath
			case "Grep", "Glob", "LS":
				tu.Target = in.Path
			}
		}
		out = append(out, tu)
	}
	return out
}

// result maps the observed HTTP exchange onto the request-outcome and
// accounting rules of proxy v1: a provider rejection with a definite HTTP
// status processed no input (explicit zero); a 2xx response must report
// usage or it is MISSING; a 2xx stream that ends before message_stop is a
// partial, non-replayable TERMINAL_PROVIDER_FAILURE that still counts.
func (a *responseAccumulator) result(status int, stream bool, _ error) HTTPAttemptResult {
	var res HTTPAttemptResult
	switch {
	// Once message_stop arrived the response is whole (see Responses).
	case status == http.StatusOK && a.stopped && a.errType == "":
		res.Outcome = OutcomeSuccess
	case status == http.StatusOK && (a.errType == "overloaded_error" || a.errType == "api_error"):
		res.Outcome = OutcomeRetryable
	case status == http.StatusOK:
		res.Outcome = OutcomeTerminalFailure
	case status == http.StatusTooManyRequests || a.errType == "rate_limit_error":
		res.Outcome = OutcomeRateLimited
	case status == 529 || status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		res.Outcome = OutcomeRetryable
	default:
		res.Outcome = OutcomeTerminalFailure
	}
	// 3d-practical §accounting: a provider response without usage (error
	// responses included) is MISSING -- the position becomes
	// MALFORMED_RESULT -- and absent cache fields are never imputed as zero.
	if a.input == nil || a.cacheRead == nil || a.cacheWrite == nil || a.conflict {
		return res
	}
	read, write := *a.cacheRead, *a.cacheWrite
	total, uncached := *a.input+read+write, *a.input+write
	res.InputTokens, res.CachedInputTokens, res.UncachedInputTokens = &total, &read, &uncached
	return res
}
