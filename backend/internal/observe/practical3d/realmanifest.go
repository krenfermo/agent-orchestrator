package practical3d

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CaptureAccountRef runs one minimal Claude Code request through a
// transparent local forwarder and returns SHA-256 of the
// anthropic-organization-id the provider answered with: the stable,
// non-secret account reference a manifest freezes.
func CaptureAccountRef(ctx context.Context, realClaude, upstream, model, workDir string) (string, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return "", err
	}
	var mu sync.Mutex
	org := ""
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = &http.Transport{Proxy: nil}
	base := rp.Director
	rp.Director = func(r *http.Request) { base(r); r.Host = u.Host }
	rp.ModifyResponse = func(resp *http.Response) error {
		if v := resp.Header.Get("Anthropic-Organization-Id"); v != "" {
			mu.Lock()
			org = v
			mu.Unlock()
		}
		return nil
	}
	srv := &http.Server{Handler: rp, ReadHeaderTimeout: 30 * time.Second}
	ln, err := netListenLoopback()
	if err != nil {
		return "", err
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, realClaude, "-p", "Reply with exactly: ok", "--model", model)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=http://"+ln.Addr().String(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	cmd.Stdin = strings.NewReader("")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("account probe: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mu.Lock()
	defer mu.Unlock()
	if org == "" {
		return "", errors.New("provider returned no organization id")
	}
	return sha256Hex([]byte(org)), nil
}

// RealMiniInputs are the frozen facts of a real-AO technical mini manifest.
type RealMiniInputs struct {
	AOCommit, FixtureCommit, AccountRefSHA256, ClaudeVersion string
	PrimaryModel, HelperModel                                string
	CodexModel, CodexVersion                                 string
	AccountRefs                                              map[string]string
	Env                                                      EnvironmentInputs
	TaskSpecs                                                map[string][]byte // task -> spec JSON
	Attachment                                               []byte            // frozen ASSISTED attachment (task A worker)
	AttachmentRef                                            string
	OracleScript                                             []byte
	HiddenManifests                                          map[string][]byte // task -> hidden manifest
	VerifyCommand                                            string
	FixtureSubtree                                           string
	ReviewTarget, ReviewFile                                 []byte
	ReviewFilePath                                           string
	ReviewCausalLine                                         int
	IndexedCommit                                            string
	PackDigest                                               string
}

// BuildRealMiniManifest assembles a technical manifest for a real-AO mini
// run: real provider/models/environment, client id marked technical so it
// can never run as (or be confused with) the official experiment.
func BuildRealMiniManifest(in RealMiniInputs) (Manifest, map[string][]byte, error) {
	blobs := map[string][]byte{}
	put := func(b []byte) string { d := sha256Hex(b); blobs[d] = b; return d }
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return Manifest{}, nil, err
	}
	present, absent := true, false
	roles := []Role{RoleWorker, RoleReviewer, RoleRepair}
	accountRef := in.AccountRefSHA256
	if len(in.AccountRefs) > 0 {
		var err error
		if accountRef, err = AccountRefSet(in.AccountRefs); err != nil {
			return Manifest{}, nil, err
		}
	}
	m := Manifest{
		SchemaVersion: ManifestSchemaVersion, ManifestSchemaSHA256: ExpectedManifestSchemaSHA256, Estimand: "project_memory_assisted_vs_off_v1",
		AOCommit: in.AOCommit, FixtureCommit: in.FixtureCommit, Arms: []Arm{ArmOff, ArmAssisted}, ClosedRoleSet: roles,
		Provider:               Provider{ProviderID: "anthropic+openai", AccountRefSHA256: accountRef, ClientID: "technical-claude-code-mini-real", ClientVersion: in.ClaudeVersion, ProviderAPIVersion: "2023-06-01"},
		ProviderAccessBoundary: "AO_OBSERVED_CLIENT_ONLY_V1",
		RetryPolicy:            RetryPolicy{AlgorithmVersion: "exponential_capped_v1"},
		Deadlines:              Deadlines{PositionSeconds: 3600, ProviderAttemptSeconds: 900},
		Instrument:             Instrument{SchemaVersion: "ao.3d-practical.instrument.v1", AttemptEventSchemaVersion: "v2", ExecutionEnvironmentDigestVersion: "v1", ProviderRequestSchemaVersion: ProviderProxyVersion, ExplorationParserVersion: M3ParserVersion, VerifyVersion: "v1", Q4RunnerVersion: "oracle.sh-v1", Q6ScorerVersion: Q6Version},
		Thresholds:             Thresholds{M1UMaxRatio: .85, M2OrM3MaxRatio: .80, DNeutralLower: .90, DNeutralUpper: 1.10, QualityRule: "pass_count_non_decrease_per_task"},
		DecisionRuleVersion:    DecisionRuleVersion, N: ExpectedN, Router: "OFF",
		ExternalContext:   ExternalContext{Policy: "DISABLED", EqualizedSources: []EqualizedSource{}},
		Q1Oracle:          Q1Oracle{Version: "v1", VerifyCommandSHA256: put([]byte(in.VerifyCommand)), AcceptedExitCodes: []int{0}},
		Q4Oracle:          Q4Oracle{Version: "v1", RunnerImageOrBinarySHA256: put(in.OracleScript), CommandSHA256: sha256Hex(in.OracleScript), TimeoutSeconds: 900},
		PositionIsolation: PositionIsolation{NewConversation: true, NewSessionID: true, CleanWorkingCopy: true, NewAODataDir: true, NewRuntimeProviderHomeWhenApplicable: true, NoPriorTranscriptMemoryOrResults: true, LocalProcessTeardown: "verified_before_next_position"},
	}
	m.ExecutionEnvironment = ExecutionEnvironment{DigestSchemaVersion: "ao.3d-practical.env.v1", Inputs: in.Env}
	d, err := EnvironmentDigest(m.ExecutionEnvironment.DigestSchemaVersion, in.Env)
	if err != nil {
		return Manifest{}, nil, err
	}
	m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest = d
	for _, role := range roles {
		m.RetryPolicy.RetryBudgets = append(m.RetryPolicy.RetryBudgets, RetryBudget{Role: role, RetryableMaxRetries: 4, RateLimitedMaxRetries: 4, BackoffPolicy: BackoffPolicy{Algorithm: "exponential_capped_v1", BaseDelayMS: 1000, MaxDelayMS: 30000, JitterAlgorithm: "none_v1"}})
		m.Deadlines.RoleSeconds = append(m.Deadlines.RoleSeconds, RoleDeadline{Role: role, Seconds: 3000})
	}
	attDigest := sha256Hex(in.Attachment)
	blobs[attDigest] = in.Attachment
	cfgFor := func(model string) (json.RawMessage, string) {
		raw := json.RawMessage(`{"claude_code_version":"` + in.ClaudeVersion + `","model":"` + model + `","permission_mode":"ao-default","stream":true,"tool_policy":"no_mcp_no_web"}`)
		c, _ := canonicalRaw(raw, noDecimals)
		return raw, sha256Hex(c)
	}
	classes := []CallClass{CallInitial, CallContinuation, CallToolResult, CallHelper, CallRetry}
	for _, task := range taskOrder {
		spec := in.TaskSpecs[task]
		if len(spec) == 0 {
			return Manifest{}, nil, fmt.Errorf("task spec %s missing", task)
		}
		hidden := put(in.HiddenManifests[task])
		m.Tasks = append(m.Tasks, Task{TaskID: task, TaskManifestSHA256: put(spec), FixtureSubtreeSHA256: in.FixtureSubtree, OracleRef: hidden, TreatmentTargetRoles: []Role{measuredRoleFor(task)}})
		m.Q4Oracle.TaskOracles = append(m.Q4Oracle.TaskOracles, TaskOracle{TaskID: task, HiddenTestManifestSHA256: hidden})
		// AO task run: worker (Claude), reviewer (Codex, AO's cross-provider
		// independence), fix in the worker's session (Claude).
		flow := []RoleFlow{{Role: RoleWorker, FlowPosition: 1, ReachableCallClasses: classes}, {Role: RoleReviewer, FlowPosition: 2, ReachableCallClasses: classes}, {Role: RoleRepair, FlowPosition: 3, ReachableCallClasses: classes}}
		m.Workflow.TaskRoles = append(m.Workflow.TaskRoles, TaskRoleFlow{TaskID: task, RoleFlow: flow})
		used := map[Role]bool{}
		for _, f := range flow {
			used[f.Role] = true
		}
		for _, r := range roles {
			tokens, calls, cc, fc := int64(0), int64(0), int64(0), int64(0)
			if used[r] {
				tokens, calls = 8_000_000, 400
			}
			if r == measuredRoleFor(task) {
				cc, fc = 60, 40
			}
			m.TokenCaps = append(m.TokenCaps, RoleCap{TaskID: task, Role: r, Cap: tokens})
			m.CallCaps = append(m.CallCaps, RoleCap{TaskID: task, Role: r, Cap: calls})
			m.M3Caps = append(m.M3Caps, M3Cap{TaskID: task, Role: r, CCap: cc, FCap: fc})
		}
		for _, f := range flow {
			for _, class := range classes {
				model := in.PrimaryModel
				assisted := TreatmentArm{AttachmentPresent: &present, AttachmentSHA256: attDigest, AttachmentArtifactRef: in.AttachmentRef, AttachmentVersion: "ao-project-memory-pack:" + in.PackDigest, ProvenanceSchemaVersion: "ao.project-memory.provenance.v1", ConstructionVersion: "projectmemory.provision.v1", RenderVersion: "projectmemory.render.v1", IndexedCommit: in.IndexedCommit, SourceManifestSHA256: in.FixtureSubtree, FreshnessInputsSHA256: sha256Hex([]byte(in.IndexedCommit)), Origin: "PROJECT_MEMORY"}
				if class == CallHelper && in.HelperModel != "" {
					// Claude Code's helper model is called with the user's
					// prompt, so in ASSISTED it carries the attachment too
					// (observed in the real mini-E2E).
					model = in.HelperModel
				}
				raw, sum := cfgFor(model)
				schema := EffectiveConfigSchemaClaudeCodeV1
				if f.Role == RoleReviewer {
					// Project Memory is targeted at the worker only
					// (AO_MEMORY_ROLES=worker): the reviewer gets none in
					// either arm, so its cells carry no attachment.
					assisted = TreatmentArm{AttachmentPresent: &absent}
					model = in.CodexModel
					raw = json.RawMessage(`{"codex_version":"` + in.CodexVersion + `","model":"` + model + `","sandbox_mode":"ao-reviewer","stream":true,"tool_policy":"no_mcp_no_web"}`)
					c, _ := canonicalRaw(raw, noDecimals)
					sum, schema = sha256Hex(c), EffectiveConfigSchemaCodexV1
				}
				m.TreatmentMapping = append(m.TreatmentMapping, TreatmentCell{TaskID: task, Role: f.Role, CallClass: class, OFF: TreatmentArm{AttachmentPresent: &absent}, ASSISTED: assisted})
				m.InvocationConfigs = append(m.InvocationConfigs, InvocationConfig{TaskID: task, Role: f.Role, CallClass: class, ModelID: model, ModelVersion: model, EffectiveConfigSchema: schema, EffectiveConfigSHA256: sum, EffectiveConfig: raw})
			}
		}
	}
	m.Workflow.WorkerFlowVersion, m.Workflow.ReviewerFlowVersion = "ao.task-run.v1", "ao.review.v1"
	other := sha256Hex([]byte("inspected: CLAUDE.md,AGENTS.md hooks disabled via --strict-mcp-config"))
	m.ContextSourceInventory = []ContextSource{{SourceID: "project_memory", State: ProjectMemoryInventoryState, VerificationVersion: "v1"}, {SourceID: "router", State: "OFF", VerificationVersion: "v1"}, {SourceID: "mcp", State: "DISABLED", VerificationVersion: "proxy-tools-v1"}, {SourceID: "apps_connectors", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "web", State: "DISABLED", VerificationVersion: "proxy-tools-v1"}, {SourceID: "global_memory", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "AO_external_evidence", State: "DISABLED", VerificationVersion: "run-policy-v1"}, {SourceID: "provider_tools", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "other", State: "DISABLED", VerificationVersion: "v1", InventorySHA256: other}}
	target := put(in.ReviewTarget)
	fileSHA := put(in.ReviewFile)
	fm, _ := json.Marshal(Q6FileManifest{Schema: Q6FileManifestSchema, ReviewTargetSHA256: target, Files: []Q6File{{File: in.ReviewFilePath, FileSHA256: fileSHA, LineCount: lineCount(in.ReviewFile)}}})
	m.Q6Oracle = Q6Oracle{Version: Q6Version, ReviewTargetSHA256: target, FileManifestSHA256: put(fm), PrimaryDefectID: "C-discount-after-tax", K: 3,
		MandatoryDefects: []MandatoryDefect{{DefectID: "C-discount-after-tax", TargetSHA256: target, File: in.ReviewFilePath, FileSHA256: fileSHA, CausalLine: in.ReviewCausalLine, DefectClass: "DATA_INTEGRITY", CauseCode: "WRONG_TARGET", ImpactCode: "INCORRECT_RESULT"}}}
	m.Randomization = Randomization{PRNGAlgorithm: "ChaCha20-IETF", PRNGVersion: "RFC8439-v1", SeedHex: hex.EncodeToString(seed), RootSeedGeneration: "OS_CSPRNG_32_bytes_before_manifest_v1", TaskStreamDerivation: "sha256-task-domain-v1", ScheduleAlgorithm: "task-paired-off-assisted-v1", ScheduleVersion: 1, OrientationBalance: "2_3_each_task"}
	if m.Randomization.Schedule, err = GenerateSchedule(m.Randomization); err != nil {
		return Manifest{}, nil, err
	}
	return m, blobs, ValidateManifest(m)
}

func netListenLoopback() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

func measuredRoleFor(task string) Role {
	if task == "C" {
		return RoleReviewer
	}
	return RoleWorker
}

// CaptureCodexAccountRef runs one minimal Codex request through a local
// forwarder (the same custom provider the positions use) and returns SHA-256
// of the ChatGPT account id the CLI authenticated as.
func CaptureCodexAccountRef(ctx context.Context, realCodex, upstream, model, workDir string) (string, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return "", err
	}
	var mu sync.Mutex
	acct := ""
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = &http.Transport{Proxy: nil}
	base := rp.Director
	rp.Director = func(r *http.Request) {
		if v := r.Header.Get("Chatgpt-Account-Id"); v != "" {
			mu.Lock()
			acct = v
			mu.Unlock()
		}
		base(r)
		r.Host = u.Host
	}
	srv := &http.Server{Handler: rp, ReadHeaderTimeout: 30 * time.Second}
	ln, err := netListenLoopback()
	if err != nil {
		return "", err
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := []string{"exec", "--skip-git-repo-check", "-c", `model_provider="p3d"`, "-c", `model_providers.p3d.name="p3d"`,
		"-c", `model_providers.p3d.base_url="http://` + ln.Addr().String() + `/backend-api/codex"`, "-c", `model_providers.p3d.wire_api="responses"`,
		"-c", "model_providers.p3d.requires_openai_auth=true", "-c", "model_providers.p3d.supports_websockets=false", "-c", `model="` + model + `"`, "Reply with exactly: ok"}
	cmd := exec.CommandContext(cctx, realCodex, args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader("")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("codex account probe: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mu.Lock()
	defer mu.Unlock()
	if acct == "" {
		return "", errors.New("codex sent no ChatGPT account id")
	}
	return sha256Hex([]byte(acct)), nil
}
