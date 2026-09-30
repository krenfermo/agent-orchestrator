package practical3d

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TechnicalFixtureProviderID marks manifests built by NewTechnicalFixture.
// Official runs refuse it and mini-E2E runs require it, so technical evidence
// can never be produced under an official experiment_id.
const TechnicalFixtureProviderID = "technical-fixture"

// ErrPrestartInvalid marks a batch that must not (and did not) start.
var ErrPrestartInvalid = errors.New("3d practical: PRESTART_INVALID")

// ArtifactResolver serves frozen, content-addressed artifacts.
type ArtifactResolver interface {
	ReadArtifact(ref string) ([]byte, error)
	ReadDigest(digest string) ([]byte, error)
}

// DirArtifactResolver serves treatment blobs by relative ref and every other
// frozen artifact from <Root>/sha256/<digest>.
type DirArtifactResolver struct{ Root string }

// ReadArtifact reads a treatment blob by its safe relative ref.
func (d DirArtifactResolver) ReadArtifact(ref string) ([]byte, error) {
	if !safeRelative(ref) {
		return nil, fmt.Errorf("unsafe artifact ref %q", ref)
	}
	return d.readContained(filepath.Join(d.Root, filepath.FromSlash(ref)))
}

// readContained refuses a root inside production AO data, and any file that
// is not a regular file resolving (through symlinks) inside the root.
func (d DirArtifactResolver) readContained(path string) ([]byte, error) {
	if err := RefuseProductionPath(d.Root); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(d.Root)
	if err != nil {
		return nil, err
	}
	if err := RefuseProductionPath(root); err != nil {
		return nil, err
	}
	if prod, err := productionDataDir(); err == nil && (within(prod, root) || within(resolveOrSelf(prod), root)) {
		return nil, fmt.Errorf("%w: artifact root %s contains production AO data", ErrUnsafeRoot, root)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if err := RefuseProductionPath(resolved); err != nil {
		return nil, err
	}
	if !within(resolved, root) {
		return nil, fmt.Errorf("artifact %s escapes the artifact root", path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact %s is not a regular file", path)
	}
	return os.ReadFile(resolved)
}

// ReadDigest reads <Root>/sha256/<digest>.
func (d DirArtifactResolver) ReadDigest(digest string) ([]byte, error) {
	if !validSHA256(digest) {
		return nil, fmt.Errorf("invalid content digest")
	}
	return d.readContained(filepath.Join(d.Root, "sha256", digest))
}

// TransportRequest carries the immutable canonical request bytes. The
// transport must deliver exactly these bytes; they are the traced snapshot.
type TransportRequest struct {
	CanonicalRequest []byte
	Position         Position
	Workspace        PositionWorkspace
}

// Transport delivers one canonical request to the provider SDK/CLI.
type Transport interface {
	Do(context.Context, TransportRequest) (ProviderResponse, error)
}

// CellRepresentation is the final representation an executor would build for
// one initial treatment cell; the preflight validates every one of them.
type CellRepresentation struct {
	TaskID         string         `json:"task_id"`
	Role           Role           `json:"role"`
	CallClass      CallClass      `json:"call_class"`
	Arm            Arm            `json:"arm"`
	Representation Representation `json:"representation"`
}

// PositionExecutor runs one position's roles. It never holds a provider SDK
// handle or credential: its only provider access is the ObservedClient.
type PositionExecutor interface {
	Preflight(context.Context, Manifest) ([]CellRepresentation, error)
	Execute(context.Context, PositionContext, *ObservedClient) (ExecutionResult, error)
}

// WorkspaceManager prepares a clean working copy and, in Finalize, verifies
// local teardown before the next position may start.
type WorkspaceManager interface {
	Prepare(context.Context, Manifest, Position, PositionWorkspace) error
	Finalize(context.Context, Manifest, Position, PositionWorkspace) error
}

// OracleRunner evaluates Q1/Q4 outside the agent's reach.
type OracleRunner interface {
	Evaluate(context.Context, PositionContext, ExecutionResult) (OracleResult, error)
}

// PositionContext is what an executor learns about its position.
type PositionContext struct {
	ExperimentID string            `json:"experiment_id"`
	Position     Position          `json:"position"`
	Workspace    PositionWorkspace `json:"workspace"`
}

// PositionWorkspace holds the fresh per-position paths under the run root.
type PositionWorkspace struct {
	Root         string `json:"root"`
	AODataDir    string `json:"ao_data_dir"`
	RuntimeHome  string `json:"runtime_home"`
	WorkingCopy  string `json:"working_copy"`
	TaskManifest string `json:"task_manifest"`
}

// RunnerOptions wires the runner to its environment and drivers.
type RunnerOptions struct {
	Root        string
	Metadata    EnvelopeMetadata
	Environment EnvironmentObserver
	Artifacts   ArtifactResolver
	Transport   Transport
	Executor    PositionExecutor
	Oracle      OracleRunner
	Workspaces  WorkspaceManager
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	// AllowExplicitTemp additionally permits roots below os.TempDir(); only
	// tests set it. Production AO data is refused regardless.
	AllowExplicitTemp bool
	// MiniE2E executes only the first MiniPositions scheduled positions of a
	// technical-fixture manifest, then records BATCH_STOP and blocks the rest.
	MiniE2E       bool
	MiniPositions int

	// allowTechnicalFullRun lets in-package tests run all 40 positions of a
	// technical-fixture manifest to exercise the complete decision path.
	allowTechnicalFullRun bool
}

// RunResult is the run directory and its decided report.
type RunResult struct {
	Root   string
	Report Report
}

type runner struct {
	m       Manifest
	o       RunnerOptions
	id      string
	root    string
	ledger  *Ledger
	reg     Registry
	spans   map[string][][]byte
	started bool
}

// Run executes the frozen schedule sequentially. Before the first SAMPLE_START
// any invalid input records PRESTART_INVALID and nothing starts; after it,
// every position ends in exactly one terminal state and the full lineage is
// decided. There is no position retry, replacement, relot or selective rerun:
// a run root is never reused and a registered experiment_id never runs again.
func Run(ctx context.Context, m Manifest, o RunnerOptions) (RunResult, error) {
	if o.Environment == nil || o.Artifacts == nil || o.Transport == nil || o.Executor == nil || o.Oracle == nil || o.Workspaces == nil {
		return RunResult{}, errors.New("3d practical: environment, artifacts, transport, executor, oracle, and workspace manager are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	if !o.allowTechnicalFullRun && o.MiniE2E != (m.Provider.ProviderID == TechnicalFixtureProviderID) {
		return RunResult{}, fmt.Errorf("%w: mini-E2E runs only technical-fixture manifests and official runs never do", ErrPrestartInvalid)
	}
	abs, err := ValidateRunRoot(o.Root, o.AllowExplicitTemp)
	if err != nil {
		return RunResult{}, err
	}
	id, err := ExperimentID(m)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: manifest is not canonicalizable: %w", ErrPrestartInvalid, err)
	}
	reg, err := registryFor(abs)
	if err != nil {
		return RunResult{}, err
	}
	if seen, err := reg.Contains(id); err != nil {
		return RunResult{}, err
	} else if seen {
		return RunResult{}, fmt.Errorf("%w: %s (registry %s)", ErrExperimentRegistered, id, reg.Path())
	}
	if err := reg.Claim(id); err != nil {
		return RunResult{}, err
	}
	ledger, root, err := CreateRunDirectory(abs, m, o.Metadata, o.AllowExplicitTemp)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { _ = ledger.Close() }()
	kind := runKind(o)
	if err := reg.Append(RegistryEntry{Type: "REGISTERED", ExperimentID: id, RunRoot: root, Kind: kind, Timestamp: o.Now().UTC()}); err != nil {
		return RunResult{}, err
	}
	canonical, _ := CanonicalManifest(m)
	if err := ledger.Append(Event{Type: EventManifest, ExperimentID: id, Timestamp: o.Now().UTC(), ManifestSHA256: id, Manifest: canonical}); err != nil {
		return RunResult{}, err
	}
	r := &runner{m: m, o: o, id: id, root: root, ledger: ledger, reg: reg}
	if err := r.preflight(ctx); err != nil {
		return r.prestartInvalid(err)
	}
	if err := r.runSchedule(ctx); err != nil {
		if !r.started && errors.Is(err, ErrPrestartInvalid) {
			return r.prestartInvalid(err)
		}
		if r.started {
			return r.failAfterStart(err)
		}
		return RunResult{Root: root}, err
	}
	res, err := r.decide()
	if err != nil && res.Report.Verdict == "" {
		return r.failAfterStart(err)
	}
	return res, err
}

func (r *runner) preflight(ctx context.Context) error {
	if err := ValidateManifest(r.m); err != nil {
		return err
	}
	observed, err := r.o.Environment.Observe(ctx)
	if err != nil {
		return fmt.Errorf("%w: observe preflight environment: %w", ErrPrestartInvalid, err)
	}
	digest, err := EnvironmentDigest(r.m.ExecutionEnvironment.DigestSchemaVersion, observed)
	if err != nil {
		return err
	}
	if digest != r.m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest {
		return fmt.Errorf("%w: execution environment digest %s differs from expected", ErrPrestartInvalid, digest)
	}
	spans, err := preflightArtifacts(r.m, r.o.Artifacts, filepath.Join(r.root, "artifacts"))
	if err != nil {
		return err
	}
	r.spans = spans
	if err := r.preflightRepresentations(ctx); err != nil {
		return err
	}
	return r.ledger.Append(Event{Type: EventPreflight, ExperimentID: r.id, Timestamp: r.o.Now().UTC(), ObservedDigest: digest})
}

// preflightRepresentations validates the final representation of every
// initial treatment cell in both arms before the batch (3d-practical §3).
func (r *runner) preflightRepresentations(ctx context.Context) error {
	reps, err := r.o.Executor.Preflight(ctx, r.m)
	if err != nil {
		return fmt.Errorf("%w: executor preflight: %w", ErrPrestartInvalid, err)
	}
	type key struct {
		task  string
		role  Role
		class CallClass
		arm   Arm
	}
	got := map[key]int{}
	for _, rep := range reps {
		k := key{rep.TaskID, rep.Role, rep.CallClass, rep.Arm}
		got[k]++
		if rep.Arm != ArmOff && rep.Arm != ArmAssisted {
			return fmt.Errorf("%w: preflight representation has unknown arm", ErrPrestartInvalid)
		}
		if _, _, err := traceRepresentation(r.m, r.spans, rep.TaskID, rep.Arm, rep.Role, rep.CallClass, rep.Representation); err != nil {
			return fmt.Errorf("%w: preflight representation %s/%s/%s/%s: %w", ErrPrestartInvalid, rep.TaskID, rep.Role, rep.CallClass, rep.Arm, err)
		}
	}
	want := 0
	for _, cell := range r.m.TreatmentMapping {
		if cell.CallClass != CallInitial {
			continue
		}
		for _, arm := range []Arm{ArmOff, ArmAssisted} {
			want++
			if got[key{cell.TaskID, cell.Role, cell.CallClass, arm}] != 1 {
				return fmt.Errorf("%w: preflight needs exactly one representation for %s/%s/%s/%s", ErrPrestartInvalid, cell.TaskID, cell.Role, cell.CallClass, arm)
			}
		}
	}
	if len(reps) != want {
		return fmt.Errorf("%w: preflight returned representations outside the initial cells", ErrPrestartInvalid)
	}
	return nil
}

func (r *runner) prestartInvalid(cause error) (RunResult, error) {
	now := r.o.Now().UTC()
	_ = r.ledger.Append(Event{Type: EventPrestartInvalid, ExperimentID: r.id, Timestamp: now, ReasonCode: ReasonPrestartInvalid, Reason: cause.Error()})
	_ = r.ledger.Append(Event{Type: EventDecision, ExperimentID: r.id, Timestamp: now, Decision: "NO_GO", ReasonCode: ReasonPrestartInvalid, Reason: cause.Error()})
	_ = r.ledger.Close()
	digest, _ := sealLedger(filepath.Join(r.root, "ledger.jsonl"))
	report := Report{SchemaVersion: ReportSchemaVersion, ExperimentID: r.id, Verdict: "NO_GO", ReasonCode: ReasonPrestartInvalid, Reason: cause.Error(), GeneratedAt: now, SignalAvailability: map[string]string{}, ResidualConfounder: ResidualConfounder{Present: true, Statement: "batch never started"}}
	_ = WriteReport(filepath.Join(r.root, "report.json"), report)
	_ = r.reg.Append(RegistryEntry{Type: "RESULT", ExperimentID: r.id, RunRoot: r.root, Kind: "PRESTART_INVALID", Timestamp: now, Verdict: "NO_GO", ReasonCode: ReasonPrestartInvalid, LedgerSHA256: digest})
	if errors.Is(cause, ErrPrestartInvalid) {
		return RunResult{Root: r.root, Report: report}, cause
	}
	return RunResult{Root: r.root, Report: report}, fmt.Errorf("%w: %w", ErrPrestartInvalid, cause)
}

func (r *runner) runSchedule(ctx context.Context) error {
	schedule := r.m.Randomization.Schedule
	limit := len(schedule)
	if r.o.MiniE2E {
		limit = r.o.MiniPositions
		if limit <= 0 {
			limit = 1
		}
	}
	stopped := false
	stopNext := ""
	for i, p := range schedule {
		if !stopped {
			reason := stopNext
			if reason == "" && ctx.Err() != nil {
				reason = "runner context ended: " + ctx.Err().Error()
			}
			if reason == "" && i >= limit {
				reason = "technical mini-E2E deliberately stops; official positions are not executed"
			}
			if reason != "" {
				if i == 0 {
					return fmt.Errorf("%w: stopped before the first SAMPLE_START: %s", ErrPrestartInvalid, reason)
				}
				prev := schedule[i-1]
				if err := r.ledger.Append(Event{Type: EventBatchStop, ExperimentID: r.id, Timestamp: r.o.Now().UTC(), SampleID: prev.SampleID, PositionIndex: prev.PositionIndex, TaskID: prev.TaskID, Arm: prev.Arm, Reason: reason}); err != nil {
					return err
				}
				stopped = true
			}
		}
		if stopped {
			if err := r.ledger.Append(Event{Type: EventPositionTerminal, ExperimentID: r.id, Timestamp: r.o.Now().UTC(), SampleID: p.SampleID, PositionIndex: p.PositionIndex, TaskID: p.TaskID, Arm: p.Arm, TerminalState: StateBlocked, Reason: "BLOCKED_BY_PRIOR_POSITION"}); err != nil {
				return err
			}
			continue
		}
		state, teardownErr, err := r.runPosition(ctx, p)
		if err != nil {
			return err
		}
		if state == StateProviderSanction {
			stopNext = "PROVIDER_SANCTION at position " + fmt.Sprint(p.PositionIndex)
		}
		if teardownErr != nil {
			stopNext = fmt.Sprintf("local teardown not verified after position %d: %v", p.PositionIndex, teardownErr)
		}
	}
	return nil
}

type positionKey struct{}

// PositionFromContext reports the position an environment observation or
// provider call belongs to (absent for the batch preflight).
func PositionFromContext(ctx context.Context) (Position, bool) {
	p, ok := ctx.Value(positionKey{}).(Position)
	return p, ok
}

func (r *runner) observe(ctx context.Context) (string, error) {
	inputs, err := r.o.Environment.Observe(ctx)
	if err != nil {
		return "", err
	}
	return EnvironmentDigest(r.m.ExecutionEnvironment.DigestSchemaVersion, inputs)
}

func (r *runner) event(p Position, typ EventType) Event {
	return Event{Type: typ, ExperimentID: r.id, Timestamp: r.o.Now().UTC(), SampleID: p.SampleID, PositionIndex: p.PositionIndex, TaskID: p.TaskID, Arm: p.Arm}
}

// runPosition returns the terminal state it recorded. A returned error is a
// ledger failure (or, before the first SAMPLE_START, PRESTART_INVALID); the
// decision then sees an incomplete lineage and is NO_GO.
func (r *runner) runPosition(ctx context.Context, p Position) (TerminalState, error, error) { //nolint:revive // (state, teardown error, fatal error)
	ctx = context.WithValue(ctx, positionKey{}, p)
	expected := r.m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest
	preDigest, obsErr := r.observe(ctx)
	pre := r.event(p, EventEnvironment)
	pre.Phase, pre.ObservedDigest = "PRE_START", preDigest
	if err := r.ledger.Append(pre); err != nil {
		return "", nil, err
	}
	if !r.started && (obsErr != nil || preDigest != expected) {
		if obsErr == nil {
			obsErr = fmt.Errorf("observed digest %s", preDigest)
		}
		return "", nil, fmt.Errorf("%w: environment diverged before the first SAMPLE_START: %w", ErrPrestartInvalid, obsErr)
	}
	workspace, setupErr := createPositionWorkspace(r.root, p)
	if setupErr == nil {
		setupErr = materializeTaskManifest(r.m, p, workspace, r.o.Artifacts)
	}
	if setupErr == nil {
		setupErr = r.o.Workspaces.Prepare(ctx, r.m, p, workspace)
	}
	if setupErr != nil && !r.started {
		return "", nil, fmt.Errorf("%w: position workspace: %w", ErrPrestartInvalid, setupErr)
	}
	if err := r.ledger.Append(r.event(p, EventSampleStart)); err != nil {
		return "", nil, err
	}
	r.started = true

	var state TerminalState
	var reason string
	result := PositionResult{}
	var execution ExecutionResult
	switch {
	case setupErr != nil:
		state, reason = StateMalformedResult, "position setup after SAMPLE_START: "+setupErr.Error()
	case obsErr != nil || preDigest != expected:
		state, reason = StateMalformedResult, "execution environment diverged before SAMPLE_START"
	default:
		positionCtx, cancel := context.WithTimeout(ctx, time.Duration(r.m.Deadlines.PositionSeconds)*time.Second)
		pc := PositionContext{ExperimentID: r.id, Position: p, Workspace: workspace}
		client := newObservedClient(positionCtx, r.m, p, workspace, r.id, r.ledger, r.o.Transport, r.o.Now, r.o.Sleep, r.spans)
		client.observe = r.observe
		var execErr error
		execution, execErr = r.o.Executor.Execute(positionCtx, pc, client)
		timedOut := errors.Is(positionCtx.Err(), context.DeadlineExceeded)
		client.close()
		cancel()
		state, reason = classify(execution.TerminalState, execErr, client, timedOut)
		result = PositionResult{ExplorationCalls: execution.ExplorationCalls, DistinctFilesRead: execution.DistinctFilesRead, MilestoneObserved: execution.MilestoneObserved, Findings: execution.Findings}
		if state == StateCompleted {
			q1, q4, oracleErr := r.runOracle(ctx, pc, execution)
			if oracleErr != nil {
				state, reason = StateMalformedResult, "oracle: "+oracleErr.Error()
			} else {
				result.Q1, result.Q4 = &q1, &q4
			}
		}
	}
	teardownErr := r.o.Workspaces.Finalize(ctx, r.m, p, workspace)
	if teardownErr != nil {
		state, reason = StateMalformedResult, "teardown verification: "+teardownErr.Error()
	}
	postDigest, postErr := r.observe(ctx)
	if postErr != nil || postDigest != expected {
		state, reason = StateMalformedResult, "execution environment diverged before terminal"
	}
	post := r.event(p, EventEnvironment)
	post.Phase, post.ObservedDigest = "PRE_TERMINAL", postDigest
	if err := r.ledger.Append(post); err != nil {
		return "", nil, err
	}
	result.TerminalState = state
	res := r.event(p, EventPositionResult)
	res.Result = &result
	if err := r.ledger.Append(res); err != nil {
		return "", nil, err
	}
	term := r.event(p, EventPositionTerminal)
	term.TerminalState, term.Reason = state, reason
	if err := r.ledger.Append(term); err != nil {
		return "", nil, err
	}
	return state, teardownErr, nil
}

// classify maps executor/client evidence onto the single terminal enum.
// Precedence: instrument/treatment malformation, then the provider failure
// the client observed first, then the frozen deadline, then executor errors.
// An executor may claim FAILED_WORKER/FAILED_REVIEWER/COMPLETED; provider
// and timeout states are only accepted from the runner's own evidence.
func classify(claimed TerminalState, execErr error, c *ObservedClient, timedOut bool) (TerminalState, string) {
	switch {
	case c.malformed != "":
		return StateMalformedResult, c.malformed
	case c.failure != "":
		return c.failure, "provider/role outcome observed by ObservedClient"
	case timedOut:
		return StateTimeout, "position deadline exceeded"
	case execErr != nil:
		return StateMalformedResult, "executor: " + execErr.Error()
	}
	switch claimed {
	case StateCompleted, StateFailedWorker, StateFailedReviewer:
		return claimed, ""
	}
	return StateMalformedResult, fmt.Sprintf("executor claimed terminal state %q without runner evidence", claimed)
}

func (r *runner) runOracle(ctx context.Context, pc PositionContext, execution ExecutionResult) (bool, bool, error) {
	oracle, err := r.o.Oracle.Evaluate(ctx, pc, execution)
	if err != nil {
		return false, false, err
	}
	expectedQ4 := ""
	for _, t := range r.m.Q4Oracle.TaskOracles {
		if t.TaskID == pc.Position.TaskID {
			expectedQ4 = t.HiddenTestManifestSHA256
		}
	}
	if oracle.Q4TaskOracleSHA256 != expectedQ4 || oracle.Q4CommandSHA256 != r.m.Q4Oracle.CommandSHA256 || oracle.Q1VerifyCommandSHA256 != r.m.Q1Oracle.VerifyCommandSHA256 {
		return false, false, errors.New("oracle evidence does not echo the frozen Q1/Q4 digests")
	}
	return acceptedExit(r.m.Q1Oracle.AcceptedExitCodes, oracle.Q1ExitCode), oracle.Q4Passed, nil
}

func (r *runner) decide() (RunResult, error) {
	if err := r.ledger.Close(); err != nil {
		return RunResult{Root: r.root}, err
	}
	path := filepath.Join(r.root, "ledger.jsonl")
	report := EvaluateLedgerFile(r.m, path, r.o.Now().UTC())
	l, err := reopenLedger(path)
	if err != nil {
		return RunResult{Root: r.root}, err
	}
	err = l.Append(Event{Type: EventDecision, ExperimentID: r.id, Timestamp: r.o.Now().UTC(), Decision: report.Verdict, ReasonCode: report.ReasonCode, Reason: report.Reason})
	if cerr := l.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return RunResult{Root: r.root}, err
	}
	// The report is published only after the ledger is sealed and its digest
	// is durably anchored; a failure before that publishes NO_GO instead.
	digest, err := sealLedger(path)
	if err != nil {
		return RunResult{Root: r.root}, err
	}
	kind := runKind(r.o)
	if err := r.reg.Append(RegistryEntry{Type: "RESULT", ExperimentID: r.id, RunRoot: r.root, Kind: kind, Timestamp: r.o.Now().UTC(), Verdict: report.Verdict, ReasonCode: report.ReasonCode, LedgerSHA256: digest}); err != nil {
		return RunResult{Root: r.root}, err
	}
	if err := WriteReport(filepath.Join(r.root, "report.json"), report); err != nil {
		return RunResult{Root: r.root, Report: report}, err
	}
	return RunResult{Root: r.root, Report: report}, nil
}

// failAfterStart handles a runner/ledger failure after the first
// SAMPLE_START: no position is omitted silently. It publishes the NO_GO
// decision over whatever the ledger holds (missing terminals are malformed
// and invalidate the lineage) and anchors it in the registry.
func (r *runner) failAfterStart(cause error) (RunResult, error) {
	_ = r.ledger.Close()
	path := filepath.Join(r.root, "ledger.jsonl")
	report := EvaluateLedgerFile(r.m, path, r.o.Now().UTC())
	report.Verdict = "NO_GO"
	if report.ReasonCode == ReasonAllConditions || report.ReasonCode == "" {
		report.ReasonCode = ReasonLineageInvalid
	}
	report.Reason = "runner failure after SAMPLE_START: " + cause.Error() + "; " + report.Reason
	digest, sealErr := sealLedger(path)
	regErr := r.reg.Append(RegistryEntry{Type: "RESULT", ExperimentID: r.id, RunRoot: r.root, Kind: "RUNNER_FAILURE_AFTER_START", Timestamp: r.o.Now().UTC(), Verdict: "NO_GO", ReasonCode: report.ReasonCode, LedgerSHA256: digest})
	reportPath := filepath.Join(r.root, "report.json")
	_ = os.Remove(reportPath) // never leave an earlier, unanchored report in place
	repErr := WriteReport(reportPath, report)
	return RunResult{Root: r.root, Report: report}, errors.Join(cause, sealErr, regErr, repErr)
}

// sealLedger makes the finished ledger read-only and returns its digest,
// which the registry anchors.
func sealLedger(path string) (string, error) {
	if err := os.Chmod(path, 0o400); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

// registryFor returns the single registry for every run under the Frente 3
// scratch root, so choosing another parent directory cannot hide a previous
// run of the same experiment_id. Explicit temp roots (tests only) use their
// parent directory.
func registryFor(runRoot string) (Registry, error) {
	scratch, err := ScratchRoot()
	if err != nil {
		return Registry{}, err
	}
	if within(runRoot, scratch) || within(resolveOrSelf(filepath.Dir(runRoot)), resolveOrSelf(scratch)) {
		return OpenRegistry(scratch), nil
	}
	return OpenRegistry(filepath.Dir(runRoot)), nil
}

func runKind(o RunnerOptions) string {
	switch {
	case o.MiniE2E:
		return "TECHNICAL_MINI_E2E"
	case o.allowTechnicalFullRun:
		return "TECHNICAL_TEST"
	}
	return "OFFICIAL"
}

func createPositionWorkspace(root string, p Position) (PositionWorkspace, error) {
	dir := filepath.Join(root, "positions", fmt.Sprintf("%02d-%s", p.PositionIndex, p.SampleID[:12]))
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return PositionWorkspace{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return PositionWorkspace{}, err
	}
	w := PositionWorkspace{Root: dir, AODataDir: filepath.Join(dir, "ao-data"), RuntimeHome: filepath.Join(dir, "runtime-home"), WorkingCopy: filepath.Join(dir, "work"), TaskManifest: filepath.Join(dir, "task-manifest")}
	for _, x := range []string{w.AODataDir, w.RuntimeHome} {
		if err := os.Mkdir(x, 0o700); err != nil {
			return PositionWorkspace{}, err
		}
	}
	return w, nil
}

func materializeTaskManifest(m Manifest, p Position, w PositionWorkspace, r ArtifactResolver) error {
	digest := ""
	for _, task := range m.Tasks {
		if task.TaskID == p.TaskID {
			digest = task.TaskManifestSHA256
		}
	}
	b, err := r.ReadDigest(digest)
	if err != nil {
		return err
	}
	if sha256Hex(b) != digest {
		return errors.New("task manifest digest mismatch")
	}
	return writeExclusive(w.TaskManifest, b)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func acceptedExit(codes []int, got int) bool {
	for _, code := range codes {
		if code == got {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
