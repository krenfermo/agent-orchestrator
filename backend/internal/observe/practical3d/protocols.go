package practical3d

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
)

// providerProtocol is one model-inference wire protocol the proxy observes.
type providerProtocol interface {
	name() string
	parse(body []byte) (parsedRequest, error)
	accumulator() streamAccumulator
	accountRef(reqHeader, respHeader http.Header) string
}

// streamAccumulator extracts outcome, accounting and tool calls from a
// response while it streams through the proxy.
type streamAccumulator interface {
	sseLine(line []byte)
	jsonBody(b []byte)
	result(status int, stream bool, readErr error) HTTPAttemptResult
	id() string
	toolUses() []ProxyToolUse
	terminal() map[string]any
}

type parsedRequest struct {
	model     string
	stream    bool
	tools     []string
	baseClass func(m Manifest, task string, role Role) CallClass
}

// protocolFor maps a request path to its protocol; nil means not inference.
func protocolFor(path string) providerProtocol {
	switch path {
	case "/v1/messages":
		return anthropicProtocol{}
	case "/backend-api/codex/responses", "/v1/responses":
		return openaiProtocol{}
	}
	return nil
}

func firstHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// primaryModelFor is the frozen model of a role's initial cell.
func primaryModelFor(m Manifest, task string, role Role) string {
	cfg, _ := invocationConfig(m, task, role, CallInitial)
	return cfg.ModelID
}

// --- Anthropic Messages (Claude Code) -------------------------------------

type anthropicProtocol struct{}

func (anthropicProtocol) name() string { return "anthropic" }

func (anthropicProtocol) parse(body []byte) (parsedRequest, error) {
	var r messagesRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Model == "" {
		return parsedRequest{}, errors.New("not a Messages API request")
	}
	return parsedRequest{model: r.Model, stream: r.Stream, tools: r.toolNames(), baseClass: r.baseClass}, nil
}

func (anthropicProtocol) accumulator() streamAccumulator { return newResponseAccumulator() }

func (anthropicProtocol) accountRef(_, resp http.Header) string {
	if org := resp.Get("Anthropic-Organization-Id"); org != "" {
		return sha256Hex([]byte(org))
	}
	return ""
}

func (a *responseAccumulator) id() string { return a.messageID }

func (a *responseAccumulator) terminal() map[string]any {
	return map[string]any{"stop_reason": a.stopReason, "message_stop": a.stopped, "error_type": a.errType}
}

// --- OpenAI Responses (Codex CLI over the ChatGPT backend) ---------------

type openaiProtocol struct{}

func (openaiProtocol) name() string { return "openai" }

type responsesRequest struct {
	Model  string            `json:"model"`
	Stream bool              `json:"stream"`
	Input  []json.RawMessage `json:"input"`
	Tools  []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tools"`
}

func (openaiProtocol) parse(body []byte) (parsedRequest, error) {
	var r responsesRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Model == "" {
		return parsedRequest{}, errors.New("not a Responses API request")
	}
	tools := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		name := t.Name
		if name == "" {
			name = t.Type
		}
		tools = append(tools, name)
	}
	sort.Strings(tools)
	return parsedRequest{model: r.Model, stream: r.Stream, tools: tools, baseClass: r.baseClass}, nil
}

// baseClass for Responses (proxy rules v1): a model other than the role's
// frozen primary model is `helper`; no assistant/tool item yet is `initial`;
// a last item answering a tool call is `tool_result`; else `continuation`.
func (r responsesRequest) baseClass(m Manifest, task string, role Role) CallClass {
	if r.Model != primaryModelFor(m, task, role) {
		return CallHelper
	}
	prior := false
	var lastType string
	for _, raw := range r.Input {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		lastType = item.Type
		if item.Role == "assistant" || strings.HasSuffix(item.Type, "_call") || strings.HasSuffix(item.Type, "_call_output") || item.Type == "reasoning" {
			prior = true
		}
	}
	switch {
	case !prior:
		return CallInitial
	case strings.HasSuffix(lastType, "_call_output"):
		return CallToolResult
	}
	return CallContinuation
}

func (openaiProtocol) accumulator() streamAccumulator { return &responsesAccumulator{} }

// accountRef hashes the ChatGPT account the Codex CLI authenticates as.
func (openaiProtocol) accountRef(req, _ http.Header) string {
	if id := req.Get("Chatgpt-Account-Id"); id != "" {
		return sha256Hex([]byte(id))
	}
	return ""
}

type responsesAccumulator struct {
	respID          string
	input, cached   *int64
	completed       bool
	status, errType string
	tools           []ProxyToolUse
	// errEnvelope: the body is the backend's own error object (OpenAI
	// `error`, or the ChatGPT backend's `detail`).
	errEnvelope bool
}

type responsesUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type responsesItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Action    *struct {
		Command []string `json:"command"`
	} `json:"action"`
}

func (a *responsesAccumulator) setResponse(resp *struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Usage  *responsesUsage `json:"usage"`
}) {
	if resp == nil {
		return
	}
	if resp.ID != "" {
		a.respID = resp.ID
	}
	if resp.Status != "" {
		a.status = resp.Status
	}
	if u := resp.Usage; u != nil && u.InputTokens != nil {
		a.input = u.InputTokens
		zero := int64(0)
		a.cached = &zero
		if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
			a.cached = u.InputTokensDetails.CachedTokens
		}
	}
}

func (a *responsesAccumulator) addItem(it *responsesItem) {
	if it == nil {
		return
	}
	switch it.Type {
	case "function_call":
		tu := ProxyToolUse{ID: firstNonEmptyStr(it.CallID, it.ID), Name: it.Name}
		var args struct {
			Command any `json:"command"`
			Cmd     any `json:"cmd"`
		}
		if json.Unmarshal([]byte(it.Arguments), &args) == nil {
			tu.Command = commandText(firstNonNil(args.Command, args.Cmd))
		}
		a.tools = append(a.tools, tu)
	case "local_shell_call":
		tu := ProxyToolUse{ID: firstNonEmptyStr(it.CallID, it.ID), Name: "local_shell"}
		if it.Action != nil {
			tu.Command = strings.Join(it.Action.Command, " ")
		}
		a.tools = append(a.tools, tu)
	}
}

func (a *responsesAccumulator) sseLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Usage  *responsesUsage `json:"usage"`
		} `json:"response"`
		Item  *responsesItem `json:"item"`
		Error *struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev) != nil {
		return
	}
	switch ev.Type {
	case "response.created", "response.in_progress":
		a.setResponse(ev.Response)
	case "response.output_item.done":
		a.addItem(ev.Item)
	case "response.completed":
		a.setResponse(ev.Response)
		a.completed = true
	case "response.failed", "response.incomplete":
		a.setResponse(ev.Response)
		a.errType = ev.Type
	case "error":
		if ev.Error != nil {
			a.errType = firstNonEmptyStr(ev.Error.Code, ev.Error.Type)
		}
	}
}

func (a *responsesAccumulator) jsonBody(b []byte) {
	var body struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Usage  *responsesUsage `json:"usage"`
		Output []responsesItem `json:"output"`
		Error  *struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(b, &body) != nil {
		return
	}
	if body.Error == nil && len(body.Detail) > 0 && string(body.Detail) != "null" {
		a.errType, a.errEnvelope = "detail", true
		return
	}
	if body.Error != nil {
		a.errType = firstNonEmptyStr(body.Error.Code, body.Error.Type)
		a.errEnvelope = true
		return
	}
	a.setResponse(&struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Usage  *responsesUsage `json:"usage"`
	}{body.ID, body.Status, body.Usage})
	a.completed = body.Status == "completed"
	for i := range body.Output {
		a.addItem(&body.Output[i])
	}
}

func (a *responsesAccumulator) result(status int, _ bool, readErr error) HTTPAttemptResult {
	var res HTTPAttemptResult
	zero := int64(0)
	switch {
	case status == http.StatusOK && a.completed && a.errType == "" && readErr == nil:
		res.Outcome = OutcomeSuccess
	case status == http.StatusOK && (a.errType == "server_error" || a.errType == "server_is_overloaded"):
		res.Outcome = OutcomeRetryable
	case status == http.StatusOK && a.errType == "rate_limit_exceeded":
		res.Outcome = OutcomeRateLimited
	case status == http.StatusOK:
		res.Outcome = OutcomeTerminalFailure
	case status == http.StatusTooManyRequests:
		res.Outcome = OutcomeRateLimited
	case status >= 500:
		res.Outcome = OutcomeRetryable
	default:
		res.Outcome = OutcomeTerminalFailure
	}
	if status != http.StatusOK {
		// As for Anthropic: explicit zeros only on the provider's own error
		// envelope; any other non-200 body is MISSING accounting.
		if !a.errEnvelope {
			return res
		}
		res.InputTokens, res.CachedInputTokens, res.UncachedInputTokens = &zero, &zero, &zero
		return res
	}
	if a.input == nil || a.cached == nil || *a.cached > *a.input {
		return res // MISSING: a 2xx response must report usage
	}
	uncached := *a.input - *a.cached
	res.InputTokens, res.CachedInputTokens, res.UncachedInputTokens = a.input, a.cached, &uncached
	return res
}

func (a *responsesAccumulator) id() string               { return a.respID }
func (a *responsesAccumulator) toolUses() []ProxyToolUse { return append([]ProxyToolUse{}, a.tools...) }
func (a *responsesAccumulator) terminal() map[string]any {
	return map[string]any{"status": a.status, "completed": a.completed, "error_type": a.errType}
}

func firstNonEmptyStr(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func commandText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}
