package postrunqa

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This file is the gate's classifier: the pass that turns two EvidenceReports
// — one taken before the execution, one after — into Findings that carry an
// answer to the three questions the gate cannot act without.
//
//  1. Did this execution cause it? A signal present with the same signature in
//     both snapshots was already true before the agent started. It is recorded
//     as Baseline and never blocks; anything else would hand an agent the
//     repository's pre-existing breakage as its own failure.
//  2. Is it ours, and is it real? A finding about a repository, run, or session
//     this execution never owned is out of scope, and a defect that exists only
//     in the agent's closing prose is unverified. Neither is auto-fixable.
//  3. May a machine repair it? Releasing a lock reconciliation already calls
//     stale is mechanical. Throwing away a working tree of uncommitted changes
//     is not.
//
// Two rules are load-bearing.
//
// The classifier reads the TYPED fields of the evidence structs — Leaked,
// RetentionDecision, TargetEphemeral, Overdue, ExitCode, WorkspaceDisposition —
// and never parses the collector's Anomaly prose to decide anything. The prose
// is carried into Finding.Evidence verbatim so a human can read it, and that is
// all it is used for. Prose is for people; the typed fields are for decisions.
//
// And the agent's own closing report is cross-checked, not believed. Defect
// language in it ("failed", "could not", "pre-existing failure") is matched
// against the structured signals above. A claim that matches one inherits that
// signal's classification — including its attribution, so an agent calling
// something "pre-existing" does not make it so. A claim that matches nothing is
// kept as report-only and never blocks. It is never discarded: it is the only
// trace of something the agent thought was wrong and no source could see.

// ExecutionScope is the project/runtime blast radius one execution was granted.
// A finding whose subject falls outside it is recorded but rejected from
// auto-fix eligibility, because repairing it would touch state this execution
// was never permitted to write.
type ExecutionScope struct {
	ExecutionID   string
	WorkflowRunID string
	SessionIDs    []string
	// Repositories are the repository paths the execution was permitted to
	// write. A repository path equal to, or nested under, one of these is in
	// scope.
	Repositories []string
	// DataDir is the AO state root the execution ran against. Empty means the
	// execution had no data dir, which makes any hook-launcher finding
	// out of scope.
	DataDir string
}

// ScopeFromExecution derives the scope from the execution the collector
// already resolved, so the classifier and the collector agree on what "ours"
// means by construction rather than by two call sites happening to match.
func ScopeFromExecution(e Execution) ExecutionScope {
	repos := make([]string, 0, len(e.Repositories))
	for _, r := range e.Repositories {
		if strings.TrimSpace(r.RepoPath) != "" {
			repos = append(repos, r.RepoPath)
		}
	}
	return ExecutionScope{
		ExecutionID:   e.ID,
		WorkflowRunID: e.WorkflowRunID,
		SessionIDs:    append([]string(nil), e.SessionIDs...),
		Repositories:  repos,
		DataDir:       e.DataDir,
	}
}

func (s ExecutionScope) ownsRepo(repoPath string) bool {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return false
	}
	target := filepath.Clean(repoPath)
	for _, candidate := range s.Repositories {
		root := filepath.Clean(candidate)
		if target == root {
			return true
		}
		if strings.HasPrefix(target, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (s ExecutionScope) ownsSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	for _, candidate := range s.SessionIDs {
		if candidate == sessionID {
			return true
		}
	}
	return false
}

// ClassificationInput is the classifier's whole world: the two snapshots to
// diff and the scope to judge against.
type ClassificationInput struct {
	// Baseline is the evidence snapshot taken before the execution ran. It is
	// a pointer because "no baseline was captured" is a real state that must
	// not read the same as "a baseline was captured and it was clean": without
	// one, nothing can be attributed, and every finding is Ambiguous.
	Baseline *EvidenceReport
	// Post is the snapshot taken after the execution.
	Post EvidenceReport
	// Scope is the project/runtime scope the execution owned.
	Scope ExecutionScope
}

// Classification is the classifier's output: every finding it derived, from
// both snapshots and from the agent's report, each carrying its own verdict.
type Classification struct {
	ExecutionID string
	// ClassifiedAt mirrors the post-run snapshot's collection time rather than
	// reading a clock, so classifying the same pair of snapshots twice yields
	// the same answer.
	ClassifiedAt time.Time
	// HasBaseline reports whether a pre-run snapshot was available. When it is
	// false every attribution is Ambiguous, and that is a property of the
	// input, not of the execution.
	HasBaseline bool
	Findings    []Finding
}

// Blocking returns the findings that should stop the gate reporting the
// subject complete.
func (c Classification) Blocking() []Finding { return filterFindings(c.Findings, Finding.Blocking) }

// AutoFixable returns the findings an automated repair may attempt.
func (c Classification) AutoFixable() []Finding {
	return filterFindings(c.Findings, Finding.AutoFixEligible)
}

// BaselineFindings returns the findings that were already true before the
// execution ran. They are kept, and reported, precisely because they are the
// evidence for *not* blocking.
func (c Classification) BaselineFindings() []Finding {
	return filterFindings(c.Findings, func(f Finding) bool { return f.Attribution == AttributionBaseline })
}

// ReportOnly returns the defect claims from the agent's closing report that no
// structured source corroborated.
func (c Classification) ReportOnly() []Finding {
	return filterFindings(c.Findings, func(f Finding) bool { return f.Verification == VerificationReportOnly })
}

func filterFindings(in []Finding, keep func(Finding) bool) []Finding {
	var out []Finding
	for _, f := range in {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

// signal is one classifiable observation derived from a structured evidence
// source, before attribution and scope are decided. It is unexported because
// the classifier's output vocabulary is Finding; this is only the intermediate
// the diff runs over.
type signal struct {
	source EvidenceSource
	// subject is the identity half of the signature and is never normalized:
	// two sessions with identical breakage must stay two findings.
	subject string
	// detail is the kind half of the signature. It is built from typed
	// evidence fields, never from the collector's prose, so it carries no
	// counts, timestamps, or durations that would change between two
	// otherwise-identical snapshots.
	detail   string
	summary  string
	evidence string
	severity Severity
	safety   SafetyVerdict
	// scope and scopeReason are decided per signal because each source is
	// keyed on a different part of the execution. scope is tri-state on
	// purpose: "the execution never told us what it owned" is not the same
	// answer as "we checked and it is ours", and collapsing the two into a
	// bool is what lets an auto-fix run against state of unknown ownership.
	scope       FindingScope
	scopeReason string
	// matchTokens are the strings that, appearing in a line of the agent's
	// report, make that line a claim about this signal.
	matchTokens []string
}

func (s signal) signature() string {
	return string(s.source) + "|" + strings.TrimSpace(s.subject) + "|" + normalizeDetail(s.detail)
}

// normalizeDetail folds the incidental differences between two renderings of
// the same kind of problem. It deliberately does not touch digits: subjects
// are excluded from it already, and collapsing digits inside a detail would
// make "branch feat/x1" and "branch feat/x2" the same signature.
func normalizeDetail(detail string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(detail))), " ")
}

// Classify diffs the two snapshots and returns one classified finding per
// structured signal, plus one per defect claim in the agent's closing report.
func Classify(in ClassificationInput) Classification {
	out := Classification{
		ExecutionID:  firstNonEmpty(in.Post.ExecutionID, in.Scope.ExecutionID),
		ClassifiedAt: in.Post.CollectedAt,
		HasBaseline:  in.Baseline != nil,
	}

	baselineSignatures := map[string]bool{}
	baselineFailedSources := map[EvidenceSource]bool{}
	if in.Baseline != nil {
		for _, s := range deriveSignals(*in.Baseline, in.Scope) {
			baselineSignatures[s.signature()] = true
		}
		for _, e := range in.Baseline.SourceErrors {
			baselineFailedSources[e.Source] = true
		}
	}
	postFailedSources := map[EvidenceSource]bool{}
	for _, e := range in.Post.SourceErrors {
		postFailedSources[e.Source] = true
	}

	signals := deriveSignals(in.Post, in.Scope)
	findings := make([]Finding, 0, len(signals))
	byFinding := make([]signal, 0, len(signals))
	for _, s := range signals {
		f := Finding{
			Source:       "evidence:" + string(s.source),
			Signal:       s.summary,
			Evidence:     s.evidence,
			Severity:     s.severity,
			Subject:      s.subject,
			Signature:    s.signature(),
			Verification: VerificationEvidence,
			// A structured signal is reproducible when the source it came from
			// read cleanly. A source that partially failed may have reported a
			// snapshot nobody can reproduce, and pretending otherwise would let
			// an auto-fix act on a one-off reading.
			Reproducible: !postFailedSources[s.source],
			Safety:       s.safety,
		}
		f.Scope, f.ScopeReason = s.scope, s.scopeReason
		f.Attribution, f.AttributionReason = attribute(s, in.Baseline != nil, baselineSignatures, baselineFailedSources)
		findings = append(findings, f)
		byFinding = append(byFinding, s)
	}

	// Every source that could not be read is its own finding. Left implicit,
	// an unreadable source is indistinguishable from a source that reported
	// nothing — which is to say, it reads as evidence of cleanliness it never
	// provided. Recorded at info severity, so it informs without blocking.
	for _, e := range in.Post.SourceErrors {
		subject := firstNonEmpty(e.Subject, string(e.Source))
		findings = append(findings, Finding{
			Source:            "evidence:" + string(e.Source),
			Signal:            fmt.Sprintf("evidence source %s could not be read for %s", e.Source, subject),
			Evidence:          e.Message,
			Severity:          SeverityInfo,
			Subject:           subject,
			Signature:         string(e.Source) + "|" + subject + "|source-unreadable",
			Scope:             ScopeInScope,
			ScopeReason:       "the gate's own evidence collection for this execution",
			Attribution:       AttributionAmbiguous,
			AttributionReason: "a source that could not be read cannot be compared against the baseline",
			Verification:      VerificationEvidence,
			Reproducible:      false,
			Safety:            SafetyAmbiguous,
		})
	}

	// The report cross-check indexes signals and their classified findings in
	// lockstep, so it is handed exactly the prefix that came from signals --
	// not the source-error findings appended after them.
	signalFindings := findings[:len(byFinding)]
	findings = append(findings, classifyReport(in.Post.FinalAgentReport, byFinding, signalFindings)...)
	out.Findings = findings
	return out
}

func attribute(s signal, hasBaseline bool, baselineSignatures map[string]bool, baselineFailedSources map[EvidenceSource]bool) (Attribution, string) {
	if !hasBaseline {
		return AttributionAmbiguous, "no pre-run evidence snapshot was captured, so nothing can be compared"
	}
	if baselineSignatures[s.signature()] {
		return AttributionBaseline, "the same signature is present in the pre-run snapshot"
	}
	if baselineFailedSources[s.source] {
		return AttributionAmbiguous, fmt.Sprintf("the %s source could not be read pre-run, so its absence from the baseline proves nothing", s.source)
	}
	return AttributionNew, "absent from the pre-run snapshot and present after the execution"
}

// --- signal derivation -------------------------------------------------------

func deriveSignals(r EvidenceReport, scope ExecutionScope) []signal {
	out := make([]signal, 0, len(r.Git)+len(r.BranchLocks)+len(r.Wakes)+len(r.Processes)+len(r.Sessions)+len(r.RuntimeErrors)+1)
	out = append(out, gitSignals(r, scope)...)
	out = append(out, branchLockSignals(r, scope)...)
	out = append(out, hookLauncherSignals(r, scope)...)
	out = append(out, wakeSignals(r, scope)...)
	out = append(out, processSignals(r, scope)...)
	out = append(out, sessionSignals(r, scope)...)
	out = append(out, runtimeErrorSignals(r, scope)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].signature() < out[j].signature() })
	return out
}

func gitSignals(r EvidenceReport, scope ExecutionScope) []signal {
	var out []signal
	for _, g := range r.Git {
		repoVerdict, reason := repoScope(scope, g.RepoPath)
		repoName := filepath.Base(g.RepoPath)

		// One signal per changed path, not one per dirty repository. A tree
		// that was already dirty with A and is now dirty with A and B has to
		// classify A as baseline and B as new; a repository-level signal could
		// only say "still dirty" and would lose that entirely.
		for _, c := range g.Changes {
			out = append(out, signal{
				source:      SourceGit,
				subject:     g.RepoPath + "::" + c.Path,
				detail:      "uncommitted:" + c.Status,
				summary:     fmt.Sprintf("uncommitted change in %s: %s %s", repoName, c.Status, c.Path),
				evidence:    g.Anomaly,
				severity:    SeverityMajor,
				safety:      SafetyUnsafe,
				scope:       repoVerdict,
				scopeReason: reason,
				matchTokens: []string{c.Path, filepath.Base(c.Path), repoName},
			})
		}
		if g.Dirty && len(g.Changes) == 0 {
			out = append(out, signal{
				source:      SourceGit,
				subject:     g.RepoPath,
				detail:      "dirty",
				summary:     fmt.Sprintf("%s has uncommitted changes", repoName),
				evidence:    g.Anomaly,
				severity:    SeverityMajor,
				safety:      SafetyUnsafe,
				scope:       repoVerdict,
				scopeReason: reason,
				matchTokens: []string{repoName, g.RepoPath},
			})
		}
		if g.CurrentBranch != "" && g.ConfiguredBranch != "" && g.CurrentBranch != g.ConfiguredBranch {
			out = append(out, signal{
				source:      SourceGit,
				subject:     g.RepoPath,
				detail:      "branch-mismatch:" + g.CurrentBranch + "!=" + g.ConfiguredBranch,
				summary:     fmt.Sprintf("%s is checked out on %q, not the configured %q", repoName, g.CurrentBranch, g.ConfiguredBranch),
				evidence:    g.Anomaly,
				severity:    SeverityMajor,
				safety:      SafetyAmbiguous,
				scope:       repoVerdict,
				scopeReason: reason,
				matchTokens: []string{repoName, g.CurrentBranch, g.ConfiguredBranch},
			})
		}
	}
	return out
}

func branchLockSignals(r EvidenceReport, scope ExecutionScope) []signal {
	var out []signal
	for _, l := range r.BranchLocks {
		repoVerdict, reason := repoScope(scope, l.RepoPath)
		subject := firstNonEmpty(l.LockKey, l.LockID)
		add := func(detail, summary string, severity Severity, safety SafetyVerdict) {
			out = append(out, signal{
				source:      SourceBranchLock,
				subject:     subject,
				detail:      detail,
				summary:     summary,
				evidence:    l.Anomaly,
				severity:    severity,
				safety:      safety,
				scope:       repoVerdict,
				scopeReason: reason,
				matchTokens: []string{subject, l.Branch, filepath.Base(l.RepoPath), "branch lock"},
			})
		}
		if r.ExecutionEnded && l.State == domain.BranchLockHeld {
			// Releasing a lock whose execution has ended is exactly what the
			// manager's own reconciliation does, so it is mechanical.
			add("held-after-execution-end",
				fmt.Sprintf("branch lock %s is still held by %s after the execution ended", subject, l.Owner),
				SeverityMajor, SafetyAutoFix)
		}
		if l.RetentionDecision == branchlock.RetentionRelease {
			add("stale-retention:"+l.RetentionReason,
				fmt.Sprintf("branch lock %s is stale: %s", subject, l.RetentionReason),
				SeverityMajor, SafetyAutoFix)
		}
	}
	return out
}

func hookLauncherSignals(r EvidenceReport, scope ExecutionScope) []signal {
	h := r.HookLauncher
	if !h.Probed {
		return nil
	}
	// A probed launcher with no data dir on the scope is not someone else's
	// launcher -- it is a launcher whose owner nobody recorded.
	launcherScope := ScopeInScope
	reason := "the hook launcher under the execution's own data dir"
	if strings.TrimSpace(scope.DataDir) == "" {
		launcherScope = ScopeUnknown
		reason = "the execution declares no data dir, so ownership of this launcher cannot be established"
	}
	subject := firstNonEmpty(h.Path, "hook-launcher")
	// Every hook-launcher defect has the same repair -- reinstall the shim
	// against a durable binary -- which is idempotent and destroys nothing, so
	// the safety verdict is a property of the source rather than of the case.
	mk := func(detail, summary string) signal {
		return signal{
			source:      SourceHookLauncher,
			subject:     subject,
			detail:      detail,
			summary:     summary,
			evidence:    h.Anomaly,
			severity:    SeverityMajor,
			safety:      SafetyAutoFix,
			scope:       launcherScope,
			scopeReason: reason,
			matchTokens: []string{"hook launcher", "hooks", filepath.Base(h.Target)},
		}
	}
	// Mirrors the collector's own precedence, but decided from typed fields
	// rather than by re-reading the prose the collector rendered from them.
	switch {
	case !h.Present:
		return []signal{mk("shim-missing", "hook launcher shim is missing at "+h.Path)}
	case !h.Executable:
		return []signal{mk("shim-not-executable", "hook launcher shim is not executable")}
	case h.Target == "":
		return []signal{mk("no-target", "hook launcher shim names no target binary")}
	case h.TargetEphemeral:
		return []signal{mk("target-ephemeral", "hook launcher targets an ephemeral Go build-cache binary ("+h.Target+")")}
	case !h.TargetPresent:
		return []signal{mk("target-missing", "hook launcher target does not exist ("+h.Target+")")}
	case !h.TargetExecutable:
		return []signal{mk("target-not-executable", "hook launcher target is not executable ("+h.Target+")")}
	}
	return nil
}

func wakeSignals(r EvidenceReport, scope ExecutionScope) []signal {
	verdict, reason := idScope(scope.WorkflowRunID, r.WorkflowRunID,
		"a wake schedule owned by this execution's workflow run",
		"the wake schedule belongs to a workflow run this execution does not own",
		"no workflow run id is recorded on either side, so wake ownership cannot be established")
	var out []signal
	for _, w := range r.Wakes {
		add := func(detail, summary string, severity Severity, safety SafetyVerdict) {
			out = append(out, signal{
				source:      SourceWake,
				subject:     w.ScheduleID,
				detail:      detail,
				summary:     summary,
				evidence:    w.Anomaly,
				severity:    severity,
				safety:      safety,
				scope:       verdict,
				scopeReason: reason,
				matchTokens: []string{w.ScheduleID, w.StepID, "wake"},
			})
		}
		if r.ExecutionEnded {
			add("open-after-execution-end",
				fmt.Sprintf("wake %s is still %s after the execution ended", w.ScheduleID, w.Status),
				SeverityMinor, SafetyAutoFix)
		}
		if w.Overdue {
			add("overdue", fmt.Sprintf("wake %s is overdue and was never fired", w.ScheduleID), SeverityMajor, SafetyAmbiguous)
		}
		if w.LastError != "" {
			// The error TEXT is deliberately kept out of the detail: it is the
			// most volatile string in the whole report, and a signature that
			// moves with it would classify every retry as a new finding.
			add("retrying-after-error",
				fmt.Sprintf("wake %s is retrying after an error", w.ScheduleID),
				SeverityMajor, SafetyAmbiguous)
		}
	}
	return out
}

func processSignals(r EvidenceReport, scope ExecutionScope) []signal {
	// Process records are read keyed on the execution id, so a record whose
	// report carries the same id as the scope is this execution's. An
	// execution with no id, or a report with none, proves nothing either way:
	// that is ScopeUnknown, not a free pass.
	verdict, reason := idScope(scope.ExecutionID, r.ExecutionID,
		"a process this execution ran",
		"the process belongs to a different execution",
		"neither the execution nor the evidence report carries an execution id, so process ownership cannot be established")
	var out []signal
	for _, p := range r.Processes {
		add := func(detail, summary string, severity Severity, safety SafetyVerdict) {
			out = append(out, signal{
				source:      SourceProcess,
				subject:     p.Label,
				detail:      detail,
				summary:     summary,
				evidence:    firstNonEmpty(p.Anomaly, p.StderrTail),
				severity:    severity,
				safety:      safety,
				scope:       verdict,
				scopeReason: reason,
				matchTokens: []string{p.Label},
			})
		}
		switch {
		case p.TimedOut:
			add("timed-out", fmt.Sprintf("%s timed out", p.Label), SeverityBlocker, SafetyAmbiguous)
		case p.ExitCode != nil && *p.ExitCode != 0:
			// A non-zero exit is the gate's home turf: a failing build or vet
			// is what the repair cycle exists to clear.
			add(fmt.Sprintf("exit-status:%d", *p.ExitCode),
				fmt.Sprintf("%s exited with status %d", p.Label, *p.ExitCode),
				SeverityBlocker, SafetyAutoFix)
		case p.Running && r.ExecutionEnded:
			add("running-after-execution-end",
				fmt.Sprintf("%s is still running after the execution ended", p.Label),
				SeverityMajor, SafetyUnsafe)
		case p.ExitCode == nil && !p.Running && r.ExecutionEnded:
			add("no-exit-status",
				fmt.Sprintf("%s recorded no exit status", p.Label),
				SeverityMinor, SafetyAmbiguous)
		}
	}
	return out
}

func sessionSignals(r EvidenceReport, scope ExecutionScope) []signal {
	var out []signal
	for _, s := range r.Sessions {
		sessionScope, reason := ScopeInScope, "a session this execution ran in"
		switch {
		case len(scope.SessionIDs) == 0:
			sessionScope = ScopeUnknown
			reason = "the execution declares no sessions, so ownership of " + s.SessionID + " cannot be established"
		case !scope.ownsSession(s.SessionID):
			sessionScope = ScopeOutOfScope
			reason = "the session is not one this execution ran in"
		}
		add := func(detail, summary string, severity Severity, safety SafetyVerdict) {
			out = append(out, signal{
				source:      SourceSession,
				subject:     s.SessionID,
				detail:      detail,
				summary:     summary,
				evidence:    s.Anomaly,
				severity:    severity,
				safety:      safety,
				scope:       sessionScope,
				scopeReason: reason,
				matchTokens: []string{s.SessionID},
			})
		}
		switch {
		case !s.Found:
			add("session-row-missing", fmt.Sprintf("session %s has no durable row", s.SessionID), SeverityMajor, SafetyAmbiguous)
		case !s.Terminated && r.ExecutionEnded:
			add("never-terminated", fmt.Sprintf("session %s was never terminated", s.SessionID), SeverityMajor, SafetyAmbiguous)
		case s.Terminated && !s.CleanupRecorded:
			add("no-cleanup-facts", fmt.Sprintf("session %s terminated with no cleanup facts recorded", s.SessionID), SeverityMajor, SafetyAutoFix)
		case s.Terminated:
			if !s.RuntimeReleased {
				add("runtime-not-released", fmt.Sprintf("session %s never confirmed its runtime released", s.SessionID), SeverityMajor, SafetyAutoFix)
			}
			switch s.WorkspaceDisposition {
			case domain.DispositionPending:
				add("workspace-teardown-pending", fmt.Sprintf("session %s workspace teardown is still pending", s.SessionID), SeverityMajor, SafetyAutoFix)
			case domain.DispositionFailed:
				add("workspace-teardown-failed", fmt.Sprintf("session %s workspace teardown failed", s.SessionID), SeverityMajor, SafetyAutoFix)
			case domain.DispositionPreservedDirty:
				// Preserved *because* it holds uncommitted work. Sweeping it is
				// the one repair guaranteed to destroy something.
				add("workspace-preserved-dirty", fmt.Sprintf("session %s workspace was preserved because it holds uncommitted changes", s.SessionID), SeverityMinor, SafetyUnsafe)
			case domain.DispositionRemoved, domain.DispositionNotApplicable:
			}
			if s.FailureCode != "" {
				add("teardown-failure:"+s.FailureCode, fmt.Sprintf("session %s last teardown failure: %s", s.SessionID, s.FailureCode), SeverityMajor, SafetyAmbiguous)
			}
		}
	}
	return out
}

// idScope decides ownership from a pair of identifiers that must match: the
// one the execution scope declares and the one the evidence report carries.
// Either being empty is ScopeUnknown -- an identifier nobody recorded is not
// an identifier that matches.
func idScope(scopeID, reportID, inReason, outReason, unknownReason string) (FindingScope, string) {
	scopeID, reportID = strings.TrimSpace(scopeID), strings.TrimSpace(reportID)
	if scopeID == "" || reportID == "" {
		return ScopeUnknown, unknownReason
	}
	if scopeID == reportID {
		return ScopeInScope, inReason
	}
	return ScopeOutOfScope, outReason
}

// repoScope decides ownership of one repository.
//
// An execution that declares no repositories does not thereby own every
// repository the collector happened to probe. Missing scope metadata is
// ScopeUnknown, which blocks (it may well be ours) but is never auto-fixed
// (we cannot show that it is).
// runtimeErrorSignals turns the daemon's and the runtimes' own error records
// into classifiable signals.
//
// The whole point of putting them through the same diff as everything else is
// that a daemon error is the single most likely thing to be pre-existing: a
// reaper that has been logging the same failure for a week is not the agent's
// doing, and blocking a task on it would be exactly the misattribution this
// package exists to prevent. So the signature is the error's IDENTITY --
// component plus code, or component plus a fingerprint of the message when the
// component supplies no code -- and never its count, its timestamps, or the
// ids interpolated into its message.
func runtimeErrorSignals(r EvidenceReport, scope ExecutionScope) []signal {
	out := make([]signal, 0, len(r.RuntimeErrors))
	for _, e := range r.RuntimeErrors {
		component := firstNonEmpty(e.Component, "unnamed-component")
		verdict, reason := runtimeErrorScope(scope, r, e)

		severity := SeverityMajor
		switch {
		case e.Fatal():
			// A fatal record means the component gave up, not that it retried.
			severity = SeverityBlocker
		case e.Warning():
			severity = SeverityMinor
		}

		summary := fmt.Sprintf("%s recorded a runtime error", component)
		if e.Code != "" {
			summary += " (" + e.Code + ")"
		}
		if e.Message != "" {
			summary += ": " + e.Message
		}

		out = append(out, signal{
			source:  SourceRuntimeLog,
			subject: component,
			detail:  "runtime-error:" + runtimeErrorFingerprint(e.RuntimeErrorRecord),
			summary: summary,
			// The rendered message is evidence for a human, never the thing
			// the diff turns on.
			evidence: firstNonEmpty(e.Anomaly, e.Message),
			severity: severity,
			// A daemon error's repair is never mechanical from here: the gate
			// has no idea what the component needs, only that it failed.
			safety:      SafetyAmbiguous,
			scope:       verdict,
			scopeReason: reason,
			matchTokens: []string{component, e.Code, e.SessionID},
		})
	}
	return out
}

// runtimeErrorScope decides ownership of one runtime error. A record that
// names a session is that session's; one that does not is the daemon's, and
// belongs to whichever execution the report was collected for.
func runtimeErrorScope(scope ExecutionScope, r EvidenceReport, e RuntimeErrorEvidence) (FindingScope, string) {
	if strings.TrimSpace(e.SessionID) != "" {
		if len(scope.SessionIDs) == 0 {
			return ScopeUnknown, "the execution declares no sessions, so ownership of the error in " + e.SessionID + " cannot be established"
		}
		if !scope.ownsSession(e.SessionID) {
			return ScopeOutOfScope, "the error belongs to session " + e.SessionID + ", which this execution did not run in"
		}
		return ScopeInScope, "a runtime error in a session this execution ran in"
	}
	return idScope(scope.ExecutionID, r.ExecutionID,
		"a daemon error recorded for this execution",
		"the error was recorded for a different execution",
		"the error names no session and neither side carries an execution id, so ownership cannot be established")
}

// runtimeErrorFingerprint is what makes two runtime error records the same
// error across two snapshots.
//
// A component's own code is used whenever it has one, because it is the only
// part of an error record that is guaranteed stable across a rephrasing. Only
// when there is no code does the message get fingerprinted, and then the
// digits come out: session ids, ports, pids, durations, and attempt counters
// are exactly the parts of a message that differ between two occurrences of
// the same failure, and leaving them in would report every recurrence as new.
func runtimeErrorFingerprint(rec RuntimeErrorRecord) string {
	if code := strings.TrimSpace(rec.Code); code != "" {
		return strings.ToLower(code)
	}
	var b strings.Builder
	lastWasDigit := false
	for _, r := range strings.ToLower(strings.Join(strings.Fields(rec.Message), " ")) {
		if r >= '0' && r <= '9' {
			if !lastWasDigit {
				b.WriteByte('#')
			}
			lastWasDigit = true
			continue
		}
		lastWasDigit = false
		b.WriteRune(r)
	}
	fingerprint := b.String()
	// Long messages tail off into the specifics of one occurrence; the head is
	// what identifies the failure.
	if len(fingerprint) > 160 {
		fingerprint = fingerprint[:160]
	}
	return fingerprint
}

func repoScope(scope ExecutionScope, repoPath string) (FindingScope, string) {
	if len(scope.Repositories) == 0 {
		return ScopeUnknown, "the execution declares no repository scope, so ownership of " + repoPath + " cannot be established"
	}
	if scope.ownsRepo(repoPath) {
		return ScopeInScope, "a repository this execution was permitted to write"
	}
	return ScopeOutOfScope, fmt.Sprintf("%s is not among the repositories this execution was permitted to write", repoPath)
}

// --- the agent's closing report ---------------------------------------------

// defectPhrases is the vocabulary that makes a line of the agent's report a
// claim worth checking. It is intentionally broad: over-matching costs an
// info-severity finding nobody has to act on, while under-matching silently
// drops the only record that the agent thought something was wrong.
var defectPhrases = []string{
	"pre-existing failure",
	"pre-existing",
	"remaining issue",
	"remaining issues",
	"still failing",
	"still fails",
	"could not",
	"couldn't",
	"unable to",
	"did not pass",
	"doesn't pass",
	"failing",
	"failure",
	"failed",
	"error",
	"broken",
}

// reportStopTokens are match tokens too generic to identify anything. A claim
// containing "wake" or "main" is not thereby a claim about a specific wake
// schedule or branch.
var reportStopTokens = map[string]bool{
	"wake": true, "main": true, "master": true, "hooks": true,
	"repo": true, "root": true, "test": true, "tests": true,
}

// classifyReport turns defect language in the agent's closing report into
// findings, cross-checked against the structured signals rather than trusted.
//
// A claim that names a subject some structured source also reported inherits
// that finding's classification wholesale — attribution included. This is the
// point of the cross-check: an agent writing "pre-existing failure" about a
// signal absent from the pre-run snapshot does not get to relabel it, and an
// agent writing "error" about something the baseline already had does not get
// to blame it on this execution either.
//
// A claim that names nothing any source reported becomes a report-only
// finding: info severity, never blocking, never auto-fixed, and never dropped.
func classifyReport(report string, signals []signal, classified []Finding) []Finding {
	claims := extractReportClaims(report)
	if len(claims) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(claims))
	for _, claim := range claims {
		lower := strings.ToLower(claim)
		matchIdx := -1
		for i, s := range signals {
			if signalMatchesClaim(s, lower) {
				matchIdx = i
				break
			}
		}
		f := Finding{
			Source:    "final_agent_report",
			Signal:    claim,
			Evidence:  claim,
			Subject:   "final agent report",
			Signature: "final_agent_report|" + normalizeDetail(claim),
		}
		if matchIdx < 0 {
			f.Severity = SeverityInfo
			f.Scope = ScopeInScope
			f.ScopeReason = "the closing report of this execution's own agent"
			f.Attribution = AttributionAmbiguous
			f.AttributionReason = "no structured source reported this, so there is nothing to compare against the baseline"
			f.Verification = VerificationReportOnly
			f.Reproducible = false
			f.Safety = SafetyAmbiguous
			out = append(out, f)
			continue
		}
		backing := classified[matchIdx]
		f.Severity = backing.Severity
		f.Scope = backing.Scope
		f.ScopeReason = backing.ScopeReason
		f.Attribution = backing.Attribution
		f.AttributionReason = backing.AttributionReason + " (per the structured evidence, not the report's own wording)"
		f.Verification = VerificationEvidence
		f.CorroboratedBy = backing.Signature
		f.Reproducible = backing.Reproducible
		// The report claim is a description of the finding, never a repair
		// plan for it. Repairs are dispatched from the structured finding the
		// claim corroborates, so this one is never itself auto-fixed.
		f.Safety = SafetyAmbiguous
		f.Subject = backing.Subject
		out = append(out, f)
	}
	return out
}

func signalMatchesClaim(s signal, lowerClaim string) bool {
	for _, token := range s.matchTokens {
		token = strings.ToLower(strings.TrimSpace(token))
		if len(token) < 4 || reportStopTokens[token] {
			continue
		}
		if strings.Contains(lowerClaim, token) {
			return true
		}
	}
	return false
}

// extractReportClaims splits the report into candidate sentences and keeps the
// ones carrying defect language.
func extractReportClaims(report string) []string {
	if strings.TrimSpace(report) == "" {
		return nil
	}
	var claims []string
	seen := map[string]bool{}
	for _, line := range strings.Split(report, "\n") {
		for _, sentence := range splitSentences(line) {
			sentence = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(sentence), "-*# \t"))
			if sentence == "" {
				continue
			}
			lower := strings.ToLower(sentence)
			if !containsAny(lower, defectPhrases) {
				continue
			}
			if seen[lower] {
				continue
			}
			seen[lower] = true
			claims = append(claims, sentence)
		}
	}
	return claims
}

func splitSentences(line string) []string {
	var out []string
	start := 0
	for i, r := range line {
		if r != '.' && r != ';' && r != '!' {
			continue
		}
		// A period followed by a non-space is a version, a path, or `./...`,
		// not the end of a sentence.
		if i+1 < len(line) && line[i+1] != ' ' {
			continue
		}
		out = append(out, line[start:i])
		start = i + 1
	}
	if start < len(line) {
		out = append(out, line[start:])
	}
	return out
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
