package workflow

import (
	stdctx "context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// task_knowledge_sources.go — where a finished task's Decisions and Risks come
// from (P2-C §15, first limitation).
//
// P2-C shipped the whole lifecycle for decisions and risks — supersession,
// resolution, conflict, sharing scope, promotion authority — and then supplied
// neither, because "no durable row currently holds them". That was true of the
// *contents* of a reviewer's opinion and of a planner's reasoning, and it was
// not true of everything AO records around them. This file closes the gap from
// the sources that do exist, and from nothing else.
//
// Three rules govern every derivation below, and each one costs coverage on
// purpose:
//
//   - **Only durable rows.** A transcript, a pane capture, a prompt and a
//     review body are not sources here. A review body in particular is prose:
//     CountReviewFindings' own doc comment says AO has never had a structured
//     finding type, and parsing one out of markdown would be the invented
//     provenance this subsystem refuses everywhere else.
//   - **Only authoritative rows.** A superseded review, a baseline QA finding,
//     an out-of-scope finding, a finding backed only by the agent's own report,
//     an unanswered question — none of them are facts about the project, and
//     none of them become active risks.
//   - **Nothing is invented.** A field AO cannot prove is left empty. A risk
//     that names no file is anchored by the task's own changed paths (which is
//     what knowledgeEvidence already does) rather than by a guessed one.
//
// The lifecycle is P2-C's, unchanged: these functions only decide what to hand
// RecordTaskOutcome, and everything after that — subject identity,
// supersession, resolution, conflict, share, promotion, compaction — happens in
// internal/projectmemory exactly as it did before.

// QAGate is the durable Post-Run QA state this package reads (migration 0126).
//
// It is the one structured finding type AO has: each finding carries its own
// attribution, scope, verification and severity, so "is this finding this
// task's problem, and does anyone need to act on it" is answered by the row
// rather than by a reader's judgement. Optional, on the same convention as
// every other dependency here: a nil QAGate contributes no risks and is never
// an error.
//
// One caveat worth stating rather than discovering: the gate's state model is
// durable and wired here, but nothing in the daemon RUNS the checks yet —
// internal/postrunqa says so itself ("no check runners, no repair dispatch, no
// scheduling"). So this source contributes nothing in production until a runner
// lands, and the two review-derived sources below carry the risks meanwhile.
// It is wired now because the wiring is the part that must be right when the
// runner arrives; a finding that reached the gate and never reached memory
// would be exactly this gap reopened.
type QAGate interface {
	LatestQARunForSubject(ctx stdctx.Context, kind postrunqa.SubjectKind, subjectID string) (postrunqa.QARun, bool, error)
}

// ReviewThreads reads a pull request's normalized review threads (migration
// 0004), which the SCM observer keeps up to date.
//
// A thread row is the only reviewer finding AO holds that names a FILE, and
// its resolved flag is the provider's own answer to "was this addressed" —
// which is why it is the source that can both raise a risk and close one.
// Optional; nil contributes nothing.
type ReviewThreads interface {
	ListPRReviewThreads(ctx stdctx.Context, prURL string) ([]domain.PullRequestReviewThread, error)
}

// TaskDecisionFact is one durable decision a finished task carries into project
// memory. It is workflow's vocabulary for projectmemory.TaskDecision; the
// translation happens once, in the wfmemory adapter.
type TaskDecisionFact struct {
	// Statement is the decision itself.
	Statement string
	// Rationale is bounded prose saying why, copied from the durable row that
	// recorded it. Never composed and never summarised.
	Rationale string
	// Topic is what the decision is ABOUT, and is what supersession turns on:
	// re-deciding the same topic retires the previous answer instead of piling
	// a second one beside it.
	Topic string
}

// TaskRiskFact is one durable risk a finished task carries into project memory.
type TaskRiskFact struct {
	// Statement is the risk, in one line.
	Statement string
	// Kind separates a risk from a deliberately deferred piece of work.
	Kind domain.KnowledgeKind
	// Topic is the risk's stable identity, and is what a later task names to
	// resolve it.
	Topic string
	// Evidence are repo-relative paths the risk is PROVEN to concern. Empty
	// where nothing proves one, in which case memory anchors the risk to the
	// task's own changed paths.
	Evidence []string
}

// Bounds on one statement, so a durable row with a long text cannot turn into
// an unbounded fact. The rationale bound is memory's own
// (projectmemory.MaxDecisionRationale); this one only keeps the STATEMENT — the
// line a later task reads in its pack — short enough to be one line.
const maxKnowledgeStatement = 300

// durableTaskDecisions collects the decisions AO can prove this task made.
//
// Ordered strongest-authority first, because the per-outcome cap is applied by
// truncation: a human-approved amendment to the plan outranks an answered
// question, and if only some of them fit, the ones that fit should be the ones
// somebody signed.
func (c *Coordinator) durableTaskDecisions(ctx stdctx.Context, run domain.WorkflowRun) []TaskDecisionFact {
	out := c.criterionAmendmentDecisions(ctx, run)
	out = append(out, c.answeredQuestionDecisions(ctx, run)...)
	if len(out) > projectmemory.MaxTaskDecisions {
		out = out[:projectmemory.MaxTaskDecisions]
	}
	return out
}

// criterionAmendmentDecisions reads the plan's own amendment ledger (migration
// 0132).
//
// This is the most authoritative decision AO records anywhere: the schema
// refuses an amendment without a human approver, a reason and at least one
// piece of evidence, and the ledger is append-only. "The acceptance criterion
// for this work changed, and here is why" is a decision later work must respect
// by any definition.
//
// Newest first, because two amendments to the SAME criterion are the same
// subject decided twice, and memory keeps the first statement it sees for a
// subject within one outcome. Handing it the newest is what makes the current
// answer current.
func (c *Coordinator) criterionAmendmentDecisions(ctx stdctx.Context, run domain.WorkflowRun) []TaskDecisionFact {
	if c.planStore == nil || run.PlannedTaskID == nil || strings.TrimSpace(*run.PlannedTaskID) == "" {
		return nil
	}
	taskID := strings.TrimSpace(*run.PlannedTaskID)
	amendments, err := c.planStore.ListWorkflowTaskCriterionAmendments(ctx, knowledgeRunIDFor(run))
	if err != nil {
		if c.log != nil {
			c.log.Warn("project memory: could not read the criterion amendments of a finished task",
				"run", run.ID, "task", taskID, "err", err)
		}
		return nil
	}
	mine := make([]domain.WorkflowTaskCriterionAmendment, 0, len(amendments))
	for _, a := range amendments {
		if strings.TrimSpace(a.TaskID) == taskID && strings.TrimSpace(a.Reason) != "" {
			mine = append(mine, a)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].CreatedAt.After(mine[j].CreatedAt) })

	out := make([]TaskDecisionFact, 0, len(mine))
	for _, a := range mine {
		original := strings.TrimSpace(a.OriginalCriterion)
		var statement string
		switch a.Disposition {
		case domain.WorkflowTaskCriterionObsolete:
			statement = "the acceptance criterion " + quoted(original) + " no longer applies to this work"
		case domain.WorkflowTaskCriterionAmended:
			amended := strings.TrimSpace(a.AmendedCriterion)
			if amended == "" {
				continue
			}
			statement = "the acceptance criterion " + quoted(original) + " now reads: " + amended
		default:
			// A disposition this build does not know is not a decision it may
			// state. The row stays durable and readable where it is.
			continue
		}
		rationale := strings.TrimSpace(a.Reason)
		if approver := strings.TrimSpace(a.ApprovedBy); approver != "" {
			rationale += " (approved by " + approver + ")"
		}
		out = append(out, TaskDecisionFact{
			Statement: boundedStatement(statement),
			Rationale: rationale,
			// The criterion's TEXT, not its index: migration 0132 says outright
			// that an index stops identifying anything as soon as an earlier
			// criterion is removed.
			Topic: "acceptance-criterion:" + taskID + ":" + textIdentity(original),
		})
	}
	return out
}

// answeredQuestionDecisions reads the durable question ledger (Checkpoint 8K).
//
// An answered question is a decision by construction: the work was blocked on
// it, an answer was recorded with its source, and that answer was delivered to
// the agent and changed what it did. The QUESTION ROW is the source here, never
// the pane it was captured from — the row is classified, fingerprinted and
// durable, and nothing below re-reads or re-interprets a transcript.
//
// Authority is read off the row and never assumed:
//
//   - a human answer is authoritative outright;
//   - a policy answer is deterministic AO policy, and is authoritative;
//   - a RESOLVER answer is one agent's opinion, so it counts only when its own
//     durable resolution row completed and did not ask for a human.
func (c *Coordinator) answeredQuestionDecisions(ctx stdctx.Context, run domain.WorkflowRun) []TaskDecisionFact {
	if c.questionsStore == nil {
		return nil
	}
	questions, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, run.ID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("project memory: could not read the answered questions of a finished task",
				"run", run.ID, "err", err)
		}
		return nil
	}
	out := make([]TaskDecisionFact, 0, len(questions))
	for _, q := range questions {
		if q.State != domain.QuestionStateAnswered || q.AnswerSource == nil {
			continue
		}
		question := strings.TrimSpace(q.QuestionText)
		answer := strings.TrimSpace(q.AnswerText)
		if question == "" || answer == "" {
			continue
		}
		rationale, ok := c.questionAnswerAuthority(ctx, q)
		if !ok {
			continue
		}
		topic := strings.TrimSpace(q.Fingerprint)
		if topic == "" {
			topic = string(q.ID)
		}
		out = append(out, TaskDecisionFact{
			Statement: boundedStatement(question + " — answered: " + answer),
			Rationale: rationale,
			Topic:     "question:" + topic,
		})
		if len(out) >= projectmemory.MaxTaskDecisions {
			break
		}
	}
	return out
}

// questionAnswerAuthority reports whether an answered question may become a
// decision, and returns the durable rationale that goes with it.
//
// The rationale is the resolver's own recorded reason summary when there is
// one, and otherwise names the answer's source — never a sentence composed
// about why the answer is right.
func (c *Coordinator) questionAnswerAuthority(
	ctx stdctx.Context, q domain.WorkflowQuestion,
) (string, bool) {
	source := *q.AnswerSource
	rationale := "answered by " + string(source)
	if ref := strings.TrimSpace(q.AnswerReference); ref != "" {
		rationale += ", reference " + ref
	}
	switch source {
	case domain.AnswerSourceHuman, domain.AnswerSourcePolicy:
		return rationale, true
	case domain.AnswerSourceResolver:
		res, ok, err := c.questionsStore.GetCurrentResolutionForQuestion(ctx, string(q.ID))
		if err != nil || !ok {
			// The answer says a resolver produced it and AO cannot read the
			// attempt that did. That is exactly the case for recording
			// nothing: an unverifiable authority is not an authority.
			return "", false
		}
		if res.Status != domain.ResolutionStatusComplete || res.RequiresHuman {
			return "", false
		}
		if summary := strings.TrimSpace(res.ReasonSummary); summary != "" {
			return summary, true
		}
		return rationale, true
	default:
		return "", false
	}
}

// durableTaskRisks collects the risks AO can prove this task leaves behind, and
// the risks it can prove are closed.
//
// The second return value feeds TaskOutcome.ResolvesRisks: a finding that a
// durable source now reports as fixed does not merely stop being emitted (which
// would leave an earlier task's risk active forever), it RESOLVES the risk that
// was recorded for it, by topic. That is the whole difference between "AO
// stopped mentioning it" and "AO knows it was dealt with".
func (c *Coordinator) durableTaskRisks(
	ctx stdctx.Context, run domain.WorkflowRun,
) ([]TaskRiskFact, []string) {
	reviews := c.taskReviewRuns(ctx, run)

	risks, resolved := c.qaGateRisks(ctx, run)
	threadRisks, threadResolved := c.reviewThreadRisks(ctx, reviews)
	verdictRisks, verdictResolved := c.reviewVerdictRisks(reviews)
	risks = append(append(risks, threadRisks...), verdictRisks...)
	resolved = append(append(resolved, threadResolved...), verdictResolved...)

	// One topic is one risk. A QA finding and a review thread that happen to
	// describe the same breakage are still two findings from two sources, so
	// only exact topic repeats are collapsed.
	risks = dedupeRisksByTopic(risks)
	if len(risks) > projectmemory.MaxTaskRisks {
		risks = risks[:projectmemory.MaxTaskRisks]
	}
	return risks, dedupeStrings(resolved)
}

// qaGateRisks turns the Post-Run QA gate's structured findings into risks.
//
// Finding.Blocking() is the filter, and it is deliberately the gate's OWN
// predicate rather than a second opinion written here: a baseline finding was
// already true before this task ran, an out-of-scope finding belongs to
// something this execution never owned, and a report-only finding is the
// agent's prose with nothing structured behind it. None of the three is this
// task's risk, and re-deciding that here would be two answers to one question.
//
// A gate pass that ended CLEAN resolves what it found instead of raising it.
// That is the honest reading of a terminal clean verdict: the findings are
// still listed as the evidence of what was checked, and the subject was
// cleared.
func (c *Coordinator) qaGateRisks(
	ctx stdctx.Context, run domain.WorkflowRun,
) ([]TaskRiskFact, []string) {
	if c.qaGate == nil {
		return nil, nil
	}
	kind, subject := postrunqa.SubjectTask, taskRefFor(run)
	if run.PlannedTaskID == nil || strings.TrimSpace(*run.PlannedTaskID) == "" {
		kind, subject = postrunqa.SubjectWorkflow, run.ID
	}
	qa, ok, err := c.qaGate.LatestQARunForSubject(ctx, kind, subject)
	if err != nil {
		if c.log != nil {
			c.log.Warn("project memory: could not read the QA gate of a finished task",
				"run", run.ID, "subject", subject, "err", err)
		}
		return nil, nil
	}
	if !ok {
		return nil, nil
	}
	cleared := qa.Phase.Terminal() && qa.Result == postrunqa.ResultClean

	var risks []TaskRiskFact
	var resolved []string
	for _, f := range qa.Findings {
		if !f.Blocking() {
			continue
		}
		topic := "qa-finding:" + qaFindingIdentity(f)
		if cleared {
			resolved = append(resolved, topic)
			continue
		}
		statement := strings.TrimSpace(f.Signal)
		if statement == "" {
			continue
		}
		if src := strings.TrimSpace(f.Source); src != "" {
			statement += " (reported by " + src + ")"
		}
		risks = append(risks, TaskRiskFact{
			Statement: boundedStatement(statement),
			Kind:      domain.KnowledgeKindRisk,
			Topic:     topic,
		})
	}
	return risks, resolved
}

// qaFindingIdentity is the finding's own stable identity.
//
// Signature is exactly that where the check set one — it is what baseline
// attribution already matches on across runs, and it is documented as carrying
// no timestamps or counts. Where it is empty, subject+signal is the same pair
// the signature would have been built from.
func qaFindingIdentity(f postrunqa.Finding) string {
	if sig := strings.TrimSpace(f.Signature); sig != "" {
		return sig
	}
	return strings.TrimSpace(f.Subject) + "|" + strings.TrimSpace(f.Signal)
}

// reviewThreadRisks turns a pull request's unresolved review threads into risks
// that name a file.
//
// The thread row carries no comment text and this does not invent one: what it
// can say is that a reviewer opened a thread on a path and line that nobody has
// resolved, which is a real thing for the next task in that file to know. The
// resolved flag closes the risk on the same identity, so a later pass over the
// same PR reports the fix rather than merely falling silent.
func (c *Coordinator) reviewThreadRisks(
	ctx stdctx.Context, reviews []domain.ReviewRun,
) ([]TaskRiskFact, []string) {
	if c.reviewThreads == nil {
		return nil, nil
	}
	var risks []TaskRiskFact
	var resolved []string
	for _, prURL := range reviewPRURLs(reviews) {
		threads, err := c.reviewThreads.ListPRReviewThreads(ctx, prURL)
		if err != nil {
			if c.log != nil {
				c.log.Warn("project memory: could not read the review threads of a finished task",
					"pr", prURL, "err", err)
			}
			continue
		}
		for _, t := range threads {
			id := strings.TrimSpace(t.ThreadID)
			if id == "" {
				continue
			}
			topic := "review-thread:" + id
			if t.Resolved {
				resolved = append(resolved, topic)
				continue
			}
			risks = append(risks, TaskRiskFact{
				Statement: boundedStatement("a reviewer thread on " + threadLocation(t) +
					" is still unresolved on " + prURL),
				Kind:     domain.KnowledgeKindRisk,
				Topic:    topic,
				Evidence: threadEvidence(t),
			})
		}
	}
	return risks, resolved
}

// reviewVerdictRisks turns the review runs' own verdict columns into risks.
//
// This is the review-level fact AO has always held durably: a pass that
// REQUESTED CHANGES against a commit. The risk it raises names the review, not
// its prose — the findings themselves live in the review body, which is not a
// structured source and is not read here.
//
// Two exclusions, both from durable columns rather than from judgement. A run
// that was SUPERSEDED no longer speaks for anything (domain.ReviewRun says so
// itself: once a replacement took authority, the old reviewer's answer is
// evidence and never a decision). And a changes_requested pass that a LATER
// pass on the same pull request approved has been answered — that resolves the
// risk instead of raising it, which is the ordinary end of a fix cycle.
func (c *Coordinator) reviewVerdictRisks(reviews []domain.ReviewRun) ([]TaskRiskFact, []string) {
	approvedAt := map[string]int64{}
	for _, r := range reviews {
		if strings.TrimSpace(r.SupersededBy) != "" {
			continue
		}
		if r.EffectiveVerdict() != domain.VerdictApproved {
			continue
		}
		if at := r.CreatedAt.UnixNano(); at > approvedAt[r.PRURL] {
			approvedAt[r.PRURL] = at
		}
	}
	var risks []TaskRiskFact
	var resolved []string
	for _, r := range reviews {
		if strings.TrimSpace(r.SupersededBy) != "" {
			continue
		}
		if r.EffectiveVerdict() != domain.VerdictChangesRequested {
			continue
		}
		topic := "review-changes-requested:" + r.ID
		if at, ok := approvedAt[r.PRURL]; ok && at > r.CreatedAt.UnixNano() {
			resolved = append(resolved, topic)
			continue
		}
		where := strings.TrimSpace(r.PRURL)
		if where == "" {
			where = "this work"
		}
		risks = append(risks, TaskRiskFact{
			Statement: boundedStatement(fmt.Sprintf(
				"reviewer %s requested changes on %s at %s, and no later review approved that work (review run %s)",
				r.Harness, where, shortCommit(r.TargetSHA), r.ID)),
			Kind:  domain.KnowledgeKindRisk,
			Topic: topic,
		})
	}
	return risks, resolved
}

// taskReviewRuns lists every review pass over this task's work.
//
// The worker session is the key, exactly as it is for the fix cycle and for
// fresh-review reconciliation: the review rows hang off the session that
// produced the work, and reading them through any other identity would be a
// second answer to "which reviews judged this task".
func (c *Coordinator) taskReviewRuns(ctx stdctx.Context, run domain.WorkflowRun) []domain.ReviewRun {
	if c.reviewRuns == nil || c.store == nil {
		return nil
	}
	session, ok := c.taskWorkerSession(ctx, run)
	if !ok {
		return nil
	}
	reviews, err := c.reviewRuns.ListReviewRunsBySession(ctx, session)
	if err != nil {
		if c.log != nil {
			c.log.Warn("project memory: could not read the reviews of a finished task",
				"run", run.ID, "session", string(session), "err", err)
		}
		return nil
	}
	return reviews
}

// taskWorkerSession resolves the session that did this run's work.
func (c *Coordinator) taskWorkerSession(ctx stdctx.Context, run domain.WorkflowRun) (domain.SessionID, bool) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return "", false
	}
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepWork {
			continue
		}
		if step.SessionID != nil && strings.TrimSpace(*step.SessionID) != "" {
			return domain.SessionID(strings.TrimSpace(*step.SessionID)), true
		}
		// The step row carries no session for a dispatch whose ownership was
		// only ever proven on a checkpoint. That checkpoint is the same one
		// fresh-review reconciliation reads, for the same reason.
		cp, ok, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID)
		if cerr == nil && ok && cp.SessionID != nil && strings.TrimSpace(*cp.SessionID) != "" {
			return domain.SessionID(strings.TrimSpace(*cp.SessionID)), true
		}
	}
	return "", false
}

// reviewPRURLs is the bounded set of pull requests this task was reviewed on.
func reviewPRURLs(reviews []domain.ReviewRun) []string {
	const maxPRs = 4
	seen := map[string]struct{}{}
	out := make([]string, 0, maxPRs)
	for _, r := range reviews {
		url := strings.TrimSpace(r.PRURL)
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		out = append(out, url)
		if len(out) >= maxPRs {
			break
		}
	}
	return out
}

// threadLocation renders where a review thread sits, without inventing a
// position the row does not hold.
func threadLocation(t domain.PullRequestReviewThread) string {
	path := strings.TrimSpace(t.Path)
	switch {
	case path == "":
		return "the pull request"
	case t.Line > 0:
		return fmt.Sprintf("%s:%d", path, t.Line)
	default:
		return path
	}
}

// threadEvidence is the thread's path, when it has one. A thread with no path
// is about the pull request rather than about a file, and giving it one would
// anchor a later task's context to a file no reviewer named.
func threadEvidence(t domain.PullRequestReviewThread) []string {
	if path := strings.TrimSpace(t.Path); path != "" {
		return []string{path}
	}
	return nil
}

// dedupeRisksByTopic keeps the first risk stated about each topic.
func dedupeRisksByTopic(in []TaskRiskFact) []TaskRiskFact {
	seen := make(map[string]struct{}, len(in))
	out := make([]TaskRiskFact, 0, len(in))
	for _, r := range in {
		if _, dup := seen[r.Topic]; dup {
			continue
		}
		seen[r.Topic] = struct{}{}
		out = append(out, r)
	}
	return out
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// boundedStatement caps one fact's statement and collapses its whitespace, so a
// durable row with an unusually long text becomes a short fact rather than an
// unbounded one.
func boundedStatement(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxKnowledgeStatement {
		return s
	}
	// Cut on a rune boundary and count the ellipsis against the bound, so the
	// result is valid UTF-8 and the bound is a real one.
	cut := s[:maxKnowledgeStatement-3]
	for cut != "" && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "..."
}

// quoted renders a criterion inline without letting its own quotes run away.
func quoted(s string) string {
	return "\"" + strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "\"", "'") + "\""
}

// textIdentity is the stable short identity of a piece of text, used where a
// topic has to be derived from text that is itself too long to be a topic. It
// hashes the text's words rather than its bytes, so re-wrapping a criterion
// does not fork it into a second subject that then supersedes the first
// forever.
func textIdentity(s string) string {
	return shortDigest(FindingsDigest(strings.Join(strings.Fields(s), " ")))
}
