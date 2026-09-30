package practical3d

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

type eventBucket struct{ all, envPre, envPost, starts, results, terminals, dispatches, finalizations []Event }

// ReportSchemaVersion versions the published report.
const ReportSchemaVersion = "ao.3d-practical.report.v2"

// EvaluateLedgerFile verifies the ledger hash chain and decides it. A broken
// chain (a removed, inserted, reordered or edited line) invalidates lineage.
func EvaluateLedgerFile(m Manifest, path string, now time.Time) Report {
	events, err := ReadLedger(path)
	if err != nil {
		r := Evaluate(m, nil, now)
		r.ReasonCode, r.Reason = ReasonLineageInvalid, "ledger unreadable: "+err.Error()
		return r
	}
	report := Evaluate(m, events, now)
	if err := VerifyLedgerChain(path); err != nil {
		report.Verdict, report.LineageValid, report.AllCompleted = "NO_GO", false, false
		report.LineageErrors = append(report.LineageErrors, err.Error())
		if report.ReasonCode == ReasonAllConditions || report.ReasonCode == ReasonNotAllCompleted || report.ReasonCode == ReasonQuality || report.ReasonCode == ReasonEfficiency || report.ReasonCode == ReasonNegativeControl {
			report.ReasonCode = ReasonLineageInvalid
		}
		report.Reason = err.Error()
	}
	return report
}

// Evaluate is decide(manifest, ledger) of 06 §6 plus the diagnostic
// publication of 06 §7. It is total: every input yields a Report, and every
// report other than GO is NO_GO.
func Evaluate(m Manifest, events []Event, now time.Time) Report {
	report := Report{SchemaVersion: ReportSchemaVersion, Verdict: "NO_GO", GeneratedAt: now, SignalAvailability: map[string]string{}}
	report.ResidualConfounder = ResidualConfounder{Present: true, Statement: "Provider cache and shared provider state were not isolated; observed cache values are a residual covariate and zero observed cache does not prove isolation."}
	noGo := func(code, reason string) Report {
		report.ReasonCode = code
		report.Reason = reason
		return report
	}
	id, err := ExperimentID(m)
	if err != nil {
		return noGo(ReasonPrestartInvalid, "manifest is not canonicalizable: "+err.Error())
	}
	report.ExperimentID = id
	if err := validateManifestExceptSchedule(m); err != nil {
		return noGo(ReasonPrestartInvalid, err.Error())
	}
	if len(events) == 0 || events[0].Type != EventManifest {
		return noGo(ReasonPrestartInvalid, "manifest was not frozen before the first SAMPLE_START")
	}
	if code, reason := checkFrozenIdentity(m, id, events[0]); code != "" {
		return noGo(code, reason)
	}
	if reason := checkPreflight(m, events); reason != "" {
		return noGo(ReasonPrestartInvalid, reason)
	}
	if err := ValidateSchedule(m.Randomization); err != nil {
		return noGo(ReasonScheduleInvalid, err.Error())
	}

	buckets := map[string]*eventBucket{}
	scheduled := map[string]Position{}
	for _, p := range m.Randomization.Schedule {
		scheduled[p.SampleID] = p
		buckets[p.SampleID] = &eventBucket{}
	}
	lineage := []string{}
	invalid := func(format string, args ...any) { lineage = append(lineage, fmt.Sprintf(format, args...)) }
	stopAt := 0
	lastIndex := 0
	current := ""
	closed := map[string]bool{}
	for i, e := range events[1:] {
		pos := i + 1
		if e.ExperimentID != id {
			invalid("event %d belongs to experiment %q", pos, e.ExperimentID)
			continue
		}
		switch e.Type {
		case EventManifest:
			invalid("duplicate MANIFEST_FROZEN at event %d", pos)
			continue
		case EventPreflight:
			continue // validated by checkPreflight
		case EventDecision:
			if pos != len(events)-1 {
				invalid("DECISION is not the final ledger event")
			}
			continue
		case EventBatchStop:
			p, ok := scheduled[e.SampleID]
			if stopAt != 0 || !ok || p.PositionIndex != e.PositionIndex || current != e.SampleID || len(buckets[e.SampleID].terminals) != 1 {
				invalid("BATCH_STOP at event %d does not directly follow one terminal position", pos)
				continue
			}
			stopAt = e.PositionIndex
			continue
		}
		b, ok := buckets[e.SampleID]
		if !ok {
			invalid("event %d (%s) has no scheduled sample", pos, e.Type)
			continue
		}
		if e.SampleID != current {
			p := scheduled[e.SampleID]
			if closed[e.SampleID] {
				invalid("position %d reappears after other positions (rerun/replacement)", p.PositionIndex)
			} else if p.PositionIndex <= lastIndex {
				invalid("position %d executed out of schedule order", p.PositionIndex)
			}
			if current != "" {
				closed[current] = true
			}
			current = e.SampleID
			if p.PositionIndex > lastIndex {
				lastIndex = p.PositionIndex
			}
		}
		b.all = append(b.all, e)
		switch e.Type {
		case EventEnvironment:
			switch e.Phase {
			case "PRE_START":
				b.envPre = append(b.envPre, e)
			case "PRE_TERMINAL":
				b.envPost = append(b.envPost, e)
			default:
				invalid("unknown environment phase %q", e.Phase)
			}
		case EventSampleStart:
			b.starts = append(b.starts, e)
		case EventPositionResult:
			b.results = append(b.results, e)
		case EventPositionTerminal:
			b.terminals = append(b.terminals, e)
		case EventAttemptDispatched:
			b.dispatches = append(b.dispatches, e)
		case EventAttemptFinalized:
			b.finalizations = append(b.finalizations, e)
		default:
			invalid("unexpected event type %q for a position", e.Type)
		}
	}
	for _, p := range m.Randomization.Schedule {
		b := buckets[p.SampleID]
		switch len(b.terminals) {
		case 0:
			invalid("position %d has no terminal record", p.PositionIndex)
		case 1:
		default:
			invalid("position %d has %d terminal records", p.PositionIndex, len(b.terminals))
		}
		if len(b.starts) > 1 {
			invalid("position %d started %d times", p.PositionIndex, len(b.starts))
		}
		isBlocked := len(b.terminals) == 1 && b.terminals[0].TerminalState == StateBlocked
		if stopAt != 0 && p.PositionIndex > stopAt && !isBlocked {
			invalid("position %d after BATCH_STOP is not BLOCKED_BY_PRIOR_POSITION", p.PositionIndex)
		}
		pr := evaluatePosition(m, p, b, stopAt)
		report.Positions = append(report.Positions, pr)
	}
	report.LineageErrors = lineage
	report.LineageValid = len(lineage) == 0
	report.AllCompleted = report.LineageValid
	for _, p := range report.Positions {
		if p.State != StateCompleted || len(p.Errors) > 0 {
			report.AllCompleted = false
		}
	}
	publishDiagnostics(m, &report)
	if !report.LineageValid {
		return noGo(ReasonLineageInvalid, "lineage invalid: "+lineage[0])
	}
	if !report.AllCompleted {
		return noGo(ReasonNotAllCompleted, "not all 40 scheduled positions are COMPLETED with valid evidence")
	}
	if !qualityOK(report.Positions) {
		return noGo(ReasonQuality, "per-task pass count decreased for Q1, Q4 or Q6")
	}
	if !efficiencyOK(m, report.Positions) {
		return noGo(ReasonEfficiency, "tasks A-C did not all meet the frozen M1u and M2/M3 ratios")
	}
	if !neutralD(m, report.Positions) {
		return noGo(ReasonNegativeControl, "task D medians are outside the neutral band")
	}
	report.Verdict = "GO"
	report.ReasonCode = ReasonAllConditions
	report.Reason = "all frozen conditions satisfied"
	return report
}

func checkFrozenIdentity(m Manifest, id string, frozen Event) (string, string) {
	if frozen.ExperimentID != id || frozen.ManifestSHA256 != id {
		return ReasonIdentityMismatch, "ledger experiment_id/manifest_sha256 differ from canonical_digest(manifest)"
	}
	embedded, err := canonicalRaw(frozen.Manifest, manifestDecimals)
	if err != nil {
		return ReasonIdentityMismatch, "ledger manifest is not canonical"
	}
	want, _ := CanonicalManifest(m)
	if !bytes.Equal(embedded, want) {
		return ReasonIdentityMismatch, "ledger manifest differs from the evaluated manifest"
	}
	return "", ""
}

// checkPreflight requires exactly one PREFLIGHT_PASSED, before any position
// event, whose observed environment digest equals the expected digest, and no
// PRESTART_INVALID record.
func checkPreflight(m Manifest, events []Event) string {
	count := 0
	for i, e := range events {
		switch e.Type {
		case EventPrestartInvalid:
			return "ledger records PRESTART_INVALID: " + e.Reason
		case EventPreflight:
			count++
			if i != 1 {
				return "PREFLIGHT_PASSED is not immediately after MANIFEST_FROZEN"
			}
			if e.ObservedDigest != m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest {
				return "preflight environment digest differs from the manifest expected digest"
			}
		}
	}
	if count != 1 {
		return "ledger does not contain exactly one PREFLIGHT_PASSED"
	}
	return ""
}

func evaluatePosition(m Manifest, p Position, b *eventBucket, stopAt int) PositionReport {
	pr := PositionReport{Position: p, State: StateMalformedResult, Diagnostics: emptyDiagnostics()}
	erradd := func(s string) { pr.Errors = append(pr.Errors, s) }
	terminal := StateMalformedResult
	if len(b.terminals) != 1 {
		erradd("terminal record cardinality is not one")
	} else {
		terminal = b.terminals[0].TerminalState
		if !validTerminal(terminal) {
			erradd("unknown terminal state")
		}
	}
	if terminal == StateBlocked && len(b.terminals) == 1 {
		if len(b.all) != 1 {
			erradd("blocked position contains execution evidence")
		}
		if stopAt == 0 || p.PositionIndex <= stopAt {
			erradd("blocked position has no prior BATCH_STOP")
		} else if len(pr.Errors) == 0 {
			pr.State = StateBlocked
		}
		pr.Metrics = imputed(m, p.TaskID)
		return pr
	}
	for _, e := range b.all {
		if e.SampleID != p.SampleID || e.PositionIndex != p.PositionIndex || e.TaskID != p.TaskID || e.Arm != p.Arm {
			erradd("position event identity mismatch")
			break
		}
	}
	if err := validateEventOrder(b.all); err != "" {
		erradd(err)
	}
	if len(b.envPre) != 1 || len(b.envPost) != 1 {
		erradd("environment observations are not exactly PRE_START + PRE_TERMINAL")
	}
	for _, e := range append(append([]Event{}, b.envPre...), b.envPost...) {
		if e.ObservedDigest != m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest {
			erradd("execution environment digest mismatch")
		}
	}
	if len(b.starts) != 1 {
		erradd("SAMPLE_START cardinality is not one")
	}
	if len(b.results) != 1 || b.results[0].Result == nil {
		erradd("position result cardinality is not one")
	}
	mat := materializeAttempts(m, p, b.dispatches, b.finalizations)
	pr.Diagnostics = mat.diagnostics
	for _, x := range mat.errors {
		erradd(x)
	}
	for _, x := range checkTerminalConsistency(m, terminal, mat) {
		erradd(x)
	}
	metrics := mat.metrics
	if len(b.results) == 1 && b.results[0].Result != nil {
		r := b.results[0].Result
		if r.TerminalState != terminal {
			erradd("position result and terminal state disagree")
		}
		if terminal == StateCompleted {
			if r.ExplorationCalls < 0 || r.DistinctFilesRead < 0 {
				erradd("negative exploration metric")
			}
			if !r.MilestoneObserved {
				erradd("required M3 milestone missing")
			}
			if c, ok := m3Cap(m, p.TaskID); !ok {
				erradd("M3 cap missing")
			} else {
				metrics.M3 = math.Min(1, math.Max(float64(r.ExplorationCalls)/float64(c.CCap), float64(r.DistinctFilesRead)/float64(c.FCap)))
				if math.IsNaN(metrics.M3) || math.IsInf(metrics.M3, 0) || metrics.M3 < 0 || metrics.M3 > 1 {
					erradd("M3 outside domain")
				}
			}
			if r.Q1 == nil || r.Q4 == nil {
				erradd("Q1/Q4 missing")
			} else {
				metrics.Q1, metrics.Q4 = *r.Q1, *r.Q4
			}
		}
		if p.TaskID == "C" {
			metrics.Q6 = Q6Value{Applicable: true, Pass: scoreQ6(m.Q6Oracle, r.Findings)}
		} else if len(r.Findings) != 0 {
			erradd("Q6 findings present outside task C")
		}
	}
	if len(pr.Errors) > 0 {
		pr.State = StateMalformedResult
		pr.Metrics = imputed(m, p.TaskID)
		return pr
	}
	pr.State = terminal
	if terminal != StateCompleted {
		pr.Metrics = imputed(m, p.TaskID)
		return pr
	}
	pr.Metrics = metrics
	return pr
}

func validateEventOrder(events []Event) string {
	started, resulted, terminal, postEnv := false, false, false, false
	dispatched := map[attemptKey]bool{}
	for _, e := range events {
		if terminal {
			return "event appended after terminal"
		}
		switch e.Type {
		case EventEnvironment:
			if e.Phase == "PRE_START" && started {
				return "PRE_START environment observation after SAMPLE_START"
			}
			if e.Phase == "PRE_TERMINAL" {
				if !started {
					return "PRE_TERMINAL environment observation before SAMPLE_START"
				}
				postEnv = true
			}
		case EventSampleStart:
			if started {
				return "duplicate SAMPLE_START"
			}
			started = true
		case EventAttemptDispatched:
			if !started || postEnv || resulted {
				return "dispatch outside the running position"
			}
			dispatched[attemptKey{e.SampleID, e.AttemptID, e.CallIndex}] = true
		case EventAttemptFinalized:
			if !started || postEnv || resulted || !dispatched[attemptKey{e.SampleID, e.AttemptID, e.CallIndex}] {
				return "finalization without a preceding matching dispatch"
			}
		case EventPositionResult:
			if !started || !postEnv {
				return "result before SAMPLE_START or before the PRE_TERMINAL observation"
			}
			resulted = true
		case EventPositionTerminal:
			if !started || !resulted {
				return "terminal before SAMPLE_START or POSITION_RESULT"
			}
			terminal = true
		}
	}
	return ""
}

type attemptKey struct {
	sample, attempt string
	index           int
}

type attempt struct{ d, f Event }

func taskOf(mat materialized) string {
	if len(mat.attempts) == 0 {
		return ""
	}
	return mat.attempts[0].d.TaskID
}

type materialized struct {
	metrics     Metrics
	diagnostics PositionDiagnostics
	errors      []string
	attempts    []attempt // valid ones, ordered by call_index
	chains      map[string][]attempt
}

func emptyDiagnostics() PositionDiagnostics {
	return PositionDiagnostics{ProviderErrors: map[string]int{}, RoleCalls: map[Role]int64{}, RoleUncachedTokens: map[Role]int64{}, CacheByRequest: []RequestCache{}}
}

// materializeAttempts is the normative 1:1 view of 06 §4: an attempt exists
// only with exactly one dispatch and one finalization for the same identity,
// all repeated identity fields equal, accounting present and in domain.
func materializeAttempts(m Manifest, p Position, dispatches, finals []Event) materialized {
	out := materialized{diagnostics: emptyDiagnostics(), chains: map[string][]attempt{}}
	type pair struct{ d, f []Event }
	pairs := map[attemptKey]*pair{}
	var order []attemptKey
	get := func(e Event) *pair {
		k := attemptKey{e.SampleID, e.AttemptID, e.CallIndex}
		if pairs[k] == nil {
			pairs[k] = &pair{}
			order = append(order, k)
		}
		return pairs[k]
	}
	for _, e := range dispatches {
		get(e).d = append(get(e).d, e)
	}
	for _, e := range finals {
		get(e).f = append(get(e).f, e)
	}
	errs := &out.errors
	seenIDs := map[string]bool{}
	allIndices := []int{}
	ctxBytes, _ := CanonicalJSON(m.ContextSourceInventory)
	ctxDigest := sha256Hex(ctxBytes)
	for _, k := range order {
		x := pairs[k]
		allIndices = append(allIndices, k.index)
		if seenIDs[k.attempt] {
			*errs = append(*errs, "attempt_id is not unique")
		}
		seenIDs[k.attempt] = true
		if len(x.d) != 1 || len(x.f) != 1 {
			*errs = append(*errs, fmt.Sprintf("attempt %d dispatch/finalization cardinality %d/%d", k.index, len(x.d), len(x.f)))
			continue
		}
		d, f := x.d[0], x.f[0]
		if d.ExperimentID != f.ExperimentID || d.SampleID != p.SampleID || f.SampleID != p.SampleID || d.AttemptID != f.AttemptID || d.CallIndex != f.CallIndex || d.Role != f.Role || d.CallClass != f.CallClass || d.TaskID != p.TaskID || f.TaskID != p.TaskID || d.Arm != p.Arm || f.Arm != p.Arm || d.AttemptID == "" || d.CallIndex < 1 {
			*errs = append(*errs, "attempt identity mismatch")
			continue
		}
		if d.RetryChainID == "" || d.RetryChainID != f.RetryChainID || d.RetryIndex != f.RetryIndex || d.RetryCause != f.RetryCause || d.RetryIndex < 0 {
			*errs = append(*errs, "retry metadata mismatch")
			continue
		}
		if !roleSet(m.ClosedRoleSet)[d.Role] || !validCallClass(d.CallClass) {
			*errs = append(*errs, "attempt role/call_class outside CLOSED_ROLE_SET")
			continue
		}
		cell, ok := treatmentCell(m, p.TaskID, d.Role, d.CallClass)
		cfg, cfgOK := invocationConfig(m, p.TaskID, d.Role, d.CallClass)
		if !ok || !cfgOK {
			*errs = append(*errs, "attempt outside treatment/config mapping")
			continue
		}
		if d.ProviderID != m.Provider.ProviderID || d.ModelID != cfg.ModelID || d.ModelVersion != cfg.ModelVersion || d.EffectiveConfigSHA256 != cfg.EffectiveConfigSHA256 {
			*errs = append(*errs, "dispatch provider/model/config differs from the invocation mapping")
			continue
		}
		if d.ObservedDigest != m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest {
			*errs = append(*errs, "dispatch lacks a matching per-attempt environment observation")
			continue
		}
		if !validSHA256(d.RepresentationSHA256) || d.AttachmentPresent == nil || d.ExternalContext == nil || *d.ExternalContext {
			*errs = append(*errs, "dispatch trace incomplete")
			continue
		}
		want := cell.OFF
		if p.Arm == ArmAssisted {
			want = cell.ASSISTED
		}
		if want.AttachmentPresent == nil || *d.AttachmentPresent != *want.AttachmentPresent {
			*errs = append(*errs, "dispatch treatment presence mismatch")
			continue
		}
		if *d.AttachmentPresent {
			if d.AttachmentSHA256 != want.AttachmentSHA256 || d.AttachmentVersion != want.AttachmentVersion || d.AttachmentOrigin != "PROJECT_MEMORY" {
				*errs = append(*errs, "dispatch attachment mismatch")
				continue
			}
		} else if d.AttachmentSHA256 != "" || d.AttachmentVersion != "" || d.AttachmentOrigin != "NONE" {
			*errs = append(*errs, "absent attachment carries attachment fields or origin other than NONE")
			continue
		}
		if d.ContextSourceInventorySHA256 != ctxDigest {
			*errs = append(*errs, "dispatch context inventory mismatch")
			continue
		}
		if states, _ := CanonicalJSON(d.ContextSourceStates); sha256Hex(states) != ctxDigest {
			*errs = append(*errs, "dispatch context source states differ from the frozen inventory")
			continue
		}
		if len(f.MissingAccounting) != 0 || f.InputTokens == nil || f.CachedInputTokens == nil || f.UncachedInputTokens == nil {
			*errs = append(*errs, "attempt accounting MISSING")
			continue
		}
		if f.TransportError != "" {
			*errs = append(*errs, "attempt finalized with a transport error")
			continue
		}
		in, cache, uncached := *f.InputTokens, *f.CachedInputTokens, *f.UncachedInputTokens
		if in < 0 || cache < 0 || uncached < 0 || in > MaxCap || cache > in || cache+uncached != in {
			*errs = append(*errs, "attempt accounting outside domain")
			continue
		}
		if !validOutcome(f.RequestOutcome) {
			*errs = append(*errs, "unknown request outcome")
			continue
		}
		if len(f.ProviderMetadata) == 0 || !json.Valid(f.ProviderMetadata) || len(f.TerminalMetadata) == 0 || !json.Valid(f.TerminalMetadata) {
			*errs = append(*errs, "provider/terminal metadata missing")
			continue
		}
		if !accountAttested(m, f.ProviderMetadata) {
			*errs = append(*errs, "provider metadata does not attest the frozen account_ref_sha256")
			continue
		}
		if f.Timestamp.Before(d.Timestamp) {
			*errs = append(*errs, "finalization precedes dispatch")
			continue
		}
		out.attempts = append(out.attempts, attempt{d, f})
	}
	sort.Ints(allIndices)
	for i, x := range allIndices {
		if x != i+1 {
			*errs = append(*errs, "call_index is not unique and contiguous from 1")
			break
		}
	}
	sort.Slice(out.attempts, func(i, j int) bool { return out.attempts[i].d.CallIndex < out.attempts[j].d.CallIndex })
	roleTokens := map[Role]int64{}
	roleCalls := map[Role]int64{}
	diag := &out.diagnostics
	lastIndex := 0
	for _, e := range dispatches {
		if e.CallIndex <= lastIndex {
			*errs = append(*errs, "ledger dispatch order differs from call_index order")
			break
		}
		lastIndex = e.CallIndex
	}
	for _, a := range out.attempts {
		if u := *a.f.UncachedInputTokens; u > roleCap(m.TokenCaps, p.TaskID, a.d.Role) {
			*errs = append(*errs, fmt.Sprintf("role %s token cap exceeded by a single attempt", a.d.Role))
			return out
		}
		d, f := a.d, a.f
		roleCalls[d.Role]++
		roleTokens[d.Role] += *f.UncachedInputTokens
		out.metrics.M1U += *f.UncachedInputTokens
		out.metrics.M2++
		diag.Attempts++
		diag.M1Total += *f.InputTokens
		diag.CachedInputTokens += *f.CachedInputTokens
		diag.UncachedInputTokens += *f.UncachedInputTokens
		if f.RequestOutcome != OutcomeSuccess {
			diag.ProviderErrors[string(f.RequestOutcome)]++
		}
		diag.CacheByRequest = append(diag.CacheByRequest, RequestCache{CallIndex: d.CallIndex, Role: d.Role, CallClass: d.CallClass, CachedInputTokens: *f.CachedInputTokens, UncachedInputTokens: *f.UncachedInputTokens})
		if diag.FirstDispatch == nil {
			t := d.Timestamp
			diag.FirstDispatch = &t
		}
		t := f.Timestamp
		diag.LastFinalization = &t
		out.chains[d.RetryChainID] = append(out.chains[d.RetryChainID], a)
	}
	diag.RoleCalls, diag.RoleUncachedTokens = roleCalls, roleTokens
	for _, role := range m.ClosedRoleSet {
		if roleCalls[role] > roleCap(m.CallCaps, p.TaskID, role) {
			*errs = append(*errs, fmt.Sprintf("role %s call cap exceeded", role))
		}
		if roleTokens[role] > roleCap(m.TokenCaps, p.TaskID, role) {
			*errs = append(*errs, fmt.Sprintf("role %s token cap exceeded", role))
		}
	}
	for _, chain := range out.chains {
		if err := validateChain(m, chain); err != "" {
			*errs = append(*errs, err)
		}
	}
	capM1u, capM2 := int64(0), int64(0)
	for _, r := range m.ClosedRoleSet {
		capM1u += roleCap(m.TokenCaps, p.TaskID, r)
		capM2 += roleCap(m.CallCaps, p.TaskID, r)
	}
	if out.metrics.M1U < 0 || out.metrics.M1U > capM1u || out.metrics.M2 < 0 || out.metrics.M2 > capM2 {
		*errs = append(*errs, "M1u/M2 outside [0, sum of caps]")
	}
	return out
}

// validateChain enforces 06 §3.4: a logical call's first attempt has
// retry_index 0; every later attempt is a `retry` call of the same role,
// dispatched after the finalization of an attempt whose outcome was RETRYABLE or RATE_LIMITED,
// names that outcome as its cause, and carries retry_index = number of
// retries with that cause so far, never beyond the frozen budget.
func validateChain(m Manifest, chain []attempt) string {
	first := chain[0].d
	if first.RetryIndex != 0 || first.RetryCause != "" || first.CallClass == CallRetry {
		return "retry chain does not start with a first attempt"
	}
	budget := retryBudget(m, first.Role)
	used := map[RequestOutcome]int{}
	for i := 1; i < len(chain); i++ {
		prev, cur := chain[i-1], chain[i].d
		if cur.Role != first.Role || cur.CallClass != CallRetry || cur.CallIndex <= prev.d.CallIndex || cur.Timestamp.Before(prev.f.Timestamp) {
			return "retry is not a later retry call of the same role dispatched after the failed attempt finalized"
		}
		cause := prev.f.RequestOutcome
		if cause != OutcomeRetryable && cause != OutcomeRateLimited {
			return "retry after a non-retryable outcome"
		}
		used[cause]++
		limit := budget.RetryableMaxRetries
		if cause == OutcomeRateLimited {
			limit = budget.RateLimitedMaxRetries
		}
		if cur.RetryCause != cause || cur.RetryIndex != used[cause] || used[cause] > limit {
			return "retry cause/index/budget mismatch"
		}
	}
	return ""
}

func chainExhausted(m Manifest, chain []attempt, outcome RequestOutcome) bool {
	last := chain[len(chain)-1]
	if last.f.RequestOutcome != outcome {
		return false
	}
	budget := retryBudget(m, last.d.Role)
	limit := budget.RetryableMaxRetries
	if outcome == OutcomeRateLimited {
		limit = budget.RateLimitedMaxRetries
	}
	n := 0
	for _, a := range chain[1:] {
		if a.d.RetryCause == outcome {
			n++
		}
	}
	return n == limit
}

// checkTerminalConsistency rejects terminal states contradicted by the
// materialized attempts (06 §4 transition table).
func checkTerminalConsistency(m Manifest, state TerminalState, mat materialized) []string {
	var last *attempt
	if n := len(mat.attempts); n > 0 {
		last = &mat.attempts[n-1]
	}
	lastChain := func() []attempt {
		if last == nil {
			return nil
		}
		return mat.chains[last.d.RetryChainID]
	}
	endsWith := func(o RequestOutcome) bool { return last != nil && last.f.RequestOutcome == o }
	var errs []string
	switch state {
	case StateCompleted:
		measured := false
		if c, ok := m3Cap(m, taskOf(mat)); ok {
			for _, chain := range mat.chains {
				first, last := chain[0], chain[len(chain)-1]
				if first.d.Role == c.Role && first.d.CallClass == CallInitial && last.f.RequestOutcome == OutcomeSuccess {
					measured = true
				}
			}
		}
		if !measured {
			errs = append(errs, "COMPLETED position has no successful initial logical call of its measured role (M1u not measured)")
		}
		for _, c := range mat.chains {
			if c[len(c)-1].f.RequestOutcome != OutcomeSuccess {
				errs = append(errs, "COMPLETED position has a logical call that did not succeed")
				break
			}
		}
	case StateProviderPolicyFailure:
		if !endsWith(OutcomePolicyFailure) {
			errs = append(errs, "PROVIDER_POLICY_FAILURE without a final POLICY_FAILURE attempt")
		}
	case StateProviderTerminalFailure:
		if !endsWith(OutcomeTerminalFailure) {
			errs = append(errs, "PROVIDER_TERMINAL_FAILURE without a final terminal provider attempt")
		}
	case StateProviderSanction:
		if !endsWith(OutcomeSanction) {
			errs = append(errs, "PROVIDER_SANCTION without a final sanction attempt")
		}
	case StateProviderRetryExhausted:
		if c := lastChain(); c == nil || !chainExhausted(m, c, OutcomeRetryable) {
			errs = append(errs, "PROVIDER_RETRY_EXHAUSTED without an exhausted retryable chain")
		}
	case StateProviderRateLimited:
		if c := lastChain(); c == nil || !chainExhausted(m, c, OutcomeRateLimited) {
			errs = append(errs, "PROVIDER_RATE_LIMITED without an exhausted rate-limit chain")
		}
	default:
		for _, c := range mat.chains {
			o := c[len(c)-1].f.RequestOutcome
			if o == OutcomePolicyFailure || o == OutcomeTerminalFailure || o == OutcomeSanction || chainExhausted(m, c, OutcomeRetryable) || chainExhausted(m, c, OutcomeRateLimited) {
				errs = append(errs, fmt.Sprintf("%s contradicts a provider-terminal attempt outcome", state))
				break
			}
		}
	}
	return errs
}

// scoreQ6 is Q6 of 3d-practical §5 / 06 §3.7. Correspondence to a frozen
// defect is exact tuple equality; the reviewer never names defect IDs.
func scoreQ6(q Q6Oracle, findings []Finding) bool {
	if len(findings) == 0 || len(findings) > q.K || len(findings) != len(q.MandatoryDefects) {
		return false
	}
	type dupKey struct {
		file               string
		line               int
		class, cause, impa string
	}
	ranks := map[int]bool{}
	dups := map[dupKey]bool{}
	matched := map[string]bool{}
	var rank1 *Finding
	for i := range findings {
		f := findings[i]
		if f.Rank < 1 || f.Rank > len(findings) || ranks[f.Rank] {
			return false
		}
		ranks[f.Rank] = true
		k := dupKey{f.File, f.CausalLine, f.DefectClass, f.CauseCode, f.ImpactCode}
		if dups[k] {
			return false
		}
		dups[k] = true
		id := matchDefect(q, f)
		if id == "" || matched[id] {
			return false
		}
		matched[id] = true
		if f.Rank == 1 {
			rank1 = &findings[i]
		}
	}
	return rank1 != nil && matchDefect(q, *rank1) == q.PrimaryDefectID && len(matched) == len(q.MandatoryDefects)
}

func matchDefect(q Q6Oracle, f Finding) string {
	for _, d := range q.MandatoryDefects {
		if FindingFor(f.Rank, d) == f {
			return d.DefectID
		}
	}
	return ""
}

func imputed(m Manifest, task string) Metrics {
	var x Metrics
	for _, r := range m.ClosedRoleSet {
		x.M1U += roleCap(m.TokenCaps, task, r)
		x.M2 += roleCap(m.CallCaps, task, r)
	}
	x.M3 = 1
	x.Q6 = Q6Value{Applicable: task == "C"}
	return x
}

func validOutcome(o RequestOutcome) bool {
	switch o {
	case OutcomeSuccess, OutcomeRetryable, OutcomeRateLimited, OutcomePolicyFailure, OutcomeTerminalFailure, OutcomeSanction:
		return true
	}
	return false
}

func validTerminal(s TerminalState) bool {
	switch s {
	case StateCompleted, StateFailedWorker, StateFailedReviewer, StateTimeout, StateProviderRetryExhausted, StateProviderRateLimited, StateProviderPolicyFailure, StateProviderTerminalFailure, StateProviderSanction, StateMalformedResult, StateBlocked:
		return true
	}
	return false
}

func qualityOK(ps []PositionReport) bool {
	for _, task := range taskOrder {
		for _, which := range []string{"Q1", "Q4", "Q6"} {
			if which == "Q6" && task != "C" {
				continue
			}
			off, asst := 0, 0
			for _, p := range ps {
				if p.Position.TaskID != task {
					continue
				}
				pass := false
				switch which {
				case "Q1":
					pass = p.Metrics.Q1
				case "Q4":
					pass = p.Metrics.Q4
				case "Q6":
					pass = p.Metrics.Q6.Applicable && p.Metrics.Q6.Pass
				}
				if pass {
					if p.Position.Arm == ArmOff {
						off++
					} else {
						asst++
					}
				}
			}
			if asst < off {
				return false
			}
		}
	}
	return true
}

func efficiencyOK(m Manifest, ps []PositionReport) bool {
	for _, task := range []string{"A", "B", "C"} {
		off := armMedians(ps, task, ArmOff)
		asst := armMedians(ps, task, ArmAssisted)
		if !improves(asst[0], off[0], m.Thresholds.M1UMaxRatio) || !(improves(asst[1], off[1], m.Thresholds.M2OrM3MaxRatio) || improves(asst[2], off[2], m.Thresholds.M2OrM3MaxRatio)) {
			return false
		}
	}
	return true
}

func neutralD(m Manifest, ps []PositionReport) bool {
	off := armMedians(ps, "D", ArmOff)
	asst := armMedians(ps, "D", ArmAssisted)
	for i := 0; i < 3; i++ {
		if off[i] == 0 {
			if asst[i] != 0 {
				return false
			}
			continue
		}
		r := asst[i] / off[i]
		if math.IsNaN(r) || r < m.Thresholds.DNeutralLower || r > m.Thresholds.DNeutralUpper {
			return false
		}
	}
	return true
}

// improves is improves(task,X,limit) of 06 §6: false when off == 0.
func improves(assisted, off, limit float64) bool {
	if off == 0 || math.IsNaN(off) || math.IsNaN(assisted) {
		return false
	}
	return assisted/off <= limit
}

func armMedians(ps []PositionReport, task string, arm Arm) [3]float64 {
	var a, b, c []float64
	for _, p := range ps {
		if p.Position.TaskID == task && p.Position.Arm == arm {
			a = append(a, float64(p.Metrics.M1U))
			b = append(b, float64(p.Metrics.M2))
			c = append(c, p.Metrics.M3)
		}
	}
	return [3]float64{median(a), median(b), median(c)}
}

func median(x []float64) float64 {
	sort.Float64s(x)
	if len(x) == 0 {
		return math.NaN()
	}
	n := len(x)
	if n%2 == 1 {
		return x[n/2]
	}
	return (x[n/2-1] + x[n/2]) / 2
}

func publishDiagnostics(m Manifest, r *Report) {
	agg := map[string]*CacheAggregate{}
	measured, missing := 0, 0
	for _, p := range r.Positions {
		k := p.Position.TaskID + "/" + string(p.Position.Arm)
		if agg[k] == nil {
			agg[k] = &CacheAggregate{TaskID: p.Position.TaskID, Arm: p.Position.Arm}
		}
		agg[k].CachedInputTokens += p.Diagnostics.CachedInputTokens
		agg[k].UncachedInputTokens += p.Diagnostics.UncachedInputTokens
		r.ResidualConfounder.ByPositionInOrder = append(r.ResidualConfounder.ByPositionInOrder, PositionCacheEntry{PositionIndex: p.Position.PositionIndex, TaskID: p.Position.TaskID, Arm: p.Position.Arm, CachedInputTokens: p.Diagnostics.CachedInputTokens, UncachedInputTokens: p.Diagnostics.UncachedInputTokens})
		measured += p.Diagnostics.Attempts
		for _, e := range p.Errors {
			if e == "attempt accounting MISSING" {
				missing++
			}
		}
	}
	for _, task := range taskOrder {
		for _, arm := range []Arm{ArmOff, ArmAssisted} {
			if a := agg[task+"/"+string(arm)]; a != nil {
				r.ResidualConfounder.ByTaskArm = append(r.ResidualConfounder.ByTaskArm, *a)
			}
			med := armMedians(r.Positions, task, arm)
			r.Medians = append(r.Medians, TaskArmMedians{TaskID: task, Arm: arm, M1U: finiteOrZero(med[0]), M2: finiteOrZero(med[1]), M3: finiteOrZero(med[2])})
		}
	}
	status := "AVAILABLE"
	if missing > 0 {
		status = fmt.Sprintf("PARTIAL: %d attempts with MISSING accounting", missing)
	}
	if measured == 0 {
		status = "UNAVAILABLE: no materialized attempts"
	}
	for _, s := range []string{"uncached_input_tokens", "input_tokens", "cached_input_tokens"} {
		r.SignalAvailability[s] = status
	}
	r.SignalAvailability["provider_requests"] = "AVAILABLE"
	r.SignalAvailability["exploration"] = "EXECUTOR_REPORTED:" + m.Instrument.ExplorationParserVersion
	r.SignalAvailability["cost"] = "UNAVAILABLE"
	r.SignalAvailability["duration"] = "PARTIAL: dispatch/finalization timestamps only"
}

func finiteOrZero(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

// AccountRefSet is the canonical digest of a per-provider account reference
// map; it is what provider.account_ref_sha256 freezes when one position uses
// several providers (Claude worker, Codex reviewer).
func AccountRefSet(refs map[string]string) (string, error) {
	b, err := CanonicalJSON(refs)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// accountAttested accepts an attempt whose observed account reference is the
// frozen one: either equal to provider.account_ref_sha256, or equal to its
// protocol's entry in a frozen per-provider map whose canonical digest is
// provider.account_ref_sha256.
func accountAttested(m Manifest, raw json.RawMessage) bool {
	var meta struct {
		Ref      string            `json:"account_ref_sha256"`
		Protocol string            `json:"provider_protocol"`
		Refs     map[string]string `json:"account_refs"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.Ref == "" {
		return false
	}
	if len(meta.Refs) == 0 {
		return meta.Ref == m.Provider.AccountRefSHA256
	}
	set, err := AccountRefSet(meta.Refs)
	return err == nil && set == m.Provider.AccountRefSHA256 && meta.Refs[meta.Protocol] == meta.Ref
}
