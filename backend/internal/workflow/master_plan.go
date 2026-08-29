package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// MasterPlanVersion is the plan-format version stamped into every plan AO
// generates; MaxPlanSteps is the default ceiling on how many steps one plan may
// contain, and PlannerContextVersion versions the context manifest a plan was
// generated from, so a stored manifest stays interpretable after the shape
// changes.
const (
	MasterPlanVersion     = "v1"
	MaxPlanSteps          = 12
	PlannerContextVersion = "v1"
)

// MasterPlan is a planner's whole answer to one objective: the ordered steps
// it proposes, and the summary it proposes them under.
type MasterPlan struct {
	Version   string        `json:"version"`
	Objective string        `json:"objective"`
	Summary   string        `json:"summary"`
	Steps     []PlannedStep `json:"steps"`
}

// PlannedStep is one step of a MasterPlan as the planner wrote it -- what to
// do, how to tell it is done, and the optional declarations (write intent,
// scope, waivers) that let AO schedule it against its siblings.
//
// Every optional field is omitempty and nil when undeclared, so a plan that
// says nothing about them serializes -- and therefore hashes -- exactly as it
// did before they existed.
type PlannedStep struct {
	ID                 string           `json:"id"`
	Title              string           `json:"title"`
	Description        string           `json:"description"`
	Dependencies       []string         `json:"dependencies"`
	AcceptanceCriteria []string         `json:"acceptanceCriteria"`
	Verify             VerificationPlan `json:"verify"`
	// WriteIntent is the step's OPTIONAL declaration of whether it is expected
	// to change the workspace at all (domain.WorkflowWriteIntent:
	// "mutating" or "read_only").
	//
	// It is the durable semantic that lets AO tell a read-only verification /
	// inspection step, whose accepted outcome is an UNCHANGED workspace, apart
	// from an implementation step that produced nothing. Absent, it is
	// Unspecified, which is treated exactly as "mutating" -- so a plan that
	// says nothing about it behaves exactly as it did before this field
	// existed, and serializes (and therefore hashes) exactly the same.
	WriteIntent domain.WorkflowWriteIntent `json:"writeIntent,omitempty"`
	// Files and Packages are the step's OPTIONAL explicit scope declaration:
	// the files it will touch when the planner knows them, and the
	// packages/components it expects to touch. They feed the task-graph
	// classifier, which trusts them over anything it infers from prose.
	//
	// Both are omitempty and are left nil when the planner declares nothing,
	// so a plan that says nothing about scope serializes -- and therefore
	// hashes -- exactly as it did before these fields existed.
	Files    []string `json:"files,omitempty"`
	Packages []string `json:"packages,omitempty"`
	// SafeWriteOverlaps are this step's OPTIONAL waivers: overlaps with a
	// named sibling step that are safe to share despite both steps writing
	// there. Absent a waiver, an overlapping write set is classified as a
	// probable write conflict -- that default is deliberate, and a waiver is
	// the only thing that clears it.
	//
	// Also omitempty and nil when undeclared, so a plan that declares none
	// serializes, and hashes, exactly as it did before this field existed.
	SafeWriteOverlaps []PlannedSafeOverlap `json:"safeWriteOverlaps,omitempty"`
}

// PlannedSafeOverlap is a plan-level declaration that a write-set overlap with
// one other step is not a conflict. It is the plan-text form of
// domain.WorkflowTaskSafeOverlap, in plan step ids rather than task ids.
type PlannedSafeOverlap struct {
	// With is the other step's id. Required: a waiver that names no
	// counterpart would be a blanket exemption, which is exactly what the
	// conflict default exists to prevent.
	With string `json:"with"`
	// Paths narrows the waiver to specific paths; a directory waives
	// everything under it. Empty waives the whole overlap with that step.
	Paths []string `json:"paths,omitempty"`
	// Reason is why sharing is safe. Required, and stored with the
	// classification so the waived decision explains itself later.
	Reason string `json:"reason"`
}

// PlanValidation is the structural verdict on a generated plan. Errors is
// non-empty exactly when Valid is false, and says what a person would have to
// change for the plan to be executable.
type PlanValidation struct {
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors"`
}

// PlannerContext is the repository state a plan was generated against, pinned
// so a stored plan can be read back against the world it was written for.
// Version is PlannerContextVersion.
type PlannerContext struct {
	Version     string            `json:"version"`
	ProjectID   string            `json:"projectId"`
	ProjectPath string            `json:"projectPath"`
	Branch      string            `json:"branch"`
	HeadSHA     string            `json:"headSha"`
	Dirty       bool              `json:"dirty"`
	Documents   []PlannerDocument `json:"documents"`
}

// PlannerDocument is one planning document included in a PlannerContext, with
// the digest of the content that was actually sent.
type PlannerDocument struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// PlannerRequest is one invocation of a Planner: the objective, the project,
// the pinned context, and the step ceiling to plan within.
type PlannerRequest struct {
	Objective string
	Project   domain.ProjectRecord
	Context   PlannerContext
	MaxSteps  int
	// RuntimeEnv overrides subprocess env for the planner process
	// (Checkpoint 8P-B.1) -- the workflow run owner's isolated
	// runtime-home, resolved once by Coordinator.resolveRuntimeEnv. Nil
	// preserves pre-8P-B.1 behavior exactly (the planner subprocess
	// inherited the daemon's real, unmodified environment before this
	// checkpoint).
	RuntimeEnv map[string]string
}

// PlannerResponse is a Planner's output together with the provider and model
// that produced it, so the plan can be attributed after the fact.
type PlannerResponse struct {
	Plan     MasterPlan
	Provider string
	Model    string
}

// Planner turns an objective into a MasterPlan. It returns an error rather
// than a partial plan: a plan AO cannot fully parse is not one it may execute.
type Planner interface {
	Generate(ctx stdctx.Context, request PlannerRequest) (PlannerResponse, error)
}

// PlannerDescriptor is the optional capability a Planner may also implement to
// name the provider and model it will use, without being invoked.
type PlannerDescriptor interface {
	Descriptor() (provider, model string)
}

// PlannerContextBuilder assembles the PlannerContext a plan is generated from.
type PlannerContextBuilder interface {
	Build(ctx stdctx.Context, project domain.ProjectRecord) (PlannerContext, error)
}

// NormalizeAndValidatePlan puts a planner's raw plan into canonical form and
// checks it is executable. It returns the normalized plan, the verdict, and the
// plan hash -- the hash being what a later approval pins, so an approval can
// prove which plan it approved.
func NormalizeAndValidatePlan(plan MasterPlan, objective string, maxSteps int) (MasterPlan, PlanValidation, string) {
	if maxSteps <= 0 {
		maxSteps = MaxPlanSteps
	}
	plan.Version = strings.TrimSpace(plan.Version)
	plan.Objective = strings.TrimSpace(plan.Objective)
	modelObjectiveEmpty := plan.Objective == ""
	if !modelObjectiveEmpty {
		// The model may harmlessly paraphrase or translate the objective. The
		// workflow's durable objective is authoritative in the accepted plan.
		plan.Objective = strings.TrimSpace(objective)
	}
	plan.Summary = strings.TrimSpace(plan.Summary)
	validation := PlanValidation{Valid: true, Errors: []string{}}
	add := func(s string) { validation.Valid = false; validation.Errors = append(validation.Errors, s) }
	if plan.Version != MasterPlanVersion {
		add("unsupported version")
	}
	if modelObjectiveEmpty {
		add("objective is required")
	}
	if len(plan.Steps) == 0 || len(plan.Steps) > maxSteps {
		add(fmt.Sprintf("steps must contain 1..%d items", maxSteps))
	}
	ids := map[string]bool{}
	for i := range plan.Steps {
		s := &plan.Steps[i]
		s.ID, s.Title, s.Description = strings.TrimSpace(s.ID), strings.TrimSpace(s.Title), strings.TrimSpace(s.Description)
		if s.Dependencies == nil {
			s.Dependencies = []string{}
		}
		sort.Strings(s.Dependencies)
		if s.AcceptanceCriteria == nil {
			s.AcceptanceCriteria = []string{}
		}
		// Normalized only when declared: an empty declaration stays nil so it
		// stays out of the canonical JSON the plan hash is taken over.
		if s.Files = normalizeStrings(s.Files); len(s.Files) == 0 {
			s.Files = nil
		}
		if s.Packages = normalizeStrings(s.Packages); len(s.Packages) == 0 {
			s.Packages = nil
		}
		for j := range s.SafeWriteOverlaps {
			w := &s.SafeWriteOverlaps[j]
			w.With, w.Reason = strings.TrimSpace(w.With), strings.TrimSpace(w.Reason)
			if w.Paths = normalizeStrings(w.Paths); len(w.Paths) == 0 {
				w.Paths = nil
			}
		}
		if len(s.SafeWriteOverlaps) == 0 {
			s.SafeWriteOverlaps = nil
		}
		// An intent AO cannot read is not silently downgraded to "mutating":
		// a planner that meant read_only and mistyped it would otherwise have
		// its declaration disappear, and the plan would look like it never
		// made one. Unreadable is a plan error; ABSENT is fine and stays
		// Unspecified.
		if raw := strings.TrimSpace(string(s.WriteIntent)); raw != "" {
			if intent := domain.NormalizeWorkflowWriteIntent(raw); intent == domain.WorkflowWriteIntentUnspecified {
				add(fmt.Sprintf("step %q has an unrecognized writeIntent %q", s.ID, raw))
			} else {
				s.WriteIntent = intent
			}
		}
		if s.Verify.Commands == nil {
			s.Verify.Commands = []VerificationCommandCheck{}
		}
		if s.Verify.Files == nil {
			s.Verify.Files = []VerificationFileCheck{}
		}
		if s.ID == "" || ids[s.ID] {
			add(fmt.Sprintf("step %d has an empty or duplicate id", i+1))
		}
		ids[s.ID] = true
		if s.Title == "" || s.Description == "" {
			add(fmt.Sprintf("step %q requires title and description", s.ID))
		}
		if len(s.AcceptanceCriteria) == 0 {
			add(fmt.Sprintf("step %q requires acceptance criteria", s.ID))
		}
		if err := s.Verify.validate(); err != nil {
			add(fmt.Sprintf("step %q: %v", s.ID, err))
		}
	}
	for _, s := range plan.Steps {
		for _, dep := range s.Dependencies {
			if !ids[dep] {
				add(fmt.Sprintf("step %q has unknown dependency %q", s.ID, dep))
			}
		}
		// A waiver is the one thing that can turn a probable write conflict
		// into independent work, so a malformed one is a plan error rather
		// than something to quietly drop: a waiver naming a step that does not
		// exist, or naming itself, or stating no reason, is never what the
		// planner meant, and letting it through would leave a real overlap
		// looking reviewed when nobody reviewed it.
		for _, w := range s.SafeWriteOverlaps {
			switch {
			case w.With == "":
				add(fmt.Sprintf("step %q has a safe write overlap with no target step", s.ID))
			case w.With == s.ID:
				add(fmt.Sprintf("step %q declares a safe write overlap with itself", s.ID))
			case !ids[w.With]:
				add(fmt.Sprintf("step %q has a safe write overlap with unknown step %q", s.ID, w.With))
			}
			if w.Reason == "" {
				add(fmt.Sprintf("step %q has a safe write overlap without a reason", s.ID))
			}
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	byID := map[string]PlannedStep{}
	for _, s := range plan.Steps {
		byID[s.ID] = s
	}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, d := range byID[id].Dependencies {
			if visit(d) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range byID {
		if visit(id) {
			add("dependency graph contains a cycle")
			break
		}
	}
	canonical, _ := json.Marshal(plan)
	sum := sha256.Sum256(canonical)
	return plan, validation, hex.EncodeToString(sum[:])
}
