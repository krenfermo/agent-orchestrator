// Package practical3d implements the frozen 3D-PRACTICAL experiment runner
// (docs/frente3/3d-practical.md and 06-benchmark-plan.md §§3–7). It is
// deliberately independent of AO's daemon and production database: the only
// durable state it writes is a new append-only run directory under the
// Frente 3 scratch root.
package practical3d

import (
	"encoding/json"
	"time"
)

// Frozen protocol constants (06 §3.1, §6, §3.7).
const (
	ManifestSchemaVersion = "ao.3d-practical.manifest.v2"
	DecisionRuleVersion   = "ao.3d-practical.decision.v2"
	Q6Version             = "ao.q6.practical.v2"
	ExpectedPositions     = 40
	ExpectedN             = 5
)

// Arm is the Project Memory mode of a position.
type Arm string

// The two arms, in the frozen manifest order.
const (
	ArmOff      Arm = "OFF"
	ArmAssisted Arm = "ASSISTED"
)

// Role is one member of the closed role enum.
type Role string

// The closed role enum, in its fixed order.
const (
	RolePlanner    Role = "planner"
	RoleWorker     Role = "worker"
	RoleReviewer   Role = "reviewer"
	RoleRepair     Role = "repair"
	RoleSummarizer Role = "summarizer"
	RoleHelper     Role = "helper"
)

var roleOrder = []Role{RolePlanner, RoleWorker, RoleReviewer, RoleRepair, RoleSummarizer, RoleHelper}

// CallClass is one member of the closed call-class enum (06 §3.2).
type CallClass string

// The closed call-class enum.
const (
	CallInitial      CallClass = "initial"
	CallContinuation CallClass = "continuation"
	CallToolResult   CallClass = "tool_result"
	CallReview       CallClass = "review"
	CallRepair       CallClass = "repair"
	CallSummary      CallClass = "summary"
	CallHelper       CallClass = "helper"
	CallRetry        CallClass = "retry"
)

// TerminalState is the single per-position terminal vocabulary (06 §4).
type TerminalState string

// The terminal enum of 06 §4.
const (
	StateCompleted               TerminalState = "COMPLETED"
	StateFailedWorker            TerminalState = "FAILED_WORKER"
	StateFailedReviewer          TerminalState = "FAILED_REVIEWER"
	StateTimeout                 TerminalState = "TIMEOUT"
	StateProviderRetryExhausted  TerminalState = "PROVIDER_RETRY_EXHAUSTED"
	StateProviderRateLimited     TerminalState = "PROVIDER_RATE_LIMITED"
	StateProviderPolicyFailure   TerminalState = "PROVIDER_POLICY_FAILURE"
	StateProviderTerminalFailure TerminalState = "PROVIDER_TERMINAL_FAILURE"
	StateProviderSanction        TerminalState = "PROVIDER_SANCTION"
	StateMalformedResult         TerminalState = "MALFORMED_RESULT"
	StateBlocked                 TerminalState = "BLOCKED_BY_PRIOR_POSITION"
)

// RequestOutcome is a per-request outcome; it feeds the transition table and
// is never itself a position state.
type RequestOutcome string

// The request outcomes of 06 §4.
const (
	OutcomeSuccess         RequestOutcome = "SUCCESS"
	OutcomeRetryable       RequestOutcome = "RETRYABLE"
	OutcomeRateLimited     RequestOutcome = "RATE_LIMITED"
	OutcomePolicyFailure   RequestOutcome = "POLICY_FAILURE"
	OutcomeTerminalFailure RequestOutcome = "TERMINAL_PROVIDER_FAILURE"
	OutcomeSanction        RequestOutcome = "PROVIDER_SANCTION"
)

// Manifest is ao.3d-practical.manifest.v2 (06 §3.1). Every field is
// mandatory; decoding is closed (DecodeManifest).
type Manifest struct {
	SchemaVersion          string               `json:"schema_version"`
	ManifestSchemaSHA256   string               `json:"manifest_schema_sha256"`
	Estimand               string               `json:"estimand"`
	AOCommit               string               `json:"ao_commit"`
	FixtureCommit          string               `json:"fixture_commit"`
	Tasks                  []Task               `json:"tasks"`
	Arms                   []Arm                `json:"arms"`
	TreatmentMapping       []TreatmentCell      `json:"treatment_mapping"`
	Provider               Provider             `json:"provider"`
	ProviderAccessBoundary string               `json:"provider_access_boundary"`
	InvocationConfigs      []InvocationConfig   `json:"invocation_config_mapping"`
	ClosedRoleSet          []Role               `json:"CLOSED_ROLE_SET"`
	Workflow               Workflow             `json:"workflow"`
	TokenCaps              []RoleCap            `json:"TOKEN_CAP_ROLE"`
	CallCaps               []RoleCap            `json:"CALL_CAP_ROLE"`
	RetryPolicy            RetryPolicy          `json:"retry_policy"`
	ExecutionEnvironment   ExecutionEnvironment `json:"execution_environment"`
	Deadlines              Deadlines            `json:"deadlines"`
	Instrument             Instrument           `json:"instrument"`
	M3Caps                 []M3Cap              `json:"M3_caps"`
	Thresholds             Thresholds           `json:"thresholds"`
	DecisionRuleVersion    string               `json:"decision_rule_version"`
	N                      int                  `json:"N"`
	Randomization          Randomization        `json:"randomization"`
	Router                 string               `json:"Router"`
	ExternalContext        ExternalContext      `json:"external_context"`
	ContextSourceInventory []ContextSource      `json:"context_source_inventory"`
	Q1Oracle               Q1Oracle             `json:"Q1_oracle"`
	Q4Oracle               Q4Oracle             `json:"Q4_oracle"`
	Q6Oracle               Q6Oracle             `json:"Q6_oracle"`
	PositionIsolation      PositionIsolation    `json:"position_isolation"`
}

// Task is one frozen task row (A–D).
type Task struct {
	TaskID               string `json:"task_id"`
	TaskManifestSHA256   string `json:"task_manifest_sha256"`
	FixtureSubtreeSHA256 string `json:"fixture_subtree_sha256"`
	OracleRef            string `json:"oracle_ref"`
	TreatmentTargetRoles []Role `json:"treatment_target_roles"`
}

// TreatmentCell is one reachable task × role × call_class cell (06 §3.2).
type TreatmentCell struct {
	TaskID    string       `json:"task_id"`
	Role      Role         `json:"role"`
	CallClass CallClass    `json:"call_class"`
	OFF       TreatmentArm `json:"OFF"`
	ASSISTED  TreatmentArm `json:"ASSISTED"`
}

// TreatmentArm is {attachment_present:false} or a complete frozen attachment.
type TreatmentArm struct {
	AttachmentPresent       *bool  `json:"attachment_present"`
	AttachmentSHA256        string `json:"attachment_sha256,omitempty"`
	AttachmentArtifactRef   string `json:"attachment_artifact_ref,omitempty"`
	AttachmentVersion       string `json:"attachment_version,omitempty"`
	ProvenanceSchemaVersion string `json:"provenance_schema_version,omitempty"`
	ConstructionVersion     string `json:"construction_version,omitempty"`
	RenderVersion           string `json:"render_version,omitempty"`
	IndexedCommit           string `json:"indexed_commit,omitempty"`
	SourceManifestSHA256    string `json:"source_manifest_sha256,omitempty"`
	FreshnessInputsSHA256   string `json:"freshness_inputs_sha256,omitempty"`
	Origin                  string `json:"origin,omitempty"`
}

// Provider identifies the one existing account and client (06 §3.3).
type Provider struct {
	ProviderID         string `json:"provider_id"`
	AccountRefSHA256   string `json:"account_ref_sha256"`
	ClientID           string `json:"client_id"`
	ClientVersion      string `json:"client_version"`
	ProviderAPIVersion string `json:"provider_api_version"`
}

// InvocationConfig is the explicit model/config of one reachable cell.
type InvocationConfig struct {
	TaskID                string          `json:"task_id"`
	Role                  Role            `json:"role"`
	CallClass             CallClass       `json:"call_class"`
	ModelID               string          `json:"model_id"`
	ModelVersion          string          `json:"model_version"`
	EffectiveConfigSchema string          `json:"effective_config_schema"`
	EffectiveConfigSHA256 string          `json:"effective_config_sha256"`
	EffectiveConfig       json.RawMessage `json:"effective_config"`
}

// Workflow defines which task × role × call_class cells are reachable.
type Workflow struct {
	WorkerFlowVersion   string         `json:"worker_flow_version"`
	ReviewerFlowVersion string         `json:"reviewer_flow_version"`
	TaskRoles           []TaskRoleFlow `json:"task_roles"`
}

// TaskRoleFlow is the ordered role flow of one task.
type TaskRoleFlow struct {
	TaskID   string     `json:"task_id"`
	RoleFlow []RoleFlow `json:"role_flow"`
}

// RoleFlow is one role step with its reachable call classes.
type RoleFlow struct {
	Role                 Role        `json:"role"`
	FlowPosition         int         `json:"flow_position"`
	ReachableCallClasses []CallClass `json:"reachable_call_classes"`
}

// RoleCap is one TOKEN_CAP_ROLE or CALL_CAP_ROLE row.
type RoleCap struct {
	TaskID string `json:"task_id"`
	Role   Role   `json:"role"`
	Cap    int64  `json:"cap"`
}

// RetryPolicy is the closed per-role retry budget table (06 §3.4).
type RetryPolicy struct {
	AlgorithmVersion string        `json:"algorithm_version"`
	RetryBudgets     []RetryBudget `json:"retry_budgets"`
}

// RetryBudget is the single budget row of one role.
type RetryBudget struct {
	Role                  Role          `json:"role"`
	RetryableMaxRetries   int           `json:"retryable_max_retries"`
	RateLimitedMaxRetries int           `json:"rate_limited_max_retries"`
	BackoffPolicy         BackoffPolicy `json:"backoff_policy"`
}

// BackoffPolicy is exponential_capped_v1 without jitter.
type BackoffPolicy struct {
	Algorithm       string `json:"algorithm"`
	BaseDelayMS     int64  `json:"base_delay_ms"`
	MaxDelayMS      int64  `json:"max_delay_ms"`
	JitterAlgorithm string `json:"jitter_algorithm"`
}

// ExecutionEnvironment carries the single expected environment digest common
// to both arms and its reproducible inputs (06 §3.6).
type ExecutionEnvironment struct {
	DigestSchemaVersion                string            `json:"digest_schema_version"`
	ExpectedExecutionEnvironmentDigest string            `json:"expected_execution_environment_digest"`
	Inputs                             EnvironmentInputs `json:"inputs"`
}

// EnvironmentInputs are the allowlisted, secret-free environment inputs.
type EnvironmentInputs struct {
	OSPlatformArch                      OSPlatformArch `json:"os_platform_arch"`
	AOBinarySHA256                      string         `json:"ao_binary_sha256"`
	AOCommit                            string         `json:"ao_commit"`
	RuntimeVersions                     []VersionInput `json:"runtime_versions"`
	ProviderClientCLIVersions           []VersionInput `json:"provider_client_cli_versions"`
	TaskToolVersions                    []VersionInput `json:"task_tool_versions"`
	EffectiveEnvironmentConfigAllowlist []ConfigInput  `json:"effective_environment_config_allowlist"`
	RunnerInstrumentVersions            []VersionInput `json:"runner_instrument_versions"`
	AdditionalLocalConfiguration        []ConfigInput  `json:"additional_local_configuration"`
}

// OSPlatformArch is {os, platform, arch}.
type OSPlatformArch struct {
	OS       string `json:"os"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

// VersionInput is one {component, version, binary_sha256} row.
type VersionInput struct {
	Component    string `json:"component"`
	Version      string `json:"version"`
	BinarySHA256 string `json:"binary_sha256"`
}

// ConfigInput is one {name, effective_value_or_sha256} row.
type ConfigInput struct {
	Name                   string `json:"name"`
	EffectiveValueOrSHA256 string `json:"effective_value_or_sha256"`
}

// Deadlines are the frozen position, attempt and per-role deadlines.
type Deadlines struct {
	PositionSeconds        int64          `json:"position_seconds"`
	ProviderAttemptSeconds int64          `json:"provider_attempt_seconds"`
	RoleSeconds            []RoleDeadline `json:"role_seconds"`
}

// RoleDeadline is the deadline of one role.
type RoleDeadline struct {
	Role    Role  `json:"role"`
	Seconds int64 `json:"seconds"`
}

// Instrument pins every instrument/parser/oracle version.
type Instrument struct {
	SchemaVersion                     string `json:"schema_version"`
	AttemptEventSchemaVersion         string `json:"attempt_event_schema_version"`
	ExecutionEnvironmentDigestVersion string `json:"execution_environment_digest_version"`
	ProviderRequestSchemaVersion      string `json:"provider_request_schema_version"`
	ExplorationParserVersion          string `json:"exploration_parser_version"`
	VerifyVersion                     string `json:"verify_version"`
	Q4RunnerVersion                   string `json:"q4_runner_version"`
	Q6ScorerVersion                   string `json:"q6_scorer_version"`
}

// M3Cap is one M3_caps row; exactly one measured role per task is positive.
type M3Cap struct {
	TaskID string `json:"task_id"`
	Role   Role   `json:"role"`
	CCap   int64  `json:"C_cap"`
	FCap   int64  `json:"F_cap"`
}

// Thresholds are the const decision thresholds of 06 §6.
type Thresholds struct {
	M1UMaxRatio    float64 `json:"m1u_max_ratio"`
	M2OrM3MaxRatio float64 `json:"m2_or_m3_max_ratio"`
	DNeutralLower  float64 `json:"D_neutral_lower"`
	DNeutralUpper  float64 `json:"D_neutral_upper"`
	QualityRule    string  `json:"quality_rule"`
}

// Randomization pins PRNG, seed derivation and the full schedule (06 §3.5).
type Randomization struct {
	PRNGAlgorithm        string     `json:"prng_algorithm"`
	PRNGVersion          string     `json:"prng_version"`
	SeedHex              string     `json:"seed_hex"`
	RootSeedGeneration   string     `json:"root_seed_generation"`
	TaskStreamDerivation string     `json:"task_stream_derivation"`
	ScheduleAlgorithm    string     `json:"schedule_algorithm"`
	ScheduleVersion      int        `json:"schedule_version"`
	OrientationBalance   string     `json:"orientation_balance"`
	Schedule             []Position `json:"schedule"`
}

// Position is one of the 40 scheduled entries.
type Position struct {
	PositionIndex int    `json:"position_index"`
	TaskID        string `json:"task_id"`
	PairIndex     int    `json:"pair_index"`
	PairOrder     []Arm  `json:"pair_order"`
	Arm           Arm    `json:"arm"`
	SampleID      string `json:"sample_id"`
}

// ExternalContext is {policy, equalized_sources}.
type ExternalContext struct {
	Policy           string            `json:"policy"`
	EqualizedSources []EqualizedSource `json:"equalized_sources"`
}

// EqualizedSource is a non-Project-Memory source identical in both arms.
type EqualizedSource struct {
	SourceID             string `json:"source_id"`
	RepresentationSHA256 string `json:"representation_sha256"`
}

// ContextSource is one row of the closed context-source inventory.
type ContextSource struct {
	SourceID             string `json:"source_id"`
	State                string `json:"state"`
	VerificationVersion  string `json:"verification_version"`
	InventorySHA256      string `json:"inventory_sha256,omitempty"`
	RepresentationSHA256 string `json:"representation_sha256,omitempty"`
}

// Q1Oracle is the frozen verify oracle.
type Q1Oracle struct {
	Version             string `json:"version"`
	VerifyCommandSHA256 string `json:"verify_command_sha256"`
	AcceptedExitCodes   []int  `json:"accepted_exit_codes"`
}

// Q4Oracle is the frozen hidden-test oracle outside the agent's reach.
type Q4Oracle struct {
	Version                   string       `json:"version"`
	RunnerImageOrBinarySHA256 string       `json:"runner_image_or_binary_sha256"`
	CommandSHA256             string       `json:"command_sha256"`
	TimeoutSeconds            int64        `json:"timeout_seconds"`
	TaskOracles               []TaskOracle `json:"task_oracles"`
}

// TaskOracle binds a task to its hidden test manifest.
type TaskOracle struct {
	TaskID                   string `json:"task_id"`
	HiddenTestManifestSHA256 string `json:"hidden_test_manifest_sha256"`
}

// Q6Oracle is the frozen task C review oracle (3d-practical §5).
type Q6Oracle struct {
	Version            string            `json:"version"`
	ReviewTargetSHA256 string            `json:"review_target_sha256"`
	FileManifestSHA256 string            `json:"file_manifest_sha256"`
	PrimaryDefectID    string            `json:"primary_defect_id"`
	MandatoryDefects   []MandatoryDefect `json:"mandatory_defects"`
	K                  int               `json:"K"`
}

// MandatoryDefect is one seeded defect with its exact causal line and codes.
type MandatoryDefect struct {
	DefectID     string `json:"defect_id"`
	TargetSHA256 string `json:"target_sha256"`
	File         string `json:"file"`
	FileSHA256   string `json:"file_sha256"`
	CausalLine   int    `json:"causal_line"`
	DefectClass  string `json:"defect_class"`
	CauseCode    string `json:"cause_code"`
	ImpactCode   string `json:"impact_code"`
}

// PositionIsolation are the per-position isolation constants.
type PositionIsolation struct {
	NewConversation                      bool   `json:"new_conversation"`
	NewSessionID                         bool   `json:"new_session_id"`
	CleanWorkingCopy                     bool   `json:"clean_working_copy"`
	NewAODataDir                         bool   `json:"new_AO_DATA_DIR"`
	NewRuntimeProviderHomeWhenApplicable bool   `json:"new_runtime_provider_home_when_applicable"`
	NoPriorTranscriptMemoryOrResults     bool   `json:"no_prior_transcript_memory_or_results"`
	LocalProcessTeardown                 string `json:"local_process_teardown"`
}

// Representation is the final post-adapter object handed to the transport.
type Representation struct {
	Payload                 json.RawMessage `json:"payload"`
	ProjectMemoryAttachment *Attachment     `json:"project_memory_attachment"`
	ExternalContext         bool            `json:"externalContext"`
	ContextSourceStates     []ContextSource `json:"context_source_states"`
}

// Attachment is a Project Memory attachment inside a representation.
type Attachment struct {
	BytesBase64 string `json:"bytes_base64"`
	SHA256      string `json:"sha256"`
	Version     string `json:"version"`
	Origin      string `json:"origin"`
}

// ProviderRequest is what an executor asks the ObservedClient to send.
type ProviderRequest struct {
	Role           Role           `json:"role"`
	CallClass      CallClass      `json:"call_class"`
	Representation Representation `json:"representation"`
}

// ProviderResponse is one transport result; nil accounting means MISSING.
type ProviderResponse struct {
	Output              json.RawMessage `json:"output"`
	Outcome             RequestOutcome  `json:"request_outcome"`
	InputTokens         *int64          `json:"input_tokens"`
	CachedInputTokens   *int64          `json:"cached_input_tokens"`
	UncachedInputTokens *int64          `json:"uncached_input_tokens"`
	ProviderMetadata    json.RawMessage `json:"provider_metadata"`
	TerminalMetadata    json.RawMessage `json:"terminal_metadata"`
}

// Finding is one reviewer finding as reported by the task C reviewer. It
// carries no defect_id: correspondence to a frozen mandatory defect is decided
// only by exact equality of target, file, file digest, causal line and codes.
type Finding struct {
	Rank         int    `json:"rank"`
	TargetSHA256 string `json:"target_sha256"`
	File         string `json:"file"`
	FileSHA256   string `json:"file_sha256"`
	CausalLine   int    `json:"causal_line"`
	DefectClass  string `json:"defect_class"`
	CauseCode    string `json:"cause_code"`
	ImpactCode   string `json:"impact_code"`
}

// FindingFor renders a frozen mandatory defect as the finding that matches it.
func FindingFor(rank int, d MandatoryDefect) Finding {
	return Finding{Rank: rank, TargetSHA256: d.TargetSHA256, File: d.File, FileSHA256: d.FileSHA256, CausalLine: d.CausalLine, DefectClass: d.DefectClass, CauseCode: d.CauseCode, ImpactCode: d.ImpactCode}
}

// ExecutionResult is what an executor reports for one position. Exploration
// counts come from the frozen exploration parser.
type ExecutionResult struct {
	TerminalState     TerminalState `json:"terminal_state"`
	ExplorationCalls  int64         `json:"exploration_calls"`
	DistinctFilesRead int64         `json:"distinct_files_read"`
	MilestoneObserved bool          `json:"milestone_observed"`
	Findings          []Finding     `json:"findings"`
}

// OracleResult is produced by the trusted Q1/Q4 oracle outside the agent's
// reach. The echoed digests bind the result to the frozen oracle inputs.
type OracleResult struct {
	Q1ExitCode            int    `json:"Q1_exit_code"`
	Q1VerifyCommandSHA256 string `json:"Q1_verify_command_sha256"`
	Q4Passed              bool   `json:"Q4_passed"`
	Q4TaskOracleSHA256    string `json:"Q4_task_oracle_sha256"`
	Q4CommandSHA256       string `json:"Q4_command_sha256"`
}

// PositionResult is the POSITION_RESULT payload.
type PositionResult struct {
	TerminalState     TerminalState `json:"terminal_state"`
	ExplorationCalls  int64         `json:"exploration_calls"`
	DistinctFilesRead int64         `json:"distinct_files_read"`
	MilestoneObserved bool          `json:"milestone_observed"`
	Q1                *bool         `json:"Q1"`
	Q4                *bool         `json:"Q4"`
	Findings          []Finding     `json:"findings"`
}

// Metrics is normalize()'s (M1u, M2, M3, Q1, Q4, Q6) for one position.
type Metrics struct {
	M1U int64   `json:"M1u"`
	M2  int64   `json:"M2"`
	M3  float64 `json:"M3"`
	Q1  bool    `json:"Q1"`
	Q4  bool    `json:"Q4"`
	Q6  Q6Value `json:"Q6"`
}

// Q6Value is a boolean for task C and the literal "NA" for tasks A/B/D.
type Q6Value struct {
	Applicable bool
	Pass       bool
}

// MarshalJSON renders "NA" outside task C.
func (q Q6Value) MarshalJSON() ([]byte, error) {
	if !q.Applicable {
		return []byte(`"NA"`), nil
	}
	return json.Marshal(q.Pass)
}

// UnmarshalJSON accepts "NA" or a boolean.
func (q *Q6Value) UnmarshalJSON(b []byte) error {
	if string(b) == `"NA"` {
		*q = Q6Value{}
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*q = Q6Value{Applicable: true, Pass: v}
	return nil
}

// EventType names a ledger event.
type EventType string

// Ledger event types.
const (
	EventManifest          EventType = "MANIFEST_FROZEN"
	EventPreflight         EventType = "PREFLIGHT_PASSED"
	EventPrestartInvalid   EventType = "PRESTART_INVALID"
	EventEnvironment       EventType = "EXECUTION_ENVIRONMENT_OBSERVED"
	EventSampleStart       EventType = "SAMPLE_START"
	EventAttemptDispatched EventType = "ATTEMPT_DISPATCHED"
	EventAttemptFinalized  EventType = "ATTEMPT_FINALIZED"
	EventPositionResult    EventType = "POSITION_RESULT"
	EventPositionTerminal  EventType = "POSITION_TERMINAL"
	EventBatchStop         EventType = "BATCH_STOP"
	EventDecision          EventType = "DECISION"
)

// Event is one append-only ledger line. The physical ledger is never updated
// or rewritten; every field that a lifecycle rule repeats is repeated verbatim.
type Event struct {
	Type                         EventType       `json:"type"`
	PrevEventSHA256              string          `json:"prev_event_sha256,omitempty"`
	ExperimentID                 string          `json:"experiment_id"`
	Timestamp                    time.Time       `json:"timestamp"`
	ManifestSHA256               string          `json:"manifest_sha256,omitempty"`
	Manifest                     json.RawMessage `json:"manifest,omitempty"`
	SampleID                     string          `json:"sample_id,omitempty"`
	PositionIndex                int             `json:"position_index,omitempty"`
	TaskID                       string          `json:"task_id,omitempty"`
	Arm                          Arm             `json:"arm,omitempty"`
	Phase                        string          `json:"phase,omitempty"`
	ObservedDigest               string          `json:"observed_digest,omitempty"`
	AttemptID                    string          `json:"attempt_id,omitempty"`
	CallIndex                    int             `json:"call_index,omitempty"`
	Role                         Role            `json:"role,omitempty"`
	CallClass                    CallClass       `json:"call_class,omitempty"`
	ProviderID                   string          `json:"provider_id,omitempty"`
	ModelID                      string          `json:"model_id,omitempty"`
	ModelVersion                 string          `json:"model_version,omitempty"`
	EffectiveConfigSHA256        string          `json:"effective_config_sha256,omitempty"`
	RepresentationSHA256         string          `json:"final_post_adapter_representation_sha256,omitempty"`
	AttachmentPresent            *bool           `json:"project_memory_attachment_present,omitempty"`
	AttachmentSHA256             string          `json:"attachment_digest,omitempty"`
	AttachmentVersion            string          `json:"attachment_version,omitempty"`
	AttachmentOrigin             string          `json:"origin,omitempty"`
	ExternalContext              *bool           `json:"externalContext,omitempty"`
	ContextSourceInventorySHA256 string          `json:"context_source_inventory_sha256,omitempty"`
	ContextSourceStates          []ContextSource `json:"context_source_states,omitempty"`
	RetryChainID                 string          `json:"retry_chain_id,omitempty"`
	RetryIndex                   int             `json:"retry_index,omitempty"`
	RetryCause                   RequestOutcome  `json:"retry_cause,omitempty"`
	RequestOutcome               RequestOutcome  `json:"request_outcome,omitempty"`
	InputTokens                  *int64          `json:"input_tokens,omitempty"`
	CachedInputTokens            *int64          `json:"cached_input_tokens,omitempty"`
	UncachedInputTokens          *int64          `json:"uncached_input_tokens,omitempty"`
	MissingAccounting            []string        `json:"missing_accounting,omitempty"`
	TransportError               string          `json:"transport_error,omitempty"`
	ProviderMetadata             json.RawMessage `json:"provider_metadata,omitempty"`
	TerminalMetadata             json.RawMessage `json:"terminal_metadata,omitempty"`
	Result                       *PositionResult `json:"result,omitempty"`
	TerminalState                TerminalState   `json:"terminal_state,omitempty"`
	Decision                     string          `json:"decision,omitempty"`
	ReasonCode                   string          `json:"reason_code,omitempty"`
	Reason                       string          `json:"reason,omitempty"`
}

// Envelope is stored next to the ledger: {experiment_id, manifest_sha256,
// manifest, metadata}. Metadata never enters the canonical manifest bytes.
type Envelope struct {
	ExperimentID   string           `json:"experiment_id"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Manifest       json.RawMessage  `json:"manifest"`
	Metadata       EnvelopeMetadata `json:"metadata"`
}

// EnvelopeMetadata holds human labels; never part of identity or decision.
type EnvelopeMetadata struct {
	HumanLabel   *string `json:"human_label"`
	HumanVersion *string `json:"human_version"`
}

// Decision reason codes. PRESTART_INVALID, IDENTITY_MISMATCH and
// SCHEDULE_INVALID are the named reasons of 06 §6; the others label which
// conjunct of the GO condition failed.
const (
	ReasonPrestartInvalid  = "PRESTART_INVALID"
	ReasonIdentityMismatch = "IDENTITY_MISMATCH"
	ReasonScheduleInvalid  = "SCHEDULE_INVALID"
	ReasonLineageInvalid   = "LINEAGE_INVALID"
	ReasonNotAllCompleted  = "NOT_ALL_COMPLETED"
	ReasonQuality          = "QUALITY_DEGRADED"
	ReasonEfficiency       = "EFFICIENCY_NOT_MET"
	ReasonNegativeControl  = "NEGATIVE_CONTROL_NOT_NEUTRAL"
	ReasonAllConditions    = "ALL_CONDITIONS_MET"
)

// PositionReport is one normalized position in the report.
type PositionReport struct {
	Position    Position            `json:"position"`
	State       TerminalState       `json:"state"`
	Metrics     Metrics             `json:"metrics"`
	Diagnostics PositionDiagnostics `json:"diagnostics"`
	Errors      []string            `json:"errors,omitempty"`
}

// PositionDiagnostics are observed (never imputed) values from validly
// materialized attempts. They never change the decision.
type PositionDiagnostics struct {
	Attempts            int            `json:"attempts"`
	M1Total             int64          `json:"M1_total_input_tokens"`
	CachedInputTokens   int64          `json:"cached_input_tokens"`
	UncachedInputTokens int64          `json:"uncached_input_tokens"`
	ProviderErrors      map[string]int `json:"provider_errors"`
	RoleCalls           map[Role]int64 `json:"role_calls"`
	RoleUncachedTokens  map[Role]int64 `json:"role_uncached_tokens"`
	FirstDispatch       *time.Time     `json:"first_dispatch,omitempty"`
	LastFinalization    *time.Time     `json:"last_finalization,omitempty"`
	CacheByRequest      []RequestCache `json:"cache_by_request"`
}

// RequestCache is the cache split of one request.
type RequestCache struct {
	CallIndex           int       `json:"call_index"`
	Role                Role      `json:"role"`
	CallClass           CallClass `json:"call_class"`
	CachedInputTokens   int64     `json:"cached_input_tokens"`
	UncachedInputTokens int64     `json:"uncached_input_tokens"`
}

// ResidualConfounder publishes the provider cache distribution. Shared
// provider state is never claimed to be isolated; observed zero cache does not
// prove isolation (3d-practical §4).
type ResidualConfounder struct {
	Present           bool                 `json:"present"`
	Statement         string               `json:"statement"`
	ByTaskArm         []CacheAggregate     `json:"by_task_arm"`
	ByPositionInOrder []PositionCacheEntry `json:"by_position_in_temporal_order"`
}

// CacheAggregate sums cache per task/arm.
type CacheAggregate struct {
	TaskID              string `json:"task_id"`
	Arm                 Arm    `json:"arm"`
	CachedInputTokens   int64  `json:"cached_input_tokens"`
	UncachedInputTokens int64  `json:"uncached_input_tokens"`
}

// PositionCacheEntry is one position's cache split in temporal order.
type PositionCacheEntry struct {
	PositionIndex       int    `json:"position_index"`
	TaskID              string `json:"task_id"`
	Arm                 Arm    `json:"arm"`
	CachedInputTokens   int64  `json:"cached_input_tokens"`
	UncachedInputTokens int64  `json:"uncached_input_tokens"`
}

// TaskArmMedians are the normalized medians of one task/arm.
type TaskArmMedians struct {
	TaskID string  `json:"task_id"`
	Arm    Arm     `json:"arm"`
	M1U    float64 `json:"median_M1u"`
	M2     float64 `json:"median_M2"`
	M3     float64 `json:"median_M3"`
}

// Report is the decision plus the full diagnostic publication (06 §7).
type Report struct {
	SchemaVersion      string             `json:"schema_version"`
	ExperimentID       string             `json:"experiment_id"`
	Verdict            string             `json:"verdict"`
	ReasonCode         string             `json:"reason_code"`
	Reason             string             `json:"reason"`
	LineageValid       bool               `json:"lineage_valid"`
	AllCompleted       bool               `json:"all_completed"`
	LineageErrors      []string           `json:"lineage_errors,omitempty"`
	Medians            []TaskArmMedians   `json:"medians_normalized"`
	SignalAvailability map[string]string  `json:"signal_availability"`
	ResidualConfounder ResidualConfounder `json:"RESIDUAL_CONFOUNDER"`
	Positions          []PositionReport   `json:"positions"`
	GeneratedAt        time.Time          `json:"generated_at"`
}
