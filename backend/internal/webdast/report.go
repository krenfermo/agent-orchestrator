package webdast

// report.go — the Go shape of an ao.pentest/v1 report. It marshals to exactly
// the JSON that skillreport.PentestSchema validates. The checker emits it; the
// daemon validates, redacts, re-validates, persists and hashes it.

// ReportSchemaVersion is the report's schemaVersion. It must match
// skillreport.PentestSchemaVersion.
const ReportSchemaVersion = "ao.pentest/v1"

// Report is one active-pentest run's structured output.
type Report struct {
	SchemaVersion    string            `json:"schemaVersion"`
	ProjectID        string            `json:"projectId"`
	SkillID          string            `json:"skillId"`
	SkillVersion     string            `json:"skillVersion"`
	RunID            string            `json:"runId"`
	AuthorizationID  string            `json:"authorizationId"`
	Target           Target            `json:"target"`
	ScopePaths       []string          `json:"scopePaths,omitempty"`
	StartedAt        string            `json:"startedAt"`
	EndedAt          string            `json:"endedAt"`
	Tool             ToolInfo          `json:"tool"`
	Limits           Limits            `json:"limits"`
	Coverage         Coverage          `json:"coverage"`
	RequestsMade     int               `json:"requestsMade"`
	Findings         []Finding         `json:"findings"`
	BlockedAttempts  []BlockedAttempt  `json:"blockedAttempts"`
	BlockedRedirects []BlockedRedirect `json:"blockedRedirects"`
	RuntimeErrors    []string          `json:"runtimeErrors"`
	Limitations      []string          `json:"limitations"`
	Provenance       Provenance        `json:"provenance"`
}

// ToolInfo names the checker for the report.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
}

// Coverage records what was and was NOT attempted, so an empty findings list
// reads as "these checks found nothing" rather than "the target is secure".
type Coverage struct {
	Statement       string   `json:"statement"`
	EndpointsProbed int      `json:"endpointsProbed"`
	ChecksRun       []string `json:"checksRun"`
	NotAttempted    []string `json:"notAttempted"`
}

// Finding is one issue. reproduction.steps carry the exact request that
// demonstrated it, with credentials redacted by the daemon before storage.
type Finding struct {
	ID             string        `json:"id"`
	Severity       string        `json:"severity"`
	Confidence     string        `json:"confidence"`
	Check          string        `json:"check"`
	Title          string        `json:"title"`
	Endpoint       string        `json:"endpoint"`
	Method         string        `json:"method,omitempty"`
	Recommendation string        `json:"recommendation"`
	Reproduction   *Reproduction `json:"reproduction,omitempty"`
}

// Reproduction is the minimal evidence of a finding.
type Reproduction struct {
	Steps []string `json:"steps"`
}

// BlockedAttempt records a destination the egress boundary refused. Its
// presence in the report is the proof that the boundary is doing its job.
type BlockedAttempt struct {
	Destination string `json:"destination"`
	Reason      string `json:"reason"`
	Count       int    `json:"count,omitempty"`
}

// BlockedRedirect records a redirect the checker did NOT follow because it
// pointed off the authorized target.
type BlockedRedirect struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Provenance records who authorized the run and the controls it ran under.
type Provenance struct {
	RequestedBy      string   `json:"requestedBy"`
	AuthorizationRef string   `json:"authorizationRef"`
	RunnerControls   []string `json:"runnerControls,omitempty"`
}
