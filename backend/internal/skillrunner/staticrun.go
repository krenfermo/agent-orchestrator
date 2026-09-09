package skillrunner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
)

// staticrun.go — the one execution path AO ships.
//
// It runs ao.static-scan/v1 over a staged, scope-limited copy of a project, in
// a container with no network, and returns a report whose coverage says what
// was and was not read. Everything it could get wrong is a refusal:
//
//   - the runtime is unusable        -> refuse
//   - the tool is not approved       -> refuse
//   - the base image is absent       -> refuse (AO pulls nothing)
//   - staging produced no files      -> refuse
//   - the container saw fewer files
//     than AO staged                 -> refuse (the silent-empty-mount case)
//   - the boundary was not
//     demonstrated                   -> refuse
//
// The last two are why this exists rather than "just run grep in a container".
// A scan that cannot prove what it read must not produce a report, because the
// report a caller would then act on says "clean" about files nobody opened.

// StaticScanRequest is one static-code run.
type StaticScanRequest struct {
	// Scope is what authorizes this run: tenant, project, skill, version and
	// mode, every field, no wildcard. It is what the image approval is looked
	// up against, and it comes from the resolved activation -- never from a
	// caller who could widen it.
	Scope skillimage.Scope
	// ProjectID and ProjectPath identify the checkout to scan.
	ProjectID   string
	ProjectPath string
	// ScopePaths limits the scan, repo-relative. Empty means the checkout.
	ScopePaths []string
	// StagingRootOverride lets an operator place staging somewhere the
	// container runtime definitely shares, when the default does not work.
	StagingRootOverride string
	// DataDir is AO's own data directory, passed so the staging root can be
	// refused if it resolves inside it. A run must never be able to see AO's
	// database or the credentials in it, and the cheapest place to enforce
	// that is before anything is staged.
	DataDir string
	// Params are the tool's two validated integers.
	Params ToolParams
	Limits Limits
}

// ScanFinding is one match. It carries the rule, never the matched text: a
// secret rule that echoed its match would put the credential in the report it
// exists to keep out.
type ScanFinding struct {
	RuleID         string `json:"ruleId"`
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Title          string `json:"title"`
	Path           string `json:"path"`
	Line           int    `json:"line"`
	Recommendation string `json:"recommendation"`
	// Confidence is always "possible" for this tool. It matches a pattern; it
	// does not parse, and it cannot follow a value to a sink.
	Confidence string `json:"confidence"`
}

// SkippedFile records something the scan did NOT read, and why. Its presence
// is what stops a short report from reading as a clean one.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanCoverage is the honest half of the report.
type ScanCoverage struct {
	// FilesStaged is what AO put on the mount; FilesVisible is what the
	// container could see. They must match or the run is refused.
	FilesStaged  int `json:"filesStaged"`
	FilesVisible int `json:"filesVisible"`
	// FilesScanned is what the rules actually ran over.
	FilesScanned int           `json:"filesScanned"`
	Skipped      []SkippedFile `json:"skipped"`
	// RulesRun names every rule that executed, so an empty findings list can
	// be read as "these checks found nothing" rather than "nothing was checked".
	RulesRun []string `json:"rulesRun"`
	// Extensions is what the tool understands at all.
	Extensions []string `json:"extensions"`
	// Limitations is the tool telling the reader what it cannot know. It is
	// not boilerplate: without it a caller reads "0 findings" as "secure".
	Limitations []string `json:"limitations"`
}

// StaticScanReport is the structured result.
type StaticScanReport struct {
	SchemaVersion string `json:"schemaVersion"`
	ProjectID     string `json:"projectId"`
	Tool          string `json:"tool"`
	// ImageDigest is what the RUNTIME resolved, read back from it, not the
	// reference AO passed. The two are the same on a healthy host and the
	// difference is the whole point of checking.
	ImageDigest string `json:"imageDigest"`
	// ApprovalID and ApprovedBy name the decision that authorized this image.
	// A report that says which bytes ran without saying who allowed them
	// leaves the more important half of the question unanswered.
	ApprovalID string `json:"approvalId"`
	ApprovedBy string `json:"approvedBy"`
	// ApprovalRevokedDuringRun records that the approval stopped being active
	// while the container was running. AO does not kill a running container
	// (see RevocationPolicy), so this is how the fact reaches the reader
	// instead of disappearing.
	ApprovalRevokedDuringRun bool             `json:"approvalRevokedDuringRun"`
	StartedAt                time.Time        `json:"startedAt"`
	EndedAt                  time.Time        `json:"endedAt"`
	Coverage                 ScanCoverage     `json:"coverage"`
	Findings                 []ScanFinding    `json:"findings"`
	Evidence                 BoundaryEvidence `json:"evidence"`
	// Truncated says the tool's own output hit AO's cap, so the finding list
	// may be incomplete. A truncated report is not a clean one.
	Truncated bool `json:"truncated"`
}

// staticScanLimitations is what this tool cannot do, stated in the report
// rather than in documentation nobody reads next to the findings.
var staticScanLimitations = []string{
	"This is a pattern scanner, not a static analyzer: it matches text, it does not parse code " +
		"and it cannot follow a value from an entry point to a sink.",
	"An empty findings list means these rules matched nothing in the files listed as scanned. " +
		"It is not evidence that the project is free of vulnerabilities.",
	"Only the listed file extensions are examined. Anything else is reported as skipped.",
	"Every finding is unverified and may be a false positive; each needs a human to confirm " +
		"reachability before it is treated as real.",
	"No dependency, network, runtime or configuration-at-deploy checking is performed.",
}

// RunStaticScan executes the one enabled mode, against the image an
// administrator approved for this exact scope.
//
// The authority is a required argument rather than a field on the Runner, so
// there is no way to construct a runner that executes without one. A nil
// authority is an installation with no trust root, which authorizes nothing.
func (r *Runner) RunStaticScan(
	ctx context.Context, authority ImageAuthority, req StaticScanRequest,
) (StaticScanReport, error) {
	if !r.Available() {
		return StaticScanReport{}, fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
	}
	params := req.Params
	if params.MaxFiles == 0 && params.MaxFileBytes == 0 {
		params = DefaultToolParams()
	}
	if err := params.Validate(); err != nil {
		return StaticScanReport{}, err
	}
	limits := req.Limits
	if limits.Wall <= 0 {
		limits = DefaultLimits()
	}

	// The trust root, first: nothing is staged for a run that is not authorized
	// to happen. Staging copies somebody's source, and doing it before the
	// authorization check would leave a copy behind for a run that was always
	// going to be refused.
	image, err := r.ResolveApprovedImage(ctx, authority, req.Scope, ToolStaticScan)
	if err != nil {
		return StaticScanReport{}, err
	}
	contract := image.Contract

	root, err := StagingRootFor(req.ProjectPath, req.StagingRootOverride)
	if err != nil {
		return StaticScanReport{}, err
	}
	// Two checks on the staging root, both BEFORE a single file is copied.
	//
	// The order matters. Validate is cheap and local: it refuses a root that is
	// a symlink, world-writable, traversing, or inside AO's data dir. Verify
	// costs a container start, so it runs second, and it answers the question
	// no amount of local checking can: does the RUNTIME see this path at all?
	//
	// Doing both here means a host whose runtime cannot see the staging root
	// finds out before somebody's source is copied to disk, and finds out with
	// the mount named — not three layers later as an unexplained failure to
	// attest filesystem isolation.
	if err := ValidateStagingRoot(root, req.DataDir); err != nil {
		return StaticScanReport{}, err
	}
	if err := r.VerifyStagingVisible(ctx, root, image); err != nil {
		return StaticScanReport{}, err
	}
	runID := "run-" + randomToken()
	staging, err := Stage(StageRequest{
		SourceDir: req.ProjectPath, ScopePaths: req.ScopePaths, Root: root, RunID: runID,
		MaxFiles: params.MaxFiles, MaxFileBytes: int64(params.MaxFileBytes),
	})
	if err != nil {
		return StaticScanReport{}, err
	}
	// Staged inputs are removed whatever happens: they are a copy of somebody's
	// source sitting beside their projects, and leaving one behind on a failure
	// is the kind of thing that is discovered months later.
	defer func() { _ = staging.Cleanup() }()

	// The last check before anything starts. Staging took real time, and an
	// approval withdrawn during it must stop the launch rather than be noticed
	// afterwards.
	if err := r.RecheckBeforeLaunch(ctx, authority, image); err != nil {
		return StaticScanReport{}, err
	}

	res, err := r.Run(ctx, Request{
		Image:    image.Ref(),
		Argv:     contract.Argv(params),
		InputDir: staging.Dir,
		Limits:   limits,
	})
	if err != nil {
		return StaticScanReport{}, err
	}
	// The mount delivered what AO staged, or the run does not produce a report.
	//
	// The visibility probe proved the runtime can see the ROOT; this proves it
	// delivered THESE bytes. They are different failures: a share added between
	// the probe and the launch, a stale VM cache, a partial copy. A scan over a
	// tree that is not the project is not a scan of the project, and reporting
	// it as one is the single most misleading thing this runner could do.
	if err := verifyStagedInputsDelivered(staging, res.Evidence); err != nil {
		return StaticScanReport{}, err
	}
	if res.TimedOut {
		return StaticScanReport{}, fmt.Errorf("skillrunner: the scan exceeded its %s wall clock; "+
			"a partial scan is not a report", limits.Wall)
	}
	if res.ExitCode != 0 {
		return StaticScanReport{}, fmt.Errorf("skillrunner: the scan exited %d: %s",
			res.ExitCode, strings.TrimSpace(lastLines(res.Stderr, 3)))
	}

	// The boundary has to have held before anything the run said is worth
	// reading.
	if err := res.Evidence.Verify(r.runtime.Controls()); err != nil {
		return StaticScanReport{}, err
	}
	if err := staging.VerifyVisible(res.Evidence.InputFilesVisible); err != nil {
		return StaticScanReport{}, err
	}

	report := parseStaticScan(res, staging, contract, req.ProjectID)
	report.ImageDigest = image.Digest
	report.ApprovalID = image.Approval.ID
	report.ApprovedBy = image.Approval.ApprovedBy
	// Asked after the fact, deliberately. AO does not kill a running container
	// on revocation -- RevocationPolicy says why -- so the only honest thing
	// left is to say so in the report the reader will act on.
	report.ApprovalRevokedDuringRun = r.RevokedSince(ctx, authority, image, time.Now().UTC())
	return report, nil
}

// parseStaticScan turns the tool's line protocol into the report. Anything it
// cannot parse is dropped rather than guessed at; the counts come from the
// tool's own totals, so a dropped line shows up as a discrepancy rather than
// as a smaller, cleaner-looking report.
func parseStaticScan(res Result, staging Staging, contract ToolContract, projectID string) StaticScanReport {
	report := StaticScanReport{
		SchemaVersion: "ao.static-scan/v1",
		ProjectID:     projectID,
		Tool:          string(contract.Tool),
		StartedAt:     res.Started,
		EndedAt:       res.Ended,
		Evidence:      res.Evidence,
		Truncated:     res.Truncated,
		Findings:      []ScanFinding{},
		Coverage: ScanCoverage{
			FilesStaged:  len(staging.Inputs),
			FilesVisible: res.Evidence.InputFilesVisible,
			Extensions:   append([]string(nil), scannedExtensions...),
			Limitations:  append([]string(nil), staticScanLimitations...),
			// Staging's own skips are part of coverage: a file AO never put
			// on the mount was never scanned, and the report has to say so.
			Skipped:  append([]SkippedFile{}, staging.Skipped...),
			RulesRun: []string{},
		},
	}
	byRule := map[string]scanRule{}
	for _, rule := range staticScanRules {
		byRule[rule.ID] = rule
		report.Coverage.RulesRun = append(report.Coverage.RulesRun, rule.ID)
	}

	section := ""
	const sep = "\x1f"
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, "AO_SECTION="); ok {
			section = rest
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok && strings.HasPrefix(key, "ao_") {
			if key == "ao_scanned_files" {
				report.Coverage.FilesScanned, _ = strconv.Atoi(value)
			}
			continue
		}
		switch section {
		case "skipped":
			path, reason, ok := strings.Cut(line, sep)
			if !ok || strings.TrimSpace(path) == "" {
				continue
			}
			report.Coverage.Skipped = append(report.Coverage.Skipped, SkippedFile{
				Path: containerRelPath(path), Reason: reason,
			})
		case "findings":
			ruleID, hit, ok := strings.Cut(line, sep)
			if !ok {
				continue
			}
			rule, known := byRule[ruleID]
			if !known {
				continue
			}
			path, lineNo, ok := strings.Cut(hit, ":")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(lineNo))
			if err != nil {
				continue
			}
			report.Findings = append(report.Findings, ScanFinding{
				RuleID: rule.ID, Severity: rule.Severity, Category: rule.Category,
				Title: rule.Title, Path: containerRelPath(path), Line: n,
				Recommendation: rule.Recommendation, Confidence: "possible",
			})
		}
	}

	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Path != report.Findings[j].Path {
			return report.Findings[i].Path < report.Findings[j].Path
		}
		if report.Findings[i].Line != report.Findings[j].Line {
			return report.Findings[i].Line < report.Findings[j].Line
		}
		return report.Findings[i].RuleID < report.Findings[j].RuleID
	})
	sort.Slice(report.Coverage.Skipped, func(i, j int) bool {
		return report.Coverage.Skipped[i].Path < report.Coverage.Skipped[j].Path
	})
	return report
}

// containerRelPath strips the mount point so a report cites repo-relative
// paths. A reader should see `internal/foo.go`, not `/work/internal/foo.go`.
func containerRelPath(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(p), "/work"), "/")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ClaimsNoVulnerabilities is deliberately absent from this package, and this
// comment is where somebody looking for it finds out why. A pattern scan over
// a subset of files with eight rules cannot support that claim, and a report
// that made it would be read as an assurance nobody produced. The report
// carries coverage and limitations instead, and the caller draws its own
// conclusion from what was actually examined.
