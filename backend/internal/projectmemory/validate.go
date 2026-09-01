package projectmemory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// validate.go — the authority pass (P2-D §2, §19, §21, §23).
//
// Drift detection (drift.go) answers "do this fact's SOURCES still look the
// way they did". Validation answers the other half: "is the thing that made
// this fact the project's knowledge still something AO holds a record of".
//
// The two are separate passes over separate columns because they fail for
// separate reasons and are repaired by separate actions. A file that changed
// is repaired by re-deriving; a promotion that turns out never to have
// happened is repaired by finding the integration, or by never promoting the
// fact at all. Running them as one pass would give an operator one verdict for
// two problems.
//
// Three vocabulary distinctions this file exists to keep straight (§19):
//
//	INVALIDATE  the licence is gone. The fact is withheld and stays withheld
//	            until something re-establishes it. This pass only ever does
//	            this.
//	REVALIDATE  the proof still checks out; refresh the record of it WITHOUT
//	            re-deriving content. Cheap, and what a warm repository mostly
//	            gets.
//	REBUILD     derive the fact again from scratch. Expensive, and deliberately
//	            NOT what this pass reaches for -- a full rebuild as the generic
//	            answer to an integrity question is how an integrity check
//	            becomes something nobody runs.
//
// The pass only ever demotes. There is no input that makes it mark a withheld
// fact authoritative as a side effect: re-establishing a licence is a
// promotion or a rebuild, both of which are explicit acts elsewhere. That
// asymmetry is the same one drift.go rests on, and for the same reason.

// ValidateRequest asks for one authority pass over one repository.
type ValidateRequest struct {
	ProjectID domain.ProjectID
	// RepoPath is the repository root to validate against.
	RepoPath string
	// Apply writes the demotions. With Apply false the pass is a dry run that
	// reports exactly what it would do and changes nothing, which is what
	// `ao memory validate` defaults to.
	Apply bool
	// MaxChecks bounds the work. Zero means DefaultValidateChecks.
	MaxChecks int
}

// DefaultValidateChecks bounds one validation pass, matching DefaultDriftChecks
// so an operator running both does not have to reason about two limits.
const DefaultValidateChecks = 2000

// ValidateFinding is one fact whose licence no longer holds.
type ValidateFinding struct {
	ItemID string
	Key    domain.ProjectMemoryKey
	From   domain.MemoryAuthority
	To     domain.MemoryAuthority
	// ReasonClass is one of the memory_* constants, and Detail says what
	// specifically was missing.
	ReasonClass string
	Detail      string
	// Applied reports whether the demotion was written. False for a dry run,
	// and also for a demotion the store refused because a newer generation had
	// already moved the row -- which is the stale-validator case and is not an
	// error.
	Applied bool
}

// ValidateReport is what one pass found.
type ValidateReport struct {
	RepoID string
	// Observed is the repository identity read from disk at pass time. It is
	// reported even when nothing was found, because "AO could not identify
	// this checkout" is itself the answer to several operator questions.
	Observed domain.RepoIdentity
	// Checked counts facts whose licence the pass evaluated.
	Checked int
	// Provable counts facts whose licence still holds.
	Provable int
	// IdentityWithheld counts facts withheld because the repository at this
	// path is not the repository they were derived from. It is separate from
	// Findings because it is applied as one indexed statement over the whole
	// repository rather than fact by fact -- there is nothing per-fact to say.
	IdentityWithheld int64
	// LegacyClassified counts pre-P2-D rows moved to legacy_unprovable.
	LegacyClassified int64
	// EdgesRetired counts graph edges withheld because a fact they name is no
	// longer provable.
	EdgesRetired int64
	// Truncated reports that MaxChecks stopped the pass short, so "nothing
	// else was found" covers only what was checked.
	Truncated bool
	Findings  []ValidateFinding
}

// Withheld reports whether the pass found anything to withhold.
func (r ValidateReport) Withheld() bool {
	return len(r.Findings) > 0 || r.IdentityWithheld > 0 || r.LegacyClassified > 0
}

// Validator checks stored licences against AO's own durable facts.
type Validator struct {
	repo Repository
	now  func() time.Time
	// identity lets a test substitute the repository-identity reader.
	// Production always shells out to git through RepoIdentityOf.
	identity func(ctx context.Context, repoPath string) domain.RepoIdentity
}

// NewValidator builds an authority validator over a durable repository.
func NewValidator(repo Repository) *Validator {
	return &Validator{
		repo:     repo,
		now:      func() time.Time { return time.Now().UTC() },
		identity: RepoIdentityOf,
	}
}

// Validate runs one authority pass.
//
// The order of the three phases is load-bearing. Repository identity first,
// because if the checkout is a different repository then every per-fact
// question below is being asked about the wrong project and the answers would
// be noise. Legacy classification second, so the per-fact pass does not have
// to re-derive "this row predates P2-D" for every row. Per-fact proof last,
// over what is left.
func (v *Validator) Validate(ctx context.Context, req ValidateRequest) (ValidateReport, error) {
	repoPath, err := canonicalRepoPath(req.RepoPath)
	if err != nil {
		return ValidateReport{}, err
	}
	repoID := domain.ProjectMemoryRepoID(repoPath)
	observed := v.identity(ctx, repoPath)
	report := ValidateReport{RepoID: repoID, Observed: observed}
	now := v.now()

	limit := req.MaxChecks
	if limit <= 0 {
		limit = DefaultValidateChecks
	}

	// --- 1. Repository identity -----------------------------------------
	//
	// Applied as one statement rather than fact by fact, because it is not a
	// per-fact question: no individual fact is wrong, the premise that these
	// facts are about THIS repository is. The store refuses the sweep outright
	// when the observed identity is unknown -- see
	// MarkProjectMemoryItemsUnprovableByRepoIdentity for why turning "AO could
	// not read the identity today" into "this project's memory is gone" would
	// be the worst possible reading of a missing fact.
	if req.Apply && observed.Known() {
		n, err := v.repo.MarkProjectMemoryItemsUnprovableByRepoIdentity(
			ctx, req.ProjectID, repoID, observed,
			domain.MemoryAuthorityReason(domain.ReasonRepoIdentityChanged, fmt.Sprintf(
				"the repository at %s identifies as %s, and these facts were derived under a different identity",
				repoPath, observed)),
			now)
		if err != nil {
			return report, err
		}
		report.IdentityWithheld = n
	}

	// --- 2. Legacy classification ---------------------------------------
	if req.Apply {
		n, err := v.repo.MarkLegacyProjectMemoryItemsUnprovable(
			ctx, req.ProjectID, repoID,
			domain.MemoryAuthorityReason(domain.ReasonLegacyNoProvenance,
				"written before AO recorded memory provenance; withheld until a bounded rebuild derives it again"),
			now)
		if err != nil {
			return report, err
		}
		report.LegacyClassified = n
	}

	// --- 3. Per-fact proof ----------------------------------------------
	items, err := v.repo.ListProjectMemoryItemsByAuthority(
		ctx, req.ProjectID, repoID, domain.AuthorityAuthoritative)
	if err != nil {
		return report, err
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if report.Checked >= limit {
			report.Truncated = true
			break
		}
		report.Checked++

		finding, ok := v.evaluate(item, observed, repoPath)
		if !ok {
			report.Provable++
			continue
		}
		if req.Apply {
			applied, err := v.repo.SetProjectMemoryItemAuthority(ctx, item.ID, item.Generation,
				finding.To, domain.MemoryAuthorityReason(finding.ReasonClass, finding.Detail), now)
			if err != nil {
				return report, err
			}
			finding.Applied = applied
			if applied {
				// P2-D §23: the graph follows its nodes. An edge derived from
				// a fact AO can no longer prove must stop being traversed as
				// current -- and must not be deleted, because the record that
				// two facts were once related is what an operator reads when
				// asking why a decision was made.
				n, err := v.repo.RetireProjectMemoryRelationsForNode(ctx, req.ProjectID, repoID,
					knowledgeNode(item.ID),
					domain.MemoryAuthorityReason(finding.ReasonClass,
						"the fact this edge names is no longer provable"), now)
				if err != nil {
					return report, err
				}
				report.EdgesRetired += n
			}
		}
		report.Findings = append(report.Findings, finding)
	}

	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].ItemID < report.Findings[j].ItemID })
	return report, nil
}

// evaluate decides whether one fact's licence still holds.
//
// It dispatches on ProvenanceKind rather than on item type, because the KIND
// is what says which proof applies. Two decisions -- one an indexer lifted out
// of a document, one a task recorded -- are the same type and have entirely
// different things to prove.
func (v *Validator) evaluate(
	item domain.ProjectMemoryItem, observed domain.RepoIdentity, repoPath string,
) (ValidateFinding, bool) {
	withhold := func(class, detail string) (ValidateFinding, bool) {
		return ValidateFinding{
			ItemID: item.ID, Key: item.Key,
			From: item.Authority, To: domain.AuthorityUnprovable,
			ReasonClass: class, Detail: detail,
		}, true
	}

	// The identity check again, per fact. The sweep in phase 1 covers the case
	// where AO could read an identity; this covers its complement -- a fact
	// recorded under a known identity in a checkout AO can no longer identify
	// at all, which the sweep cannot express as a WHERE clause and which is
	// just as much a broken premise.
	if !domain.RepoIdentityCompatible(item.RepoIdentity, observed) {
		return withhold(domain.ReasonRepoIdentityChanged, fmt.Sprintf(
			"derived under repository identity %q; %s now identifies as %q",
			orUnknown(item.RepoIdentity), repoPath, orUnknown(observed)))
	}

	switch item.ProvenanceKind {
	case "":
		return withhold(domain.ReasonLegacyNoProvenance,
			"this fact records no provenance kind, so AO cannot say which proof applies to it")

	case domain.ProvenanceLegacy:
		return withhold(domain.ReasonLegacyNoProvenance,
			"written before AO recorded memory provenance")

	case domain.ProvenanceRepoDerivation:
		// Its proof is its evidence, which is drift detection's department --
		// and drift is a separate pass on purpose, so this one does not
		// re-read the filesystem for every fact. What validation adds is the
		// one thing drift cannot check: a derivation that names neither a
		// commit nor a path is anchored to nothing at all, so nothing could
		// ever disprove it. A fact that cannot be disproved is not a strong
		// fact; it is an unfalsifiable one, and it is withheld.
		if item.SourceCommit == "" && len(item.SourcePaths) == 0 && item.Key.Scope != domain.MemoryScopeProject {
			return withhold(domain.ReasonProvenanceMissing,
				"a repository derivation that names neither a source commit nor a source path cannot be checked against anything")
		}
		return ValidateFinding{}, false

	case domain.ProvenanceTaskOutcome, domain.ProvenanceWorkflowKnowledge:
		if item.Origin != domain.OriginCanonical {
			// Task-local knowledge makes no claim to be the project's, so
			// there is no promotion to prove. Its sharing scope already limits
			// who may read it, and that check is at the read side.
			return ValidateFinding{}, false
		}
		// Canonical task knowledge claims the project HAS this work. That
		// claim needs the promotion that made it, and the commit the work
		// landed at. Either one missing is the "unprovable canonical" case
		// P2-D §20 requires to fail closed.
		if strings.TrimSpace(item.PromotionAuthority) == "" {
			return withhold(domain.ReasonPromotionUnprovable,
				"this fact is canonical project knowledge and names no mutation-provenance record that licensed its promotion")
		}
		if strings.TrimSpace(item.IntegratedCommit) == "" {
			return withhold(domain.ReasonPromotionUnprovable,
				"this fact is canonical project knowledge and names no commit the work became part of")
		}
		return ValidateFinding{}, false

	default:
		// A provenance kind this build does not recognise. Withheld rather
		// than served: an unknown proof is not a proof, and a newer build's
		// vocabulary read by an older one must not be treated as vouched for
		// merely because the older build cannot evaluate it.
		return withhold(domain.ReasonProvenanceMissing, fmt.Sprintf(
			"provenance kind %q is not one this build can check", item.ProvenanceKind))
	}
}

func orUnknown(id domain.RepoIdentity) string {
	if !id.Known() {
		return "(unidentifiable)"
	}
	return string(id)
}
