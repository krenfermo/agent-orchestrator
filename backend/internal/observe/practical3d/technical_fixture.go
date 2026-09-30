package practical3d

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// TechnicalArtifacts are the content-addressed blobs a technical fixture
// manifest refers to.
type TechnicalArtifacts struct {
	AttachmentRef string
	Attachment    []byte
	Blobs         map[string][]byte
}

// Write stores the artifacts in the DirArtifactResolver layout.
func (a TechnicalArtifacts) Write(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "sha256"), 0o700); err != nil {
		return err
	}
	for d, b := range a.Blobs {
		if err := writeExclusive(filepath.Join(dir, "sha256", d), b); err != nil {
			return err
		}
	}
	return writeExclusive(filepath.Join(dir, a.AttachmentRef), a.Attachment)
}

// TechnicalReviewedFile is the task C file whose line 3 carries the seeded
// technical-fixture defect.
var TechnicalReviewedFile = []byte("package fixture\n\nfunc Allowed(role string) bool { return true }\n")

// NewTechnicalFixture creates deterministic local-only inputs for tests and
// the mini-E2E. Its provider is TechnicalFixtureProviderID, which official
// runs refuse, so it can never become an official experiment manifest.
func NewTechnicalFixture(seedHex string) (Manifest, EnvironmentInputs, TechnicalArtifacts, error) {
	if len(seedHex) != 64 {
		return Manifest{}, EnvironmentInputs{}, TechnicalArtifacts{}, fmt.Errorf("seed must be 32 bytes")
	}
	art := TechnicalArtifacts{AttachmentRef: "attachment.bin", Attachment: []byte("technical fixture Project Memory attachment: module map, entry points and test layout; never official evidence\n"), Blobs: map[string][]byte{}}
	blob := func(s string) string {
		b := []byte(s)
		d := sha256Hex(b)
		art.Blobs[d] = b
		return d
	}
	commit := "0123456789abcdef0123456789abcdef01234567"
	present, absent := true, false
	zero := sha256Hex([]byte("fixture-zero"))
	fileSHA := blob(string(TechnicalReviewedFile))
	reviewTarget := blob("technical review target diff")
	fm, _ := json.Marshal(Q6FileManifest{Schema: Q6FileManifestSchema, ReviewTargetSHA256: reviewTarget, Files: []Q6File{{File: "fixture.go", FileSHA256: fileSHA, LineCount: lineCount(TechnicalReviewedFile)}}})
	fileManifest := blob(string(fm))
	env := EnvironmentInputs{OSPlatformArch: OSPlatformArch{OS: "fixture", Platform: "fixture", Arch: "fixture"}, AOBinarySHA256: zero, AOCommit: commit, RuntimeVersions: []VersionInput{}, ProviderClientCLIVersions: []VersionInput{}, TaskToolVersions: []VersionInput{}, EffectiveEnvironmentConfigAllowlist: []ConfigInput{}, RunnerInstrumentVersions: []VersionInput{}, AdditionalLocalConfiguration: []ConfigInput{}}
	m := Manifest{
		SchemaVersion: ManifestSchemaVersion, ManifestSchemaSHA256: ExpectedManifestSchemaSHA256, Estimand: "project_memory_assisted_vs_off_v1",
		AOCommit: commit, FixtureCommit: commit, Arms: []Arm{ArmOff, ArmAssisted}, ClosedRoleSet: []Role{RoleWorker, RoleReviewer},
		Provider:               Provider{ProviderID: TechnicalFixtureProviderID, AccountRefSHA256: zero, ClientID: "fixture", ClientVersion: "1", ProviderAPIVersion: "1"},
		ProviderAccessBoundary: "AO_OBSERVED_CLIENT_ONLY_V1",
		RetryPolicy:            RetryPolicy{AlgorithmVersion: "exponential_capped_v1"},
		Deadlines:              Deadlines{PositionSeconds: 30, ProviderAttemptSeconds: 10},
		Instrument:             Instrument{SchemaVersion: "v1", AttemptEventSchemaVersion: "v1", ExecutionEnvironmentDigestVersion: "v1", ProviderRequestSchemaVersion: "v1", ExplorationParserVersion: "v1", VerifyVersion: "v1", Q4RunnerVersion: "v1", Q6ScorerVersion: "v1"},
		Thresholds:             Thresholds{M1UMaxRatio: .85, M2OrM3MaxRatio: .80, DNeutralLower: .90, DNeutralUpper: 1.10, QualityRule: "pass_count_non_decrease_per_task"},
		DecisionRuleVersion:    DecisionRuleVersion, N: ExpectedN, Router: "OFF",
		ExternalContext:   ExternalContext{Policy: "DISABLED", EqualizedSources: []EqualizedSource{}},
		Q1Oracle:          Q1Oracle{Version: "v1", VerifyCommandSHA256: blob("technical verify command"), AcceptedExitCodes: []int{0}},
		Q4Oracle:          Q4Oracle{Version: "v1", RunnerImageOrBinarySHA256: zero, CommandSHA256: blob("technical q4 command"), TimeoutSeconds: 10},
		Q6Oracle:          Q6Oracle{Version: Q6Version, ReviewTargetSHA256: reviewTarget, FileManifestSHA256: fileManifest, PrimaryDefectID: "d1", K: 3},
		PositionIsolation: PositionIsolation{NewConversation: true, NewSessionID: true, CleanWorkingCopy: true, NewAODataDir: true, NewRuntimeProviderHomeWhenApplicable: true, NoPriorTranscriptMemoryOrResults: true, LocalProcessTeardown: "verified_before_next_position"},
	}
	m.ExecutionEnvironment = ExecutionEnvironment{DigestSchemaVersion: "fixture-env-v1", Inputs: env}
	digest, err := EnvironmentDigest(m.ExecutionEnvironment.DigestSchemaVersion, env)
	if err != nil {
		return Manifest{}, EnvironmentInputs{}, TechnicalArtifacts{}, err
	}
	m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest = digest
	for _, role := range m.ClosedRoleSet {
		m.RetryPolicy.RetryBudgets = append(m.RetryPolicy.RetryBudgets, RetryBudget{Role: role, RetryableMaxRetries: 1, RateLimitedMaxRetries: 1, BackoffPolicy: BackoffPolicy{Algorithm: "exponential_capped_v1", BaseDelayMS: 1, MaxDelayMS: 1, JitterAlgorithm: "none_v1"}})
		m.Deadlines.RoleSeconds = append(m.Deadlines.RoleSeconds, RoleDeadline{Role: role, Seconds: 20})
	}
	cfg := json.RawMessage(`{"compaction":"disabled","context_window_tokens":null,"max_output_tokens":4096,"reasoning_effort":null,"request_headers":[],"request_timeout_seconds":10,"sdk_max_retries":0,"streaming":false,"system_prompt_sha256":null,"temperature":"0","tools":[],"top_p":null}`)
	cfgCanonical, err := canonicalRaw(cfg, noDecimals)
	if err != nil {
		return Manifest{}, EnvironmentInputs{}, TechnicalArtifacts{}, err
	}
	for _, task := range taskOrder {
		role := RoleWorker
		if task == "C" {
			role = RoleReviewer
		}
		m.Tasks = append(m.Tasks, Task{TaskID: task, TaskManifestSHA256: blob("technical task " + task), FixtureSubtreeSHA256: zero, OracleRef: blob("technical oracle " + task), TreatmentTargetRoles: []Role{role}})
		m.Workflow.TaskRoles = append(m.Workflow.TaskRoles, TaskRoleFlow{TaskID: task, RoleFlow: []RoleFlow{{Role: role, FlowPosition: 1, ReachableCallClasses: []CallClass{CallInitial, CallRetry}}}})
		m.Q4Oracle.TaskOracles = append(m.Q4Oracle.TaskOracles, TaskOracle{TaskID: task, HiddenTestManifestSHA256: blob("technical hidden tests " + task)})
		for _, r := range m.ClosedRoleSet {
			tokens, calls, ccap, fcap := int64(0), int64(0), int64(0), int64(0)
			if r == role {
				tokens, calls, ccap, fcap = 1000, 10, 10, 10
			}
			m.TokenCaps = append(m.TokenCaps, RoleCap{TaskID: task, Role: r, Cap: tokens})
			m.CallCaps = append(m.CallCaps, RoleCap{TaskID: task, Role: r, Cap: calls})
			m.M3Caps = append(m.M3Caps, M3Cap{TaskID: task, Role: r, CCap: ccap, FCap: fcap})
		}
		for _, class := range []CallClass{CallInitial, CallRetry} {
			off := TreatmentArm{AttachmentPresent: &absent}
			assisted := TreatmentArm{AttachmentPresent: &present, AttachmentSHA256: sha256Hex(art.Attachment), AttachmentArtifactRef: art.AttachmentRef, AttachmentVersion: "fixture-v1", ProvenanceSchemaVersion: "v1", ConstructionVersion: "v1", RenderVersion: "v1", IndexedCommit: commit, SourceManifestSHA256: zero, FreshnessInputsSHA256: zero, Origin: "PROJECT_MEMORY"}
			m.TreatmentMapping = append(m.TreatmentMapping, TreatmentCell{TaskID: task, Role: role, CallClass: class, OFF: off, ASSISTED: assisted})
			m.InvocationConfigs = append(m.InvocationConfigs, InvocationConfig{TaskID: task, Role: role, CallClass: class, ModelID: "fixture", ModelVersion: "1", EffectiveConfigSchema: EffectiveConfigSchemaV1, EffectiveConfigSHA256: sha256Hex(cfgCanonical), EffectiveConfig: cfg})
		}
	}
	m.Workflow.WorkerFlowVersion = "v1"
	m.Workflow.ReviewerFlowVersion = "v1"
	m.ContextSourceInventory = []ContextSource{{SourceID: "project_memory", State: ProjectMemoryInventoryState, VerificationVersion: "v1"}, {SourceID: "router", State: "OFF", VerificationVersion: "v1"}, {SourceID: "mcp", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "apps_connectors", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "web", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "global_memory", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "AO_external_evidence", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "provider_tools", State: "DISABLED", VerificationVersion: "v1"}, {SourceID: "other", State: "DISABLED", VerificationVersion: "v1", InventorySHA256: zero}}
	m.Q6Oracle.MandatoryDefects = []MandatoryDefect{{DefectID: "d1", TargetSHA256: reviewTarget, File: "fixture.go", FileSHA256: fileSHA, CausalLine: 3, DefectClass: "AUTHORIZATION_BYPASS", CauseCode: "MISSING_GUARD", ImpactCode: "UNAUTHORIZED_ACCESS"}}
	m.Randomization = Randomization{PRNGAlgorithm: "ChaCha20-IETF", PRNGVersion: "RFC8439-v1", SeedHex: seedHex, RootSeedGeneration: "OS_CSPRNG_32_bytes_before_manifest_v1", TaskStreamDerivation: "sha256-task-domain-v1", ScheduleAlgorithm: "task-paired-off-assisted-v1", ScheduleVersion: 1, OrientationBalance: "2_3_each_task"}
	schedule, err := GenerateSchedule(m.Randomization)
	if err != nil {
		return Manifest{}, EnvironmentInputs{}, TechnicalArtifacts{}, err
	}
	m.Randomization.Schedule = schedule
	return m, env, art, ValidateManifest(m)
}

// TechnicalRepresentation builds the representation a well-behaved executor
// sends for the fixture: the frozen attachment only where the cell has one.
func TechnicalRepresentation(m Manifest, attachment []byte, task string, arm Arm, role Role, class CallClass) Representation {
	r := Representation{Payload: json.RawMessage(`{"technical_fixture":true}`), ContextSourceStates: m.ContextSourceInventory}
	cell, _ := treatmentCell(m, task, role, class)
	want := cell.OFF
	if arm == ArmAssisted {
		want = cell.ASSISTED
	}
	if want.AttachmentPresent != nil && *want.AttachmentPresent {
		r.ProjectMemoryAttachment = &Attachment{BytesBase64: encodeBase64(attachment), SHA256: want.AttachmentSHA256, Version: want.AttachmentVersion, Origin: "PROJECT_MEMORY"}
	}
	return r
}

// TechnicalPreflight returns the initial-cell representations of both arms.
func TechnicalPreflight(m Manifest, attachment []byte) []CellRepresentation {
	var out []CellRepresentation
	for _, cell := range m.TreatmentMapping {
		if cell.CallClass != CallInitial {
			continue
		}
		for _, arm := range []Arm{ArmOff, ArmAssisted} {
			out = append(out, CellRepresentation{TaskID: cell.TaskID, Role: cell.Role, CallClass: cell.CallClass, Arm: arm, Representation: TechnicalRepresentation(m, attachment, cell.TaskID, arm, cell.Role, cell.CallClass)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}
