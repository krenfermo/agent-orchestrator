package practical3d

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HTTPAttemptRequest is one real provider HTTP request intercepted at the
// AO-owned provider proxy, before it is forwarded. Role comes from AO's
// control plane (the usage-attribution window of the launching subject), and
// BaseClass from the request's own structure; neither is agent-declared text.
type HTTPAttemptRequest struct {
	Subject   string
	Role      Role
	BaseClass CallClass
	Model     string
	Stream    bool
	ToolNames []string
	Body      []byte
}

// HTTPAttemptResult is what the proxy observed for the forwarded request.
type HTTPAttemptResult struct {
	Outcome             RequestOutcome
	InputTokens         *int64
	CachedInputTokens   *int64
	UncachedInputTokens *int64
	TransportError      string
	ProviderMetadata    json.RawMessage
	TerminalMetadata    json.RawMessage
	// Expired reports that the frozen attempt deadline ended the attempt.
	Expired bool
}

// HTTPAttempt is a dispatched attempt awaiting its finalization.
type HTTPAttempt struct {
	c        *ObservedClient
	base     Event
	subject  string
	digest   string
	finished bool
}

type subjectChain struct {
	chainID    string
	digest     string
	outcome    RequestOutcome
	retries    map[RequestOutcome]int
	role       Role
	firstClass CallClass
	inFlight   bool
}

// ErrAttemptRefused means the proxy must not forward the request: the
// position is already terminal/malformed or the request violates the frozen
// mapping. The refusal itself makes the position fail closed.
var ErrAttemptRefused = errors.New("3d practical: provider attempt refused")

// BeginHTTPAttempt validates a real provider request against the frozen
// manifest and appends ATTEMPT_DISPATCHED before the proxy forwards it. A
// replay of the previous request of the same subject after a RETRYABLE or
// RATE_LIMITED outcome is a `retry` of that logical call (06 §3.4); a retry
// beyond the frozen budget is refused and ends the position.
func (c *ObservedClient) BeginHTTPAttempt(req HTTPAttemptRequest) (*HTTPAttempt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	refuse := func(reason string) error {
		_ = c.malform(reason)
		return fmt.Errorf("%w: %s", ErrAttemptRefused, reason)
	}
	if c.closed {
		return nil, fmt.Errorf("%w: position ended", ErrAttemptRefused)
	}
	if c.malformed != "" {
		return nil, fmt.Errorf("%w: position malformed: %s", ErrAttemptRefused, c.malformed)
	}
	if c.failure != "" {
		return nil, fmt.Errorf("%w: position already %s", ErrAttemptRefused, c.failure)
	}
	if strings.TrimSpace(req.Subject) == "" {
		return nil, refuse("provider request without an AO subject")
	}
	digest := sha256Hex(req.Body)
	if c.subjects == nil {
		c.subjects = map[string]*subjectChain{}
	}
	class := req.BaseClass
	prev := c.subjects[req.Subject]
	retryIndex, retryCause, chain := 0, RequestOutcome(""), ""
	if prev != nil && !prev.inFlight && prev.digest == digest && (prev.outcome == OutcomeRetryable || prev.outcome == OutcomeRateLimited) {
		budget := retryBudget(c.m, prev.role)
		limit := budget.RetryableMaxRetries
		if prev.outcome == OutcomeRateLimited {
			limit = budget.RateLimitedMaxRetries
		}
		if prev.retries[prev.outcome] >= limit {
			state := StateProviderRetryExhausted
			if prev.outcome == OutcomeRateLimited {
				state = StateProviderRateLimited
			}
			_ = c.fail(state)
			return nil, fmt.Errorf("%w: retry budget exhausted", ErrAttemptRefused)
		}
		prev.retries[prev.outcome]++
		class, retryIndex, retryCause, chain = CallRetry, prev.retries[prev.outcome], prev.outcome, prev.chainID
		req.Role = prev.role
	}
	if start, ok := c.roleStart[req.Role]; ok && c.now().Sub(start) > time.Duration(roleDeadline(c.m, req.Role))*time.Second {
		_ = c.fail(StateTimeout)
		return nil, fmt.Errorf("%w: role deadline exceeded", ErrAttemptRefused)
	}
	trace, err := traceHTTPRequest(c.m, c.spans, c.p.TaskID, c.p.Arm, req.Role, class, req)
	if err != nil {
		return nil, refuse("treatment/representation: " + err.Error())
	}
	cfg, _ := invocationConfig(c.m, c.p.TaskID, req.Role, class)
	observed := ""
	if c.observe != nil {
		d, err := c.observe(c.ctx)
		if err != nil || d != c.m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest {
			return nil, refuse("execution environment diverged before a provider attempt")
		}
		observed = d
	}
	if chain == "" {
		c.chains++
		chain = sha256Hex([]byte(fmt.Sprintf("chain:%s:%s:%d", c.experimentID, c.p.SampleID, c.chains)))
		c.subjects[req.Subject] = &subjectChain{chainID: chain, digest: digest, retries: map[RequestOutcome]int{}, role: req.Role, firstClass: class}
	}
	c.subjects[req.Subject].inFlight = true
	c.callIndex++
	attemptID := sha256Hex([]byte(fmt.Sprintf("attempt:%s:%s:%d", c.experimentID, c.p.SampleID, c.callIndex)))
	base := Event{ExperimentID: c.experimentID, SampleID: c.p.SampleID, PositionIndex: c.p.PositionIndex, TaskID: c.p.TaskID, Arm: c.p.Arm, AttemptID: attemptID, CallIndex: c.callIndex, Role: req.Role, CallClass: class, RetryChainID: chain, RetryIndex: retryIndex, RetryCause: retryCause}
	dispatch := base
	dispatch.Type, dispatch.Timestamp = EventAttemptDispatched, c.now().UTC()
	dispatch.ProviderID, dispatch.ModelID, dispatch.ModelVersion, dispatch.EffectiveConfigSHA256 = c.m.Provider.ProviderID, cfg.ModelID, cfg.ModelVersion, cfg.EffectiveConfigSHA256
	dispatch.RepresentationSHA256 = digest
	dispatch.ObservedDigest = observed
	dispatch.AttachmentPresent, dispatch.AttachmentSHA256, dispatch.AttachmentVersion, dispatch.AttachmentOrigin = trace.AttachmentPresent, trace.AttachmentSHA256, trace.AttachmentVersion, trace.AttachmentOrigin
	dispatch.ExternalContext, dispatch.ContextSourceInventorySHA256, dispatch.ContextSourceStates = ptr(false), trace.ContextSourceInventorySHA256, trace.ContextSourceStates
	if err := c.ledger.Append(dispatch); err != nil {
		return nil, refuse("ledger append dispatch: " + err.Error())
	}
	if _, ok := c.roleStart[req.Role]; !ok {
		c.roleStart[req.Role] = c.now()
	}
	return &HTTPAttempt{c: c, base: base, subject: req.Subject, digest: digest}, nil
}

// Finish appends ATTEMPT_FINALIZED exactly once and applies the 06 §4
// transition table to the position.
func (a *HTTPAttempt) Finish(res HTTPAttemptResult) error {
	c := a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.finished {
		return errors.New("attempt already finalized")
	}
	a.finished = true
	final := a.base
	final.Type, final.Timestamp = EventAttemptFinalized, c.now().UTC()
	final.RequestOutcome, final.InputTokens, final.CachedInputTokens, final.UncachedInputTokens = res.Outcome, res.InputTokens, res.CachedInputTokens, res.UncachedInputTokens
	final.ProviderMetadata, final.TerminalMetadata, final.TransportError = res.ProviderMetadata, res.TerminalMetadata, res.TransportError
	for name, v := range map[string]*int64{"input_tokens": res.InputTokens, "cached_input_tokens": res.CachedInputTokens, "uncached_input_tokens": res.UncachedInputTokens} {
		if v == nil {
			final.MissingAccounting = append(final.MissingAccounting, name)
		}
	}
	sortStrings(final.MissingAccounting)
	if err := c.ledger.Append(final); err != nil {
		return c.malform("ledger append finalization: " + err.Error())
	}
	if sc := c.subjects[a.subject]; sc != nil && sc.chainID == a.base.RetryChainID {
		sc.inFlight, sc.outcome, sc.digest = false, res.Outcome, a.digest
	}
	switch {
	case res.Expired:
		return c.fail(StateTimeout)
	case len(final.MissingAccounting) > 0:
		return c.malform("provider accounting MISSING")
	case res.TransportError != "":
		return c.malform("transport: " + res.TransportError)
	}
	switch res.Outcome {
	case OutcomeSuccess, OutcomeRetryable, OutcomeRateLimited:
		return nil
	case OutcomePolicyFailure:
		return c.fail(StateProviderPolicyFailure)
	case OutcomeTerminalFailure:
		return c.fail(StateProviderTerminalFailure)
	case OutcomeSanction:
		return c.fail(StateProviderSanction)
	}
	return c.malform("unknown request outcome")
}

// Violation marks the position malformed for an observation-boundary breach
// (for example a provider request the proxy had to reject, or a provider
// call AO recorded that the proxy never saw).
func (c *ObservedClient) Violation(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.malform(reason)
}

// Outcome reports the client's terminal evidence.
func (c *ObservedClient) Outcome() (malformed string, failure TerminalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.malformed, c.failure
}

// traceHTTPRequest derives the treatment/context trace of a real request from
// the exact bytes forwarded. ASSISTED cells with an attachment must carry the
// frozen attachment bytes; OFF and absent cells must carry no span of any
// frozen attachment. MCP/web tools are forbidden while those context sources
// are DISABLED, and the model must equal the frozen invocation config.
func traceHTTPRequest(m Manifest, spans map[string][][]byte, task string, arm Arm, role Role, class CallClass, req HTTPAttemptRequest) (treatmentTrace, error) {
	if !roleSet(m.ClosedRoleSet)[role] {
		return treatmentTrace{}, fmt.Errorf("role %q outside CLOSED_ROLE_SET", role)
	}
	cell, ok := treatmentCell(m, task, role, class)
	cfg, cfgOK := invocationConfig(m, task, role, class)
	if !ok || !cfgOK {
		return treatmentTrace{}, fmt.Errorf("task/role/call_class %s/%s/%s outside the frozen mapping", task, role, class)
	}
	if req.Model != cfg.ModelID {
		return treatmentTrace{}, fmt.Errorf("request model %q differs from frozen %q", req.Model, cfg.ModelID)
	}
	if err := checkEffectiveHTTPConfig(cfg, req); err != nil {
		return treatmentTrace{}, err
	}
	if err := checkContextTools(m, req.ToolNames); err != nil {
		return treatmentTrace{}, err
	}
	manifestCtx, err := CanonicalJSON(m.ContextSourceInventory)
	if err != nil {
		return treatmentTrace{}, err
	}
	want := cell.OFF
	if arm == ArmAssisted {
		want = cell.ASSISTED
	}
	present := want.AttachmentPresent != nil && *want.AttachmentPresent
	trace := treatmentTrace{AttachmentPresent: &present, AttachmentOrigin: "NONE", ContextSourceInventorySHA256: sha256Hex(manifestCtx), ContextSourceStates: m.ContextSourceInventory}
	if present {
		body := spans[attachmentBodyKey(want.AttachmentSHA256)]
		if len(body) != 1 || !bytes.Contains(req.Body, body[0]) {
			return treatmentTrace{}, errors.New("ASSISTED request does not carry the frozen attachment bytes")
		}
		// Every Project Memory marker must belong to a copy of the frozen
		// attachment: a second, altered memory block (for example returned
		// by a tool) is untracked memory-labelled content.
		if err := markersOnlyWithin(req.Body, body[0]); err != nil {
			return treatmentTrace{}, err
		}
		trace.AttachmentSHA256, trace.AttachmentVersion, trace.AttachmentOrigin = want.AttachmentSHA256, want.AttachmentVersion, "PROJECT_MEMORY"
		return trace, nil
	}
	for _, span := range spans[allTasksSpans] {
		if bytes.Contains(req.Body, span) {
			return treatmentTrace{}, errors.New("request without attachment contains a Project Memory attachment span")
		}
	}
	if err := markersOnlyWithin(req.Body, nil); err != nil {
		return treatmentTrace{}, err
	}
	return trace, nil
}

// markersOnlyWithin fails if any Project Memory marker in body lies outside
// every occurrence of attachment (nil: no marker may occur at all).
func markersOnlyWithin(body, attachment []byte) error {
	var covers [][2]int
	if len(attachment) > 0 {
		for off := 0; ; {
			i := bytes.Index(body[off:], attachment)
			if i < 0 {
				break
			}
			covers = append(covers, [2]int{off + i, off + i + len(attachment)})
			off += i + 1
		}
	}
	for _, marker := range projectMemoryMarkers {
		mk := []byte(marker)
		for off := 0; ; {
			i := bytes.Index(body[off:], mk)
			if i < 0 {
				break
			}
			start, end := off+i, off+i+len(mk)
			inside := false
			for _, c := range covers {
				if c[0] <= start && end <= c[1] {
					inside = true
					break
				}
			}
			if !inside {
				return fmt.Errorf("request carries Project Memory marker %q outside the frozen attachment", marker)
			}
			off = start + 1
		}
	}
	return nil
}

// attachmentBodyKey indexes the exact in-request form of a frozen attachment.
func attachmentBodyKey(digest string) string { return "body:" + digest }

func checkContextTools(m Manifest, tools []string) error {
	state := map[string]string{}
	for _, s := range m.ContextSourceInventory {
		state[s.SourceID] = s.State
	}
	for _, t := range tools {
		switch {
		case strings.HasPrefix(t, "mcp__") && state["mcp"] != "EQUALIZED":
			return fmt.Errorf("MCP tool %q offered while mcp is %s", t, state["mcp"])
		case (t == "WebFetch" || t == "WebSearch" || strings.HasPrefix(t, "web_search")) && state["web"] != "EQUALIZED":
			return fmt.Errorf("web tool %q offered while web is %s", t, state["web"])
		}
	}
	return nil
}
