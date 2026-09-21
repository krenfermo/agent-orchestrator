package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// deliverable_observability.go — the check that refuses to spend a dispatch on
// work whose required deliverable git cannot see.
//
// The incident (MEDUSA, 2026-09-09, recorded in docs/roadmap.md §Frente 1): a
// run stopped on `ambiguous_worker_state` because the deliverable the task
// asked for lived at a path covered by the repository's `.gitignore`. AO was
// right — there was no verifiable change in git — and the diagnosis still cost
// a complete human intervention, because AO cannot tell "the worker did
// nothing" apart from "the worker produced something git cannot see". Both
// present as an idle worker over an unchanged tree.
//
// The two are indistinguishable AFTERWARDS, and completely distinguishable
// BEFOREHAND: the plan says which paths the task must produce, and the
// repository says which paths it ignores. Asking those two questions before
// the launch costs one `git check-ignore` and turns a run that ends in an
// unreadable stop, minutes or hours later, into a refusal that names the path
// and the rule that hides it.
//
// What makes the stop inevitable rather than merely likely, once a required
// deliverable is ignored:
//
//   - the completion classifier's only evidence is git (HeadSHA against the
//     checkpoint's BaseSHA, plus dirty/untracked state). An ignored path moves
//     none of them, so a mutating task that produced exactly what was asked
//     reads as a task that produced nothing.
//   - the work would not survive anyway. StashUncommitted and
//     MaterializeIntegrationCommit both build their commit through
//     `git add -A` without `-f` (adapters/workspace/gitworktree/workspace.go),
//     which honours `.gitignore` — so the deliverable is silently dropped from
//     the preserve commit and from the integration commit.
//
// So this is not a heuristic about what might go wrong. It is AO declining to
// start work it has already proven it could neither observe nor keep.
//
// Three properties are load-bearing, and they are deliberately the same three
// provider_preflight.go rests on:
//
//   - no probe wired is not a refusal. It is exactly the previous behaviour.
//   - a probe that ERRORS is not a refusal either. "AO could not ask git" is
//     unknown, and grounding a dispatch on an unknown would be worse than the
//     incident this exists for.
//   - it never edits anybody's `.gitignore`. Force-adding a path on a person's
//     behalf, or rewriting their ignore rules to make a dispatch proceed, is
//     the same class of mistake as answering a provider's trust prompt for
//     them. It reports; the person decides.

// WorkflowErrorDeliverableNotObservable is the attempt error class for a
// dispatch refused here: everything this task is required to produce sits at a
// path the repository ignores, so neither the completion classifier nor the
// integration commit could ever see it.
//
// It is a class of its own rather than a flavour of ambiguous_worker_state
// because it is the opposite kind of statement. `ambiguous_worker_state` is AO
// admitting it cannot prove what happened, after the fact; this is AO proving,
// in advance, exactly what would have gone wrong — and the remedy (untrack the
// path, or point the task at one git can see) is nothing like "inspect the
// session".
const WorkflowErrorDeliverableNotObservable domain.WorkflowErrorClass = "deliverable_not_observable"

// ReasonDeliverableNotObservable is the canonical attention reason for that
// class. See attention.go for the human action it carries.
const ReasonDeliverableNotObservable = string(WorkflowErrorDeliverableNotObservable)

// DeliverableSource says where a required path was declared, because the two
// sources carry different weight and a person reading the refusal is owed the
// difference: a verification file check is a machine-readable assertion the
// plan made, while a criterion path is one AO read out of the requirement text.
type DeliverableSource string

const (
	// DeliverableFromVerification is a VerificationFileCheck with Exists true:
	// the plan naming, structurally, a file that must be there afterwards.
	DeliverableFromVerification DeliverableSource = "verification_file"
	// DeliverableFromCriterion is a path named by an acceptance criterion. The
	// criteria are the requirements in force (effective_task_specification.go),
	// so a path one of them names is a required path -- the only inference is
	// the extraction, which uses the package's existing conservative rule and
	// drops everything it is unsure about.
	DeliverableFromCriterion DeliverableSource = "acceptance_criterion"
)

// RequiredDeliverable is one path this task is required to produce.
type RequiredDeliverable struct {
	// Path is repository-relative and slash-separated.
	Path   string
	Source DeliverableSource
	// Declaration is the verbatim text that named it -- the criterion, or the
	// verification check's own path -- so the refusal can quote the requirement
	// rather than paraphrase it.
	Declaration string
}

// IgnoredDeliverable is one required path the repository ignores, with the
// exact rule that hides it. The rule is carried, not merely the verdict,
// because "backend/out/report.pdf is ignored" sends a person looking and
// ".gitignore:12:out/" sends them to the line.
type IgnoredDeliverable struct {
	Path string
	// RuleSource is the file holding the pattern (".gitignore",
	// ".git/info/exclude", a global excludes file), RuleLine its 1-based line,
	// and Pattern the pattern itself. All three are empty/zero only when the
	// probe could not attribute the match, which no current probe does.
	RuleSource string
	RuleLine   int
	Pattern    string
}

// DeliverableIgnoreProbe is the narrow, read-only port that answers "which of
// these paths does this repository ignore?".
//
// Optional. A nil implementation keeps AO's pre-existing behaviour exactly.
//
// Two contract points the implementation must honour, because the whole check
// is wrong without them:
//
//   - a TRACKED path is never ignored, whatever patterns match it. git already
//     works this way (`git check-ignore` omits tracked paths unless asked not
//     to), and it is the correct answer: git sees changes to a tracked file
//     regardless of any rule that would have ignored it.
//   - a NEGATED pattern (`!keep.log`) re-includes the path, so the path is not
//     ignored. `git check-ignore -v` reports the negating rule as a match, and
//     a probe that reads "a rule matched" as "ignored" gets every exception in
//     every .gitignore in the repository exactly backwards.
type DeliverableIgnoreProbe interface {
	// IgnoredPaths returns one entry per path the repository ignores, in any
	// order. Paths that do not exist on disk are answered normally: the
	// question is about the NAME, and a deliverable that has not been produced
	// yet is precisely the case this check runs in. An error means the question
	// could not be asked, which is unknown -- never a refusal.
	IgnoredPaths(ctx stdctx.Context, repoPath string, paths []string) ([]IgnoredDeliverable, error)
}

// deliverableExtensions are file types a plan can require as a deliverable that
// the scope classifier's codeExtensions deliberately does not admit: reports,
// exports, archives and images. They are listed here, local to this check,
// rather than added to codeExtensions, because that set governs write-scope
// estimation and conflict detection for every task in every plan -- widening it
// would serialize work that does not conflict, to fix something that is not the
// subject here.
// urlRe matches an absolute URL with a scheme, so one can be removed from a
// requirement sentence before any of it is read as a path.
var urlRe = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]*://\S+`)

var deliverableExtensions = map[string]bool{
	".pdf": true, ".csv": true, ".tsv": true, ".xlsx": true, ".xls": true,
	".docx": true, ".doc": true, ".pptx": true, ".odt": true, ".ods": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".zip": true, ".tar": true, ".gz": true, ".tgz": true,
	".log": true, ".xml": true, ".jsonl": true, ".ndjson": true,
	".parquet": true, ".sqlite": true, ".db": true, ".bin": true, ".out": true,
}

// RequiredDeliverables derives, deterministically, every path this task is
// required to produce. No IO, no model call: the same artifact always yields
// the same list, sorted, de-duplicated, and with the strongest declaration
// kept when both sources name one path.
//
// It is exported so the refusal can be reproduced from a stored artifact
// without re-running a dispatch.
func RequiredDeliverables(artifact PlanArtifact) []RequiredDeliverable {
	byPath := map[string]RequiredDeliverable{}
	add := func(d RequiredDeliverable) {
		if d.Path == "" {
			return
		}
		// A verification check outranks a criterion mention of the same path:
		// it is the stronger claim, and it is the one a person can act on
		// without re-reading prose. Between two mentions of equal rank the
		// FIRST wins, so the declaration quoted back to a person is the
		// earliest requirement that named the path rather than whichever one
		// happened to be iterated last.
		if prev, ok := byPath[d.Path]; ok && (prev.Source == DeliverableFromVerification || prev.Source == d.Source) {
			return
		}
		byPath[d.Path] = d
	}

	for _, check := range artifact.Verification.Files {
		// Exists false asserts a path is ABSENT afterwards. An absent file is
		// not a deliverable and an ignored one is not a problem, so those are
		// skipped rather than reported.
		if !check.Exists {
			continue
		}
		p, _, ok := cleanPathToken(check.Path)
		if !ok {
			continue
		}
		add(RequiredDeliverable{Path: p, Source: DeliverableFromVerification, Declaration: check.Path})
	}

	for _, criterion := range artifact.AcceptanceCriteria {
		for _, p := range deliverablePathsInText(criterion) {
			add(RequiredDeliverable{
				Path:        p,
				Source:      DeliverableFromCriterion,
				Declaration: strings.TrimSpace(criterion),
			})
		}
	}

	out := make([]RequiredDeliverable, 0, len(byPath))
	for _, d := range byPath {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// deliverablePathsInText pulls required paths out of one requirement sentence.
//
// It is the package's own extractPaths -- the conservative rule that drops
// everything it is unsure about, because an over-eager path here is a refused
// dispatch over a phantom requirement -- plus exactly one widening: a token
// whose extension names an artifact type a plan can require (a report, an
// export, an archive) is admitted on the same terms a code file is. Without
// that widening the check would miss the very shape the incident had, since
// `out/report.pdf` is not a code path and never will be.
func deliverablePathsInText(text string) []string {
	// URLs come out first. The shared path tokenizer splits on the scheme
	// separator, so "https://example.com/report.pdf" reaches it as the token
	// "example.com/report.pdf" -- which has a slash and an artifact extension
	// and is, by every rule below, a repository path. It is not one, and
	// refusing a dispatch because a criterion linked to a document would be a
	// refusal over nothing. Removing them before tokenizing is done here rather
	// than in normalizePath because that function governs write-scope
	// estimation for every task in every plan, and this is not the change to
	// make to it.
	text = urlRe.ReplaceAllString(text, " ")

	seen := map[string]bool{}
	out := []string{}
	// roots is nil: this runs on one task's own text, with no plan-wide corpus
	// to discover repository roots from. The effect is that a two-segment path
	// with no recognised extension ("out/report") is dropped, which is the
	// conservative direction.
	for _, p := range extractPaths(text, nil) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, tok := range pathTokenRe.FindAllString(text, -1) {
		cleaned, segments, ok := cleanPathToken(tok)
		if !ok || seen[cleaned] {
			continue
		}
		if !deliverableExtensions[strings.ToLower(path.Ext(cleaned))] {
			continue
		}
		// A bare artifact filename ("report.pdf") is admitted, exactly as a
		// bare "go.mod" is: an extension this specific is not prose.
		if len(segments) > 1 {
			for _, s := range segments {
				if englishSlashWords[strings.ToLower(s)] {
					ok = false
					break
				}
			}
		}
		if !ok {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	sort.Strings(out)
	return out
}

// deliverableVerdict is the decision derived from one task's requirements and
// the repository's answer about them.
type deliverableVerdict struct {
	// Ready is true when the dispatch may proceed.
	Ready bool
	// Ignored are the required paths the repository hides, sorted by path.
	// Populated only on a refusal.
	Ignored []IgnoredDeliverable
	Detail  string
}

// evaluateDeliverableObservability is the whole policy, as a pure function, so
// every rule below is testable without a repository.
//
// It refuses on ONE condition, and the narrowness is the point:
//
//	the task is mutating, it declares at least one required deliverable, and
//	EVERY required deliverable is ignored.
//
// Each clause removes a way of being wrong:
//
//   - MUTATING. A task the plan declared read-only is required to change
//     nothing, so it has no deliverable to hide and read_only_completion.go
//     already reads an unchanged tree as its success. Unspecified is treated as
//     mutating, as everywhere else.
//   - AT LEAST ONE. A task that names no path is not thereby suspicious. Most
//     tasks name none; refusing them would ground the product.
//   - EVERY. If even one required path is observable, the worker's work lands
//     somewhere git can see, the completion classifier gets its evidence, and
//     the run does not end where MEDUSA's did. Refusing there would stop
//     perfectly good tasks that mention an ignored build output in passing
//     ("the build writes dist/app.js and the tests pass") -- which is a common
//     sentence and not a defect. The condition this check exists for is the
//     one where AO has NOTHING to observe.
func evaluateDeliverableObservability(intent domain.WorkflowWriteIntent, required []RequiredDeliverable, ignored []IgnoredDeliverable) deliverableVerdict {
	if intent.ReadOnly() || len(required) == 0 || len(ignored) == 0 {
		return deliverableVerdict{Ready: true}
	}
	hidden := map[string]IgnoredDeliverable{}
	for _, ig := range ignored {
		// A path the repository reported but the task never required is not
		// this check's business, and letting one in would let a probe widen the
		// refusal beyond what the plan actually asked for.
		hidden[ig.Path] = ig
	}
	observable := 0
	matched := make([]IgnoredDeliverable, 0, len(required))
	for _, d := range required {
		ig, ok := hidden[d.Path]
		if !ok {
			observable++
			continue
		}
		matched = append(matched, ig)
	}
	if observable > 0 || len(matched) == 0 {
		return deliverableVerdict{Ready: true}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Path < matched[j].Path })
	return deliverableVerdict{
		Ignored: matched,
		Detail:  describeIgnoredDeliverables(required, matched),
	}
}

// describeIgnoredDeliverables writes the refusal a person reads. It names every
// path, the rule that hides it and where that rule lives, states the two
// consequences as facts rather than predictions, and gives the two remedies --
// because a refusal that does not say what to do next is the unreadable stop
// this check was built to replace.
func describeIgnoredDeliverables(required []RequiredDeliverable, ignored []IgnoredDeliverable) string {
	decl := map[string]RequiredDeliverable{}
	for _, d := range required {
		decl[d.Path] = d
	}
	var b strings.Builder
	noun := "the only deliverable this task requires is"
	if len(ignored) > 1 {
		noun = fmt.Sprintf("all %d deliverables this task requires are", len(ignored))
	}
	fmt.Fprintf(&b, "deliverable observability: %s at a path this repository ignores, so git could not show the work and the integration commit would drop it", noun)
	for _, ig := range ignored {
		b.WriteString("; ")
		b.WriteString(ig.Path)
		if ig.Pattern != "" {
			fmt.Fprintf(&b, " (ignored by %s)", formatIgnoreRule(ig))
		}
		if d, ok := decl[ig.Path]; ok && d.Declaration != "" && d.Declaration != ig.Path {
			fmt.Fprintf(&b, ", required by %q", d.Declaration)
		}
	}
	b.WriteString(". Either stop ignoring that path (remove or negate the rule, or track the file with `git add -f`), or change the task to deliver somewhere git can see, then continue this run.")
	return b.String()
}

// formatIgnoreRule renders one rule as git itself prints it, `source:line:pattern`,
// so the string can be pasted into a search and land on the line.
func formatIgnoreRule(ig IgnoredDeliverable) string {
	switch {
	case ig.RuleSource != "" && ig.RuleLine > 0:
		return fmt.Sprintf("%s:%d:%s", ig.RuleSource, ig.RuleLine, ig.Pattern)
	case ig.RuleSource != "":
		return fmt.Sprintf("%s:%s", ig.RuleSource, ig.Pattern)
	default:
		return ig.Pattern
	}
}

// ErrDeliverableNotObservable is the typed error a refusal becomes, so the
// existing launch-failure machinery (classifyWorkerLaunchFailure,
// recordWorkerLaunchFailure) carries it without a second failure path.
type ErrDeliverableNotObservable struct {
	Ignored []IgnoredDeliverable
	Detail  string
}

func (e *ErrDeliverableNotObservable) Error() string { return e.Detail }

// preflightDeliverableObservability runs the check for one dispatch attempt.
//
// Every read failure degrades to "proceed". The artifact is read from the run's
// own plan step, which is the same durable row the prompt was built from, so
// the paths checked here are the paths the worker is about to be asked for.
func (c *Coordinator) preflightDeliverableObservability(ctx stdctx.Context, run domain.WorkflowRun) error {
	if c.deliverableIgnores == nil {
		return nil
	}
	repoPath := c.projectPathFor(ctx, run.ProjectID)
	if strings.TrimSpace(repoPath) == "" {
		return nil
	}
	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: deliverable observability could not read the plan artifact; dispatching anyway",
				"run", run.ID, "err", err)
		}
		return nil
	}
	if artifact.WriteIntent.ReadOnly() {
		return nil
	}
	required := RequiredDeliverables(artifact)
	if len(required) == 0 {
		return nil
	}
	paths := make([]string, 0, len(required))
	for _, d := range required {
		paths = append(paths, d.Path)
	}
	ignored, err := c.deliverableIgnores.IgnoredPaths(ctx, repoPath, paths)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: deliverable observability could not be checked; dispatching anyway",
				"run", run.ID, "repo", repoPath, "err", err)
		}
		return nil
	}
	verdict := evaluateDeliverableObservability(artifact.WriteIntent, required, ignored)
	if verdict.Ready {
		return nil
	}
	if c.log != nil {
		c.log.Warn("workflow: refusing a dispatch whose required deliverable git cannot see",
			"run", run.ID, "repo", repoPath, "detail", verdict.Detail)
	}
	return &ErrDeliverableNotObservable{Ignored: verdict.Ignored, Detail: verdict.Detail}
}

// classifyDeliverableRefusal maps the refusal onto the launch-recovery
// vocabulary: never retryable (waiting does not change a .gitignore) and
// carrying its own precise attention reason.
func classifyDeliverableRefusal(err error) (workerLaunchClassification, bool) {
	var d *ErrDeliverableNotObservable
	if !errors.As(err, &d) {
		return workerLaunchClassification{}, false
	}
	return workerLaunchClassification{
		Class:     WorkflowErrorDeliverableNotObservable,
		Certainty: CertaintyActual,
		Retryable: false,
		Reason:    ReasonDeliverableNotObservable,
	}, true
}
