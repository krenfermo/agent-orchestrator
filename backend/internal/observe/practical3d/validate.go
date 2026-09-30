package practical3d

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// ValidateManifest is validate_manifest() of 06 §3.7: true only for a
// complete, closed, cross-consistent manifest whose schedule equals the
// regeneration from its seed.
func ValidateManifest(m Manifest) error {
	if err := validateManifestExceptSchedule(m); err != nil {
		return err
	}
	return ValidateSchedule(m.Randomization)
}

// ValidateSchedule regenerates the 40 entries and requires exact structural
// equality (06 §3.5). Any mismatch is SCHEDULE_INVALID.
func ValidateSchedule(r Randomization) error {
	generated, err := GenerateSchedule(r)
	if err != nil {
		return err
	}
	if len(r.Schedule) != ExpectedPositions || !reflect.DeepEqual(r.Schedule, generated) {
		return fmt.Errorf("%w: %s: schedule differs from deterministic regeneration", ErrInvalidManifest, ReasonScheduleInvalid)
	}
	return nil
}

func validateManifestExceptSchedule(m Manifest) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return fail("schema_version=%q", m.SchemaVersion)
	}
	if m.ManifestSchemaSHA256 != ExpectedManifestSchemaSHA256 {
		return fail("manifest_schema_sha256 does not match frozen §§3.1–3.7")
	}
	if m.Estimand != "project_memory_assisted_vs_off_v1" {
		return fail("invalid estimand")
	}
	if !validGitCommit(m.AOCommit) || !validGitCommit(m.FixtureCommit) {
		return fail("AO and fixture commits must be full lowercase git commits")
	}
	if !reflect.DeepEqual(m.Arms, []Arm{ArmOff, ArmAssisted}) {
		return fail("arms must be [OFF,ASSISTED]")
	}
	if m.ProviderAccessBoundary != "AO_OBSERVED_CLIENT_ONLY_V1" {
		return fail("provider access boundary is not closed")
	}
	if m.DecisionRuleVersion != DecisionRuleVersion || m.N != ExpectedN || m.Router != "OFF" {
		return fail("decision/N/Router constants differ")
	}
	if err := validateRoles(m.ClosedRoleSet); err != nil {
		return err
	}
	if err := validateTasks(m.Tasks, m.ClosedRoleSet); err != nil {
		return err
	}
	reachable, err := validateWorkflow(m.Workflow, m.ClosedRoleSet)
	if err != nil {
		return err
	}
	if err := validateTreatment(m.TreatmentMapping, reachable); err != nil {
		return err
	}
	if err := validateInvocationConfigs(m.InvocationConfigs, reachable); err != nil {
		return err
	}
	for _, t := range m.Tasks {
		for _, role := range t.TreatmentTargetRoles {
			found := false
			for _, cell := range m.TreatmentMapping {
				if cell.TaskID == t.TaskID && cell.Role == role && cell.ASSISTED.AttachmentPresent != nil && *cell.ASSISTED.AttachmentPresent {
					found = true
				}
			}
			if !found {
				return fail("task %s target role %s has no ASSISTED cell with attachment present", t.TaskID, role)
			}
		}
	}
	used := usedRoles(m.Workflow)
	if err := validateCaps("TOKEN_CAP_ROLE", m.TokenCaps, m.ClosedRoleSet, used); err != nil {
		return err
	}
	if err := validateCaps("CALL_CAP_ROLE", m.CallCaps, m.ClosedRoleSet, used); err != nil {
		return err
	}
	if err := validateRetries(m.RetryPolicy, m.ClosedRoleSet); err != nil {
		return err
	}
	for key := range reachable {
		if key.class == CallRetry {
			continue
		}
		budget := retryBudget(m, key.role)
		if (budget.RetryableMaxRetries > 0 || budget.RateLimitedMaxRetries > 0) && !reachable[cellKey{key.task, key.role, CallRetry}] {
			return fail("retry budget makes missing retry cell reachable for %s/%s", key.task, key.role)
		}
	}
	if err := validateEnvironment(m.ExecutionEnvironment, m.AOCommit); err != nil {
		return err
	}
	if err := validateDeadlines(m.Deadlines, m.ClosedRoleSet); err != nil {
		return err
	}
	if err := validateInstrument(m.Instrument); err != nil {
		return err
	}
	if err := validateM3Caps(m.M3Caps, m.ClosedRoleSet, used); err != nil {
		return err
	}
	if m.Thresholds != (Thresholds{M1UMaxRatio: .85, M2OrM3MaxRatio: .80, DNeutralLower: .90, DNeutralUpper: 1.10, QualityRule: "pass_count_non_decrease_per_task"}) {
		return fail("thresholds differ from frozen constants")
	}
	if _, err := GenerateSchedule(m.Randomization); err != nil {
		return err
	}
	if err := validateContext(m.ExternalContext, m.ContextSourceInventory); err != nil {
		return err
	}
	if err := validateOracles(m.Q1Oracle, m.Q4Oracle, m.Q6Oracle); err != nil {
		return err
	}
	if !m.PositionIsolation.NewConversation || !m.PositionIsolation.NewSessionID || !m.PositionIsolation.CleanWorkingCopy || !m.PositionIsolation.NewAODataDir || !m.PositionIsolation.NewRuntimeProviderHomeWhenApplicable || !m.PositionIsolation.NoPriorTranscriptMemoryOrResults || m.PositionIsolation.LocalProcessTeardown != "verified_before_next_position" {
		return fail("position isolation constants differ")
	}
	if strings.TrimSpace(m.Provider.ProviderID) == "" || strings.TrimSpace(m.Provider.ClientID) == "" || strings.TrimSpace(m.Provider.ClientVersion) == "" || strings.TrimSpace(m.Provider.ProviderAPIVersion) == "" || !validSHA256(m.Provider.AccountRefSHA256) {
		return fail("provider identity/config incomplete")
	}
	return nil
}

var taskOrder = []string{"A", "B", "C", "D"}

// MaxCap bounds every TOKEN/CALL cap so no sum of caps or of in-cap attempt
// accounting can overflow int64.
const MaxCap = 1 << 40

// ProjectMemoryInventoryState records in the context inventory that Project
// Memory is governed only by the treatment mapping (06 §3.6).
const ProjectMemoryInventoryState = "TREATMENT_MAPPING"

func validGitCommit(s string) bool { return len(s) == 40 && strings.ToLower(s) == s && isHex(s) }
func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validateRoles(roles []Role) error {
	if len(roles) == 0 {
		return fmt.Errorf("%w: CLOSED_ROLE_SET empty", ErrInvalidManifest)
	}
	allowed := map[Role]int{}
	for i, r := range roleOrder {
		allowed[r] = i
	}
	last := -1
	for _, r := range roles {
		idx, ok := allowed[r]
		if !ok || idx <= last {
			return fmt.Errorf("%w: CLOSED_ROLE_SET unknown, duplicate, or out of order role %q", ErrInvalidManifest, r)
		}
		last = idx
	}
	return nil
}

func validateTasks(tasks []Task, roles []Role) error {
	if len(tasks) != 4 {
		return fmt.Errorf("%w: tasks must contain A-D", ErrInvalidManifest)
	}
	roleSet := roleSet(roles)
	for i, t := range tasks {
		if t.TaskID != string(rune('A'+i)) || !validSHA256(t.TaskManifestSHA256) || !validSHA256(t.FixtureSubtreeSHA256) || !validSHA256(t.OracleRef) {
			return fmt.Errorf("%w: invalid task row %d", ErrInvalidManifest, i)
		}
		seen := map[Role]bool{}
		if len(t.TreatmentTargetRoles) == 0 {
			return fmt.Errorf("%w: task %s has no treatment target role", ErrInvalidManifest, t.TaskID)
		}
		for _, r := range t.TreatmentTargetRoles {
			if !roleSet[r] || seen[r] {
				return fmt.Errorf("%w: invalid treatment target role", ErrInvalidManifest)
			}
			seen[r] = true
		}
	}
	return nil
}

type cellKey struct {
	task  string
	role  Role
	class CallClass
}

func validateWorkflow(w Workflow, roles []Role) (map[cellKey]bool, error) {
	if w.WorkerFlowVersion == "" || w.ReviewerFlowVersion == "" || len(w.TaskRoles) != 4 {
		return nil, fmt.Errorf("%w: incomplete workflow", ErrInvalidManifest)
	}
	allowedRoles := roleSet(roles)
	reachable := map[cellKey]bool{}
	for i, t := range w.TaskRoles {
		if t.TaskID != string(rune('A'+i)) || len(t.RoleFlow) == 0 {
			return nil, fmt.Errorf("%w: invalid workflow task order", ErrInvalidManifest)
		}
		seenRole := map[Role]bool{}
		for j, rf := range t.RoleFlow {
			if rf.FlowPosition != j+1 || !allowedRoles[rf.Role] || seenRole[rf.Role] || len(rf.ReachableCallClasses) == 0 {
				return nil, fmt.Errorf("%w: invalid role flow for task %s", ErrInvalidManifest, t.TaskID)
			}
			seenRole[rf.Role] = true
			seenClass := map[CallClass]bool{}
			for _, c := range rf.ReachableCallClasses {
				if !validCallClass(c) || seenClass[c] {
					return nil, fmt.Errorf("%w: invalid call class", ErrInvalidManifest)
				}
				seenClass[c] = true
				reachable[cellKey{t.TaskID, rf.Role, c}] = true
			}
		}
	}
	return reachable, nil
}

func validCallClass(c CallClass) bool {
	switch c {
	case CallInitial, CallContinuation, CallToolResult, CallReview, CallRepair, CallSummary, CallHelper, CallRetry:
		return true
	}
	return false
}

func validateTreatment(rows []TreatmentCell, reachable map[cellKey]bool) error {
	seen := map[cellKey]bool{}
	for _, row := range rows {
		k := cellKey{row.TaskID, row.Role, row.CallClass}
		if !reachable[k] || seen[k] {
			return fmt.Errorf("%w: missing/extra/duplicate treatment cell", ErrInvalidManifest)
		}
		seen[k] = true
		if err := validateTreatmentArm(row.OFF, false); err != nil {
			return err
		}
		if err := validateTreatmentArm(row.ASSISTED, true); err != nil {
			return err
		}
	}
	if len(seen) != len(reachable) {
		return fmt.Errorf("%w: treatment mapping incomplete", ErrInvalidManifest)
	}
	return nil
}
func validateTreatmentArm(a TreatmentArm, assisted bool) error {
	if a.AttachmentPresent == nil {
		return fmt.Errorf("%w: attachment_present required", ErrInvalidManifest)
	}
	fieldsEmpty := a.AttachmentSHA256 == "" && a.AttachmentArtifactRef == "" && a.AttachmentVersion == "" && a.ProvenanceSchemaVersion == "" && a.ConstructionVersion == "" && a.RenderVersion == "" && a.IndexedCommit == "" && a.SourceManifestSHA256 == "" && a.FreshnessInputsSHA256 == "" && a.Origin == ""
	if !assisted {
		if *a.AttachmentPresent || !fieldsEmpty {
			return fmt.Errorf("%w: OFF contains attachment fields", ErrInvalidManifest)
		}
		return nil
	}
	if !*a.AttachmentPresent {
		if !fieldsEmpty {
			return fmt.Errorf("%w: absent ASSISTED attachment has extra fields", ErrInvalidManifest)
		}
		return nil
	}
	if !validSHA256(a.AttachmentSHA256) || a.AttachmentArtifactRef == "" || a.AttachmentVersion == "" || a.ProvenanceSchemaVersion == "" || a.ConstructionVersion == "" || a.RenderVersion == "" || !validGitCommit(a.IndexedCommit) || !validSHA256(a.SourceManifestSHA256) || !validSHA256(a.FreshnessInputsSHA256) || a.Origin != "PROJECT_MEMORY" {
		return fmt.Errorf("%w: incomplete ASSISTED attachment", ErrInvalidManifest)
	}
	return nil
}

func validateInvocationConfigs(rows []InvocationConfig, reachable map[cellKey]bool) error {
	seen := map[cellKey]bool{}
	for _, r := range rows {
		k := cellKey{r.TaskID, r.Role, r.CallClass}
		configText := strings.TrimSpace(string(r.EffectiveConfig))
		if !reachable[k] || seen[k] || r.ModelID == "" || r.ModelVersion == "" || r.EffectiveConfigSchema == "" || !validSHA256(r.EffectiveConfigSHA256) || configText == "" || configText[0] != '{' {
			return fmt.Errorf("%w: invalid invocation config mapping", ErrInvalidManifest)
		}
		canonical, err := canonicalRaw(r.EffectiveConfig, noDecimals)
		if err != nil || sha256Hex(canonical) != r.EffectiveConfigSHA256 {
			return fmt.Errorf("%w: effective config digest mismatch", ErrInvalidManifest)
		}
		if err := validateEffectiveConfig(r.EffectiveConfigSchema, r.EffectiveConfig); err != nil {
			return fmt.Errorf("%w: %s/%s/%s: %w", ErrInvalidManifest, r.TaskID, r.Role, r.CallClass, err)
		}
		seen[k] = true
	}
	if len(seen) != len(reachable) {
		return fmt.Errorf("%w: invocation config mapping incomplete", ErrInvalidManifest)
	}
	return nil
}

// usedRoles returns, per task, the roles present in that task's role_flow.
func usedRoles(w Workflow) map[string]map[Role]bool {
	out := map[string]map[Role]bool{}
	for _, t := range w.TaskRoles {
		out[t.TaskID] = map[Role]bool{}
		for _, rf := range t.RoleFlow {
			out[t.TaskID][rf.Role] = true
		}
	}
	return out
}

// validateCaps requires the complete table in task A-D × CLOSED_ROLE_SET
// order, a positive cap for every role the workflow uses in that task and
// cap=0 for every unused role (06 §3.1, §5).
func validateCaps(name string, rows []RoleCap, roles []Role, used map[string]map[Role]bool) error {
	if len(rows) != 4*len(roles) {
		return fmt.Errorf("%w: %s cardinality", ErrInvalidManifest, name)
	}
	i := 0
	for _, task := range taskOrder {
		for _, role := range roles {
			r := rows[i]
			i++
			if r.TaskID != task || r.Role != role {
				return fmt.Errorf("%w: %s must be ordered task A-D × CLOSED_ROLE_SET", ErrInvalidManifest, name)
			}
			if r.Cap > MaxCap {
				return fmt.Errorf("%w: %s %s/%s cap exceeds %d", ErrInvalidManifest, name, task, role, int64(MaxCap))
			}
			if used[task][role] && r.Cap <= 0 {
				return fmt.Errorf("%w: %s %s/%s used role needs a positive cap", ErrInvalidManifest, name, task, role)
			}
			if !used[task][role] && r.Cap != 0 {
				return fmt.Errorf("%w: %s %s/%s unused role must have cap=0", ErrInvalidManifest, name, task, role)
			}
		}
	}
	return nil
}

func validateRetries(p RetryPolicy, roles []Role) error {
	if p.AlgorithmVersion == "" || len(p.RetryBudgets) != len(roles) {
		return fmt.Errorf("%w: retry budget coverage", ErrInvalidManifest)
	}
	seen := map[Role]bool{}
	allowed := roleSet(roles)
	for _, b := range p.RetryBudgets {
		if !allowed[b.Role] || seen[b.Role] || b.RetryableMaxRetries < 0 || b.RateLimitedMaxRetries < 0 || b.BackoffPolicy.Algorithm != "exponential_capped_v1" || b.BackoffPolicy.BaseDelayMS <= 0 || b.BackoffPolicy.MaxDelayMS < b.BackoffPolicy.BaseDelayMS || b.BackoffPolicy.JitterAlgorithm != "none_v1" {
			return fmt.Errorf("%w: ambiguous/invalid retry budget for role %q", ErrInvalidManifest, b.Role)
		}
		seen[b.Role] = true
	}
	return nil
}

func validateEnvironment(e ExecutionEnvironment, aoCommit string) error {
	if e.DigestSchemaVersion == "" || !validSHA256(e.ExpectedExecutionEnvironmentDigest) || e.Inputs.AOCommit != aoCommit || !validSHA256(e.Inputs.AOBinarySHA256) || e.Inputs.OSPlatformArch.OS == "" || e.Inputs.OSPlatformArch.Platform == "" || e.Inputs.OSPlatformArch.Arch == "" {
		return fmt.Errorf("%w: execution environment incomplete", ErrInvalidManifest)
	}
	if e.Inputs.RuntimeVersions == nil || e.Inputs.ProviderClientCLIVersions == nil || e.Inputs.TaskToolVersions == nil || e.Inputs.EffectiveEnvironmentConfigAllowlist == nil || e.Inputs.RunnerInstrumentVersions == nil || e.Inputs.AdditionalLocalConfiguration == nil {
		return fmt.Errorf("%w: execution environment arrays are required", ErrInvalidManifest)
	}
	for _, list := range [][]VersionInput{e.Inputs.RuntimeVersions, e.Inputs.ProviderClientCLIVersions, e.Inputs.TaskToolVersions, e.Inputs.RunnerInstrumentVersions} {
		if err := validateVersionInputs(list); err != nil {
			return err
		}
	}
	for _, list := range [][]ConfigInput{e.Inputs.EffectiveEnvironmentConfigAllowlist, e.Inputs.AdditionalLocalConfiguration} {
		if err := validateConfigInputs(list); err != nil {
			return err
		}
	}
	payload := struct {
		DigestSchemaVersion string            `json:"digest_schema_version"`
		Inputs              EnvironmentInputs `json:"inputs"`
	}{e.DigestSchemaVersion, e.Inputs}
	b, err := CanonicalJSON(payload)
	if err != nil || sha256Hex(b) != e.ExpectedExecutionEnvironmentDigest {
		return fmt.Errorf("%w: execution environment digest mismatch", ErrInvalidManifest)
	}
	return nil
}
func validateVersionInputs(v []VersionInput) error {
	last := ""
	for i, x := range v {
		if x.Component == "" || x.Version == "" || !validSHA256(x.BinarySHA256) || (i > 0 && x.Component <= last) {
			return fmt.Errorf("%w: version inputs must be complete and sorted unique", ErrInvalidManifest)
		}
		last = x.Component
	}
	return nil
}
func validateConfigInputs(v []ConfigInput) error {
	last := ""
	for i, x := range v {
		if x.Name == "" || x.EffectiveValueOrSHA256 == "" || (i > 0 && x.Name <= last) {
			return fmt.Errorf("%w: config inputs must be complete and sorted unique", ErrInvalidManifest)
		}
		last = x.Name
	}
	return nil
}

func validateDeadlines(d Deadlines, roles []Role) error {
	if d.PositionSeconds <= 0 || d.ProviderAttemptSeconds <= 0 || len(d.RoleSeconds) != len(roles) {
		return fmt.Errorf("%w: deadlines incomplete", ErrInvalidManifest)
	}
	seen := map[Role]bool{}
	for _, r := range d.RoleSeconds {
		if !roleSet(roles)[r.Role] || seen[r.Role] || r.Seconds <= 0 {
			return fmt.Errorf("%w: invalid role deadline", ErrInvalidManifest)
		}
		seen[r.Role] = true
	}
	return nil
}
func validateInstrument(i Instrument) error {
	if i.SchemaVersion == "" || i.AttemptEventSchemaVersion == "" || i.ExecutionEnvironmentDigestVersion == "" || i.ProviderRequestSchemaVersion == "" || i.ExplorationParserVersion == "" || i.VerifyVersion == "" || i.Q4RunnerVersion == "" || i.Q6ScorerVersion == "" {
		return fmt.Errorf("%w: instrument versions incomplete", ErrInvalidManifest)
	}
	return nil
}
func validateM3Caps(rows []M3Cap, roles []Role, used map[string]map[Role]bool) error {
	if len(rows) != 4*len(roles) {
		return fmt.Errorf("%w: M3 caps incomplete", ErrInvalidManifest)
	}
	positive := map[string]int{}
	for i, r := range rows {
		if r.TaskID != taskOrder[i/len(roles)] || r.Role != roles[i%len(roles)] || r.CCap < 0 || r.FCap < 0 || (r.CCap == 0) != (r.FCap == 0) {
			return fmt.Errorf("%w: invalid or unordered M3 cap", ErrInvalidManifest)
		}
		if r.CCap > 0 && !used[r.TaskID][r.Role] {
			return fmt.Errorf("%w: M3 measured role %s/%s is not in the workflow", ErrInvalidManifest, r.TaskID, r.Role)
		}
		if r.CCap > 0 {
			positive[r.TaskID]++
			want := RoleWorker
			if r.TaskID == "C" {
				want = RoleReviewer
			}
			if r.Role != want {
				return fmt.Errorf("%w: wrong measured M3 role", ErrInvalidManifest)
			}
		}
	}
	for _, t := range []string{"A", "B", "C", "D"} {
		if positive[t] != 1 {
			return fmt.Errorf("%w: task %s must have one M3 role", ErrInvalidManifest, t)
		}
	}
	return nil
}

func validateContext(ext ExternalContext, inv []ContextSource) error {
	required := []string{"project_memory", "router", "mcp", "apps_connectors", "web", "global_memory", "AO_external_evidence", "provider_tools", "other"}
	if ext.EqualizedSources == nil || len(inv) != len(required) {
		return fmt.Errorf("%w: context inventory incomplete", ErrInvalidManifest)
	}
	equal := map[string]string{}
	for _, e := range ext.EqualizedSources {
		if e.SourceID == "" || !validSHA256(e.RepresentationSHA256) || equal[e.SourceID] != "" {
			return fmt.Errorf("%w: invalid equalized source", ErrInvalidManifest)
		}
		equal[e.SourceID] = e.RepresentationSHA256
	}
	if ext.Policy == "DISABLED" && len(equal) != 0 {
		return fmt.Errorf("%w: disabled external context has equalized sources", ErrInvalidManifest)
	}
	if ext.Policy != "DISABLED" && ext.Policy != "EQUALIZED" {
		return fmt.Errorf("%w: invalid external context policy", ErrInvalidManifest)
	}
	if ext.Policy == "EQUALIZED" && len(equal) == 0 {
		return fmt.Errorf("%w: EQUALIZED external context needs a non-empty source list", ErrInvalidManifest)
	}
	for i := 1; i < len(ext.EqualizedSources); i++ {
		if ext.EqualizedSources[i].SourceID <= ext.EqualizedSources[i-1].SourceID {
			return fmt.Errorf("%w: equalized_sources must be ordered by source_id", ErrInvalidManifest)
		}
	}
	equalizedInInventory := 0
	for i, s := range inv {
		if s.SourceID != required[i] || s.VerificationVersion == "" {
			return fmt.Errorf("%w: context inventory order/content", ErrInvalidManifest)
		}
		switch s.SourceID {
		case "router":
			if s.State != "OFF" {
				return fmt.Errorf("%w: router must be OFF", ErrInvalidManifest)
			}
		case "project_memory":
			if s.State != ProjectMemoryInventoryState {
				return fmt.Errorf("%w: project_memory inventory state must be %s", ErrInvalidManifest, ProjectMemoryInventoryState)
			}
		default:
			if s.State != "DISABLED" && s.State != "EQUALIZED" {
				return fmt.Errorf("%w: invalid source state", ErrInvalidManifest)
			}
		}
		if s.State == "EQUALIZED" {
			if s.RepresentationSHA256 == "" || equal[s.SourceID] != s.RepresentationSHA256 {
				return fmt.Errorf("%w: unequal context source", ErrInvalidManifest)
			}
			equalizedInInventory++
		} else if s.RepresentationSHA256 != "" {
			return fmt.Errorf("%w: disabled source has representation", ErrInvalidManifest)
		}
		if s.SourceID == "other" && !validSHA256(s.InventorySHA256) {
			return fmt.Errorf("%w: other inventory digest missing", ErrInvalidManifest)
		}
	}
	if equalizedInInventory != len(equal) {
		return fmt.Errorf("%w: equalized_sources names a source that is not EQUALIZED in the inventory", ErrInvalidManifest)
	}
	return nil
}

func validateOracles(q1 Q1Oracle, q4 Q4Oracle, q6 Q6Oracle) error {
	if q1.Version == "" || !validSHA256(q1.VerifyCommandSHA256) || len(q1.AcceptedExitCodes) == 0 {
		return fmt.Errorf("%w: Q1 oracle invalid", ErrInvalidManifest)
	}
	seenExit := map[int]bool{}
	for _, x := range q1.AcceptedExitCodes {
		if x < 0 || seenExit[x] {
			return fmt.Errorf("%w: Q1 exit codes invalid", ErrInvalidManifest)
		}
		seenExit[x] = true
	}
	if q4.Version == "" || !validSHA256(q4.RunnerImageOrBinarySHA256) || !validSHA256(q4.CommandSHA256) || q4.TimeoutSeconds <= 0 || len(q4.TaskOracles) != 4 {
		return fmt.Errorf("%w: Q4 oracle invalid", ErrInvalidManifest)
	}
	for i, x := range q4.TaskOracles {
		if x.TaskID != string(rune('A'+i)) || !validSHA256(x.HiddenTestManifestSHA256) {
			return fmt.Errorf("%w: Q4 task oracle invalid", ErrInvalidManifest)
		}
	}
	if q6.Version != Q6Version || !validSHA256(q6.ReviewTargetSHA256) || !validSHA256(q6.FileManifestSHA256) || q6.K != 3 || len(q6.MandatoryDefects) == 0 || len(q6.MandatoryDefects) > q6.K {
		return fmt.Errorf("%w: Q6 oracle invalid", ErrInvalidManifest)
	}
	seen := map[string]bool{}
	primary := 0
	for _, d := range q6.MandatoryDefects {
		if d.DefectID == "" || seen[d.DefectID] || d.TargetSHA256 != q6.ReviewTargetSHA256 || !validSHA256(d.FileSHA256) || d.CausalLine <= 0 || !safeRelative(d.File) || !validDefectClass(d.DefectClass) || !validCause(d.CauseCode) || !validImpact(d.ImpactCode) {
			return fmt.Errorf("%w: Q6 mandatory defect invalid", ErrInvalidManifest)
		}
		seen[d.DefectID] = true
		if d.DefectID == q6.PrimaryDefectID {
			primary++
		}
	}
	if primary != 1 {
		return fmt.Errorf("%w: primary_defect_id must reference exactly one defect", ErrInvalidManifest)
	}
	return nil
}
func safeRelative(p string) bool {
	return p != "" && !filepath.IsAbs(p) && !strings.Contains(p, "\\") && p == filepath.Clean(p) && p != ".." && !strings.HasPrefix(p, "../")
}
func validDefectClass(s string) bool {
	return contains([]string{"AUTHORIZATION_BYPASS", "INPUT_VALIDATION", "STATE_TRANSITION", "DATA_INTEGRITY", "CONCURRENCY", "ERROR_HANDLING", "RESOURCE_LIFECYCLE", "API_CONTRACT", "SECURITY_BOUNDARY"}, s)
}
func validCause(s string) bool {
	return contains([]string{"MISSING_GUARD", "WRONG_PREDICATE", "WRONG_TARGET", "STALE_STATE", "UNSAFE_DEFAULT", "MISSING_CLEANUP", "NON_ATOMIC_UPDATE", "ERROR_DROPPED"}, s)
}
func validImpact(s string) bool {
	return contains([]string{"UNAUTHORIZED_ACCESS", "INCORRECT_RESULT", "DATA_LOSS", "STATE_CORRUPTION", "RACE", "RESOURCE_LEAK", "CRASH", "CONTRACT_VIOLATION"}, s)
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func roleSet(roles []Role) map[Role]bool {
	m := map[Role]bool{}
	for _, r := range roles {
		m[r] = true
	}
	return m
}

func treatmentCell(m Manifest, task string, role Role, class CallClass) (TreatmentCell, bool) {
	for _, x := range m.TreatmentMapping {
		if x.TaskID == task && x.Role == role && x.CallClass == class {
			return x, true
		}
	}
	return TreatmentCell{}, false
}
func roleCap(rows []RoleCap, task string, role Role) int64 {
	for _, x := range rows {
		if x.TaskID == task && x.Role == role {
			return x.Cap
		}
	}
	return 0
}
func retryBudget(m Manifest, role Role) RetryBudget {
	for _, x := range m.RetryPolicy.RetryBudgets {
		if x.Role == role {
			return x
		}
	}
	return RetryBudget{}
}
func m3Cap(m Manifest, task string) (M3Cap, bool) {
	for _, x := range m.M3Caps {
		if x.TaskID == task && x.CCap > 0 {
			return x, true
		}
	}
	return M3Cap{}, false
}
