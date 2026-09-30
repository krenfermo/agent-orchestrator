package practical3d

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ObservedClient is the only provider boundary (AO_OBSERVED_CLIENT_ONLY_V1).
// Every attempt, retry and partial stream is appended as ATTEMPT_DISPATCHED
// before the transport sees it and ATTEMPT_FINALIZED after, with a contiguous
// per-position call_index. Calls are serialized so call_index follows
// dispatch order.
type ObservedClient struct {
	mu           sync.Mutex
	ctx          context.Context
	m            Manifest
	p            Position
	workspace    PositionWorkspace
	experimentID string
	ledger       *Ledger
	transport    Transport
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
	spans        map[string][][]byte
	callIndex    int
	chains       int
	roleStart    map[Role]time.Time
	malformed    string
	failure      TerminalState
	observe      func(context.Context) (string, error)
	subjects     map[string]*subjectChain
	closed       bool
}

func newObservedClient(ctx context.Context, m Manifest, p Position, workspace PositionWorkspace, id string, l *Ledger, t Transport, now func() time.Time, sleep func(context.Context, time.Duration) error, spans map[string][][]byte) *ObservedClient {
	return &ObservedClient{ctx: ctx, m: m, p: p, workspace: workspace, experimentID: id, ledger: l, transport: t, now: now, sleep: sleep, spans: spans, roleStart: map[Role]time.Time{}}
}

// CallError reports a terminal provider/deadline outcome to the executor.
type CallError struct{ State TerminalState }

func (e *CallError) Error() string { return string(e.State) }

func (c *ObservedClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *ObservedClient) malform(reason string) error {
	if c.malformed == "" {
		c.malformed = reason
	}
	return errors.New(reason)
}

func (c *ObservedClient) fail(state TerminalState) error {
	if c.failure == "" {
		c.failure = state
	}
	return &CallError{State: c.failure}
}

// Call performs one logical provider call, including its in-position retries
// under the frozen per-role budget and backoff.
func (c *ObservedClient) Call(req ProviderRequest) (ProviderResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ProviderResponse{}, errors.New("provider call after the position ended")
	}
	if c.malformed != "" {
		return ProviderResponse{}, errors.New(c.malformed)
	}
	if c.failure != "" {
		return ProviderResponse{}, &CallError{State: c.failure}
	}
	if req.CallClass == CallRetry {
		return ProviderResponse{}, c.malform("executors cannot issue retry calls; retries are owned by ObservedClient")
	}
	budget := retryBudget(c.m, req.Role)
	c.chains++
	chain := sha256Hex([]byte(fmt.Sprintf("chain:%s:%s:%d", c.experimentID, c.p.SampleID, c.chains)))
	retryIndex := 0
	var retryCause RequestOutcome
	retries := map[RequestOutcome]int{}
	for {
		class := req.CallClass
		if retryIndex > 0 {
			class = CallRetry
		}
		if start, ok := c.roleStart[req.Role]; ok && c.now().Sub(start) > time.Duration(roleDeadline(c.m, req.Role))*time.Second {
			return ProviderResponse{}, c.fail(StateTimeout)
		}
		canonical, trace, err := traceRepresentation(c.m, c.spans, c.p.TaskID, c.p.Arm, req.Role, class, req.Representation)
		if err != nil {
			return ProviderResponse{}, c.malform("treatment/representation: " + err.Error())
		}
		cfg, _ := invocationConfig(c.m, c.p.TaskID, req.Role, class)
		observed := ""
		if c.observe != nil {
			d, err := c.observe(c.ctx)
			if err != nil || d != c.m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest {
				return ProviderResponse{}, c.malform("execution environment diverged before a provider attempt")
			}
			observed = d
		}
		c.callIndex++
		attemptID := sha256Hex([]byte(fmt.Sprintf("attempt:%s:%s:%d", c.experimentID, c.p.SampleID, c.callIndex)))
		base := Event{ExperimentID: c.experimentID, SampleID: c.p.SampleID, PositionIndex: c.p.PositionIndex, TaskID: c.p.TaskID, Arm: c.p.Arm, AttemptID: attemptID, CallIndex: c.callIndex, Role: req.Role, CallClass: class, RetryChainID: chain, RetryIndex: retryIndex, RetryCause: retryCause}
		dispatch := base
		dispatch.Type, dispatch.Timestamp = EventAttemptDispatched, c.now().UTC()
		dispatch.ProviderID, dispatch.ModelID, dispatch.ModelVersion, dispatch.EffectiveConfigSHA256 = c.m.Provider.ProviderID, cfg.ModelID, cfg.ModelVersion, cfg.EffectiveConfigSHA256
		dispatch.RepresentationSHA256 = sha256Hex(canonical)
		dispatch.ObservedDigest = observed
		dispatch.AttachmentPresent, dispatch.AttachmentSHA256, dispatch.AttachmentVersion, dispatch.AttachmentOrigin = trace.AttachmentPresent, trace.AttachmentSHA256, trace.AttachmentVersion, trace.AttachmentOrigin
		dispatch.ExternalContext, dispatch.ContextSourceInventorySHA256, dispatch.ContextSourceStates = ptr(false), trace.ContextSourceInventorySHA256, trace.ContextSourceStates
		if err := c.ledger.Append(dispatch); err != nil {
			return ProviderResponse{}, c.malform("ledger append dispatch: " + err.Error())
		}
		if _, ok := c.roleStart[req.Role]; !ok {
			c.roleStart[req.Role] = c.now()
		}
		attemptCtx, cancel := context.WithTimeout(c.ctx, time.Duration(c.m.Deadlines.ProviderAttemptSeconds)*time.Second)
		resp, callErr := c.transport.Do(attemptCtx, TransportRequest{CanonicalRequest: bytes.Clone(canonical), Position: c.p, Workspace: c.workspace})
		attemptExpired := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		final := base
		final.Type, final.Timestamp = EventAttemptFinalized, c.now().UTC()
		final.RequestOutcome, final.InputTokens, final.CachedInputTokens, final.UncachedInputTokens = resp.Outcome, resp.InputTokens, resp.CachedInputTokens, resp.UncachedInputTokens
		final.ProviderMetadata, final.TerminalMetadata = resp.ProviderMetadata, resp.TerminalMetadata
		for name, v := range map[string]*int64{"input_tokens": resp.InputTokens, "cached_input_tokens": resp.CachedInputTokens, "uncached_input_tokens": resp.UncachedInputTokens} {
			if v == nil {
				final.MissingAccounting = append(final.MissingAccounting, name)
			}
		}
		sortStrings(final.MissingAccounting)
		if callErr != nil {
			final.TransportError = callErr.Error()
		}
		if err := c.ledger.Append(final); err != nil {
			return ProviderResponse{}, c.malform("ledger append finalization: " + err.Error())
		}
		if len(final.MissingAccounting) > 0 && (callErr == nil || !attemptExpired) {
			return resp, c.malform("provider accounting MISSING")
		}
		if callErr != nil && attemptExpired {
			// The frozen attempt/position deadline ended the attempt: TIMEOUT (06 §4).
			return resp, c.fail(StateTimeout)
		}
		if callErr != nil {
			return resp, c.malform("transport: " + callErr.Error())
		}
		var limit int
		switch resp.Outcome {
		case OutcomeSuccess:
			return resp, nil
		case OutcomeRetryable:
			limit = budget.RetryableMaxRetries
		case OutcomeRateLimited:
			limit = budget.RateLimitedMaxRetries
		case OutcomePolicyFailure:
			return resp, c.fail(StateProviderPolicyFailure)
		case OutcomeTerminalFailure:
			return resp, c.fail(StateProviderTerminalFailure)
		case OutcomeSanction:
			return resp, c.fail(StateProviderSanction)
		default:
			return resp, c.malform("unknown request outcome")
		}
		if retries[resp.Outcome] >= limit {
			if resp.Outcome == OutcomeRateLimited {
				return resp, c.fail(StateProviderRateLimited)
			}
			return resp, c.fail(StateProviderRetryExhausted)
		}
		retries[resp.Outcome]++
		retryIndex, retryCause = retries[resp.Outcome], resp.Outcome
		delay := budget.BackoffPolicy.BaseDelayMS << uint(retryIndex-1)
		if delay > budget.BackoffPolicy.MaxDelayMS || delay <= 0 {
			delay = budget.BackoffPolicy.MaxDelayMS
		}
		if err := c.sleep(c.ctx, time.Duration(delay)*time.Millisecond); err != nil {
			return resp, c.fail(StateTimeout)
		}
	}
}

type treatmentTrace struct {
	AttachmentPresent                                     *bool
	AttachmentSHA256, AttachmentVersion, AttachmentOrigin string
	ContextSourceInventorySHA256                          string
	ContextSourceStates                                   []ContextSource
}

// traceRepresentation serializes the final post-adapter representation and
// derives the treatment trace from those same bytes (re-decoded), so the
// traced snapshot is exactly what the transport receives. OFF, and ASSISTED
// cells frozen as absent, must carry no attachment and no byte span of any
// frozen attachment for the task.
func traceRepresentation(m Manifest, spans map[string][][]byte, task string, arm Arm, role Role, class CallClass, r Representation) ([]byte, treatmentTrace, error) {
	if !roleSet(m.ClosedRoleSet)[role] {
		return nil, treatmentTrace{}, fmt.Errorf("unknown role %q", role)
	}
	cell, ok := treatmentCell(m, task, role, class)
	if _, cfgOK := invocationConfig(m, task, role, class); !ok || !cfgOK {
		return nil, treatmentTrace{}, fmt.Errorf("task/role/call_class %s/%s/%s outside the frozen mapping", task, role, class)
	}
	canonical, err := canonicalRequestJSON(r)
	if err != nil {
		return nil, treatmentTrace{}, err
	}
	var snap Representation
	if err := strictUnmarshal(canonical, &snap); err != nil {
		return nil, treatmentTrace{}, fmt.Errorf("snapshot does not round-trip: %w", err)
	}
	if snap.ExternalContext {
		return nil, treatmentTrace{}, errors.New("externalContext must be false")
	}
	want := cell.OFF
	if arm == ArmAssisted {
		want = cell.ASSISTED
	}
	present := snap.ProjectMemoryAttachment != nil
	if want.AttachmentPresent == nil || present != *want.AttachmentPresent {
		return nil, treatmentTrace{}, errors.New("treatment attachment presence mismatch")
	}
	ctxStates, err := CanonicalJSON(snap.ContextSourceStates)
	if err != nil {
		return nil, treatmentTrace{}, err
	}
	manifestCtx, err := CanonicalJSON(m.ContextSourceInventory)
	if err != nil {
		return nil, treatmentTrace{}, err
	}
	if !bytes.Equal(ctxStates, manifestCtx) {
		return nil, treatmentTrace{}, errors.New("context source states differ from the frozen inventory")
	}
	trace := treatmentTrace{AttachmentPresent: &present, AttachmentOrigin: "NONE", ContextSourceInventorySHA256: sha256Hex(manifestCtx), ContextSourceStates: snap.ContextSourceStates}
	if present {
		a := snap.ProjectMemoryAttachment
		decoded, err := base64.StdEncoding.Strict().DecodeString(a.BytesBase64)
		if err != nil {
			return nil, treatmentTrace{}, errors.New("attachment bytes are not canonical base64")
		}
		if sha256Hex(decoded) != a.SHA256 || a.SHA256 != want.AttachmentSHA256 || a.Version != want.AttachmentVersion || a.Origin != "PROJECT_MEMORY" {
			return nil, treatmentTrace{}, errors.New("ASSISTED attachment differs from the frozen cell")
		}
		trace.AttachmentSHA256, trace.AttachmentVersion, trace.AttachmentOrigin = a.SHA256, a.Version, a.Origin
		return canonical, trace, nil
	}
	for _, span := range spans[allTasksSpans] {
		if bytes.Contains(canonical, span) {
			return nil, treatmentTrace{}, errors.New("request without attachment contains a Project Memory attachment span")
		}
	}
	return canonical, trace, nil
}

// minSpan is the shortest attachment span searched for in OFF requests.
const minSpan = 32

// attachmentSpans returns the byte patterns whose presence in a no-attachment
// request proves Project Memory leakage: the attachment's base64, its JSON
// string escaping, and each non-trivial line of it.
func attachmentSpans(b []byte) [][]byte {
	var out [][]byte
	add := func(x []byte) {
		if len(x) >= minSpan {
			out = append(out, x)
		}
	}
	add([]byte(base64.StdEncoding.EncodeToString(b)))
	add(canonicalStringBody(string(b)))
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		add(canonicalStringBody(string(bytes.TrimSpace(line))))
	}
	return out
}

// allTasksSpans keys the union of every frozen attachment's spans: a request
// without attachment must carry no span of ANY task's attachment.
const allTasksSpans = "*"

// canonicalStringBody is how a string's bytes appear inside canonical request
// bytes: NFC, JSON-escaped without HTML escaping, quotes stripped.
func canonicalStringBody(s string) []byte {
	b, err := canonicalRequestJSON(s)
	if err != nil || len(b) < 2 {
		return nil
	}
	return b[1 : len(b)-1]
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
