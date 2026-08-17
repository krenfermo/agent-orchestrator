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

const (
	MasterPlanVersion     = "v1"
	MaxPlanSteps          = 12
	PlannerContextVersion = "v1"
)

type MasterPlan struct {
	Version   string        `json:"version"`
	Objective string        `json:"objective"`
	Summary   string        `json:"summary"`
	Steps     []PlannedStep `json:"steps"`
}

type PlannedStep struct {
	ID                 string           `json:"id"`
	Title              string           `json:"title"`
	Description        string           `json:"description"`
	Dependencies       []string         `json:"dependencies"`
	AcceptanceCriteria []string         `json:"acceptanceCriteria"`
	Verify             VerificationPlan `json:"verify"`
}

type PlanValidation struct {
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors"`
}

type PlannerContext struct {
	Version     string            `json:"version"`
	ProjectID   string            `json:"projectId"`
	ProjectPath string            `json:"projectPath"`
	Branch      string            `json:"branch"`
	HeadSHA     string            `json:"headSha"`
	Dirty       bool              `json:"dirty"`
	Documents   []PlannerDocument `json:"documents"`
}

type PlannerDocument struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}
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
type PlannerResponse struct {
	Plan     MasterPlan
	Provider string
	Model    string
}

type Planner interface {
	Generate(ctx stdctx.Context, request PlannerRequest) (PlannerResponse, error)
}
type PlannerDescriptor interface {
	Descriptor() (provider, model string)
}
type PlannerContextBuilder interface {
	Build(ctx stdctx.Context, project domain.ProjectRecord) (PlannerContext, error)
}

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
