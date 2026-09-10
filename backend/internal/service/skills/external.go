package skills

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// external.go -- the half of the marketplace that only exists because a git
// forge's names can move.
//
// # What is different about an external registry, in one list
//
//	the identity is a COMMIT, not a version string
//	a TAG can be re-pointed, silently, by whoever owns the repository
//	there is NO REVOCATION FEED to poll
//	hosting says NOTHING about who wrote the code
//	the publisher name in the package is a claim anybody can type
//
// Everything in this file is one of those five turned into a check that fails
// closed, plus the ledger that makes the second one observable at all.
//
// # What is deliberately NOT different
//
// The digests, the quarantine, the manifest agreement check, the capability
// comparison, the signature chain and the catalog install path. An external
// release goes through exactly the same ones, in the same order, for the same
// reasons. A second install path for "the risky kind" would be a second place
// for a check to be missing.

// ExternalStore is the durable half of external registry support: where each
// tag pointed, and what an administrator here has withdrawn.
//
// It is a SEPARATE interface from MarketplaceStore rather than more methods on
// it, for the reason RegistryStatusStore is separate: an installation with no
// external registry never needs it, and a marketplace without it degrades to
// refusing external installs rather than failing to start.
type ExternalStore interface {
	UpsertSkillExternalTag(ctx context.Context, obs store.SkillExternalTag) (store.SkillExternalTag, error)
	GetSkillExternalTag(ctx context.Context, registryID, owner, repository, tag string) (store.SkillExternalTag, bool, error)
	ListSkillExternalTags(ctx context.Context, registryID string) ([]store.SkillExternalTag, error)
	ListMovedSkillExternalTags(ctx context.Context) ([]store.SkillExternalTag, error)

	// ExternalRevocation reports an administrative withdrawal of an owner, a
	// repository or a commit. It is the whole of external revocation: a forge
	// publishes no feed, and inventing one AO would poll would mean inventing
	// semantics GitHub does not have.
	ExternalRevocation(ctx context.Context, subject skillregistry.RevocationSubject, subjectID string) (skillregistry.TrustRevocation, bool, error)
}

// WithExternal wires the external half.
//
// A separate call rather than another constructor parameter, for the same
// reason WithConnectivity and WithTrust are: most call sites are tests with a
// local fixture registry that has no tags and no forge, and a parameter they
// would pass nil to is a parameter somebody has to read.
func (m *Marketplace) WithExternal(external ExternalStore) *Marketplace {
	if m == nil {
		return nil
	}
	m.external = external
	return m
}

// externalAvailable reports whether this installation can record what an
// external install requires it to record.
//
// It gates the install rather than only the ledger, and that is deliberate:
// installing from a forge WITHOUT the moved-tag ledger would mean AO could
// never afterwards tell that the tag it installed from had moved -- which is
// the single control this whole phase adds. An install that cannot be
// remembered correctly is refused.
func (m *Marketplace) externalAvailable() bool { return m != nil && m.external != nil }

// ---------------------------------------------------------------- tag ledger

// TagStatus is what AO knows about one tag right now, for a listing.
type TagStatus struct {
	Observation store.SkillExternalTag
	Verdict     skillregistry.TagVerdict
}

// observeTag compares a freshly resolved release against the ledger, records
// the result, and returns what it concluded.
//
// It is called on every path that produces a pinned release -- search, detail
// and install -- because a tag that moved between somebody looking and
// somebody installing is exactly the window this exists to close, and a check
// that only ran at install would leave the marketplace showing a commit that
// is no longer there.
//
// It NEVER changes an installed release's recorded commit. The ledger is a
// statement about the world; the provenance row is a statement about this
// host, and only one of them is allowed to be rewritten by somebody else's
// force-push.
func (m *Marketplace) observeTag(
	ctx context.Context, actor string, rel skillregistry.Release,
) (TagStatus, bool) {
	if !m.externalAvailable() {
		return TagStatus{}, false
	}
	fresh, ok := skillregistry.TagObservationOf(rel, m.now())
	if !ok {
		return TagStatus{}, false
	}
	previous, known, err := m.external.GetSkillExternalTag(
		ctx, fresh.RegistryID, fresh.Owner, fresh.Repository, fresh.Tag)
	if err != nil {
		// A ledger AO cannot read must not silently become a ledger that says
		// "stable". The caller treats a missing status as "unknown", and the
		// install path refuses rather than guessing.
		return TagStatus{}, false
	}
	verdict, updated := skillregistry.CompareTag(fresh, previous, known, m.now())
	stored, err := m.external.UpsertSkillExternalTag(ctx, updated)
	if err != nil {
		return TagStatus{}, false
	}
	switch verdict {
	case skillregistry.TagFirstSeen:
		m.audit(ctx, store.SkillAuditEntry{
			Actor: actor, Action: store.SkillAuditExternalReleasePinned,
			SkillID: rel.SkillID, Version: rel.Version, Digest: rel.ArtifactDigest,
			Detail: fmt.Sprintf("%s: tag %s pinned to commit %s. The tag is a label; the commit "+
				"is the release", rel.Source.Slug(), stored.Tag, stored.Commit),
		})
	case skillregistry.TagMoved:
		// Recorded whether or not anybody installs afterwards. A tag moving is
		// not necessarily an attack -- publishers re-tag by mistake -- but it
		// is the one observable a substitution shares with an honest accident,
		// and the trail has to hold it either way.
		m.audit(ctx, store.SkillAuditEntry{
			Actor: actor, Action: store.SkillAuditExternalTagMoved,
			SkillID: rel.SkillID, Version: rel.Version,
			Detail: skillregistry.DescribeTagMove(stored),
		})
	}
	return TagStatus{Observation: stored, Verdict: verdict}, true
}

// ------------------------------------------------------------- policy checks

// checkExternalPolicy is every refusal an external registry adds, in the order
// an operator most wants to read them.
//
// The order is not cosmetic. A registry that is DENIED should say so before
// anybody hears about a publisher mismatch, because the second reads like a
// problem with the package and the first is a decision this installation made.
// And an owner that is revoked outranks a repository that is, which outranks a
// commit, only in the sense of blast radius -- the narrowest true reason is
// the one reported, because "your whole account is banned" is a very different
// conversation from "this one commit is".
func (m *Marketplace) checkExternalPolicy(
	ctx context.Context, reg skillregistry.Registry, rel skillregistry.Release,
) error {
	if !reg.Type.External() {
		return nil
	}
	if !m.externalAvailable() {
		return apierr.Invalid("SKILL_EXTERNAL_LEDGER_UNAVAILABLE",
			"this installation cannot record where an external release came from, so it will not "+
				"install one. Installing without the tag ledger would mean AO could never "+
				"afterwards tell that the tag it installed from had moved", nil)
	}
	if reg.TrustPolicy.BlocksInstall() {
		return apierr.Forbidden("SKILL_EXTERNAL_DENIED",
			fmt.Sprintf("registry %s is on the external_deny policy: it stays visible so people "+
				"can see what it offers, and installs nothing", reg.ID))
	}
	src := rel.Source
	if !src.Declared() {
		return apierr.Forbidden("SKILL_EXTERNAL_NO_COMMIT",
			fmt.Sprintf("%s names no source commit. A release from a forge is one exact commit "+
				"or it is nothing: every other name there can be moved", rel.Ref()))
	}
	if err := src.Validate(); err != nil {
		return apierr.Forbidden("SKILL_EXTERNAL_SOURCE_INVALID", err.Error())
	}
	// The registry reads one account, and a release from another one is a
	// substitution however well formed it is.
	if !strings.EqualFold(src.Owner, reg.Owner) {
		return apierr.Forbidden("SKILL_EXTERNAL_OWNER_MISMATCH",
			fmt.Sprintf("%s is owned by %q and registry %s reads %q",
				src.Slug(), src.Owner, reg.ID, reg.Owner))
	}
	if reg.Repository != "" && !strings.EqualFold(src.Repository, reg.Repository) {
		return apierr.Forbidden("SKILL_EXTERNAL_REPO_MISMATCH",
			fmt.Sprintf("%s is not the repository registry %s reads (%s/%s)",
				src.Slug(), reg.ID, reg.Owner, reg.Repository))
	}
	// The allowlist. It grants PERMISSION TO INSTALL and never trust: an
	// allowlisted owner still installs as verified unless a signature also
	// verifies, which is enforced by AssessInstalled and not here.
	if !reg.AllowsOwner(src.Owner) {
		return apierr.Forbidden("SKILL_EXTERNAL_OWNER_NOT_ALLOWED",
			fmt.Sprintf("registry %s installs only from %s, and %s is owned by %q. Being on the "+
				"allowlist is permission to install, not a statement that the publisher is trusted",
				reg.ID, strings.Join(reg.AllowedOwners, ", "), src.Slug(), src.Owner))
	}
	// The publisher pin (FASE F). It catches the day a repository AO was
	// pointed at starts publishing under a different name -- which is the same
	// day somebody would like the install to go through unnoticed.
	identity := skillregistry.ExternalIdentityOf(rel, reg)
	if err := identity.CheckExpectedPublisher(); err != nil {
		return apierr.Forbidden("SKILL_EXTERNAL_PUBLISHER_MISMATCH", err.Error())
	}
	// Administrative revocation, narrowest true reason first.
	if rev, subject, ok := m.externalRevocation(ctx, src); ok {
		return apierr.Forbidden("SKILL_EXTERNAL_REVOKED",
			fmt.Sprintf("%s was withdrawn on this installation on %s: %s. AO blocks new installs "+
				"under it and removes nothing that is already here",
				describeExternalSubject(subject, rev.SubjectID),
				rev.RevokedAt.UTC().Format(time.RFC3339), rev.Reason))
	}
	return nil
}

// externalRevocation returns the narrowest administrative withdrawal that
// blocks this source.
func (m *Marketplace) externalRevocation(
	ctx context.Context, src skillregistry.GitSource,
) (skillregistry.TrustRevocation, skillregistry.RevocationSubject, bool) {
	if !m.externalAvailable() {
		return skillregistry.TrustRevocation{}, "", false
	}
	for _, candidate := range skillregistry.ExternalSubjectsFor(src) {
		rev, ok, err := m.external.ExternalRevocation(ctx, candidate.Subject, candidate.ID)
		if err != nil || !ok {
			continue
		}
		return rev, candidate.Subject, true
	}
	return skillregistry.TrustRevocation{}, "", false
}

func describeExternalSubject(subject skillregistry.RevocationSubject, id string) string {
	switch subject {
	case skillregistry.SubjectExternalOwner:
		return "the account " + id
	case skillregistry.SubjectExternalRepository:
		return "the repository " + id
	case skillregistry.SubjectExternalCommit:
		return "the commit " + id
	}
	return id
}

// -------------------------------------------------------------- moved tags

// checkTagNotMoved refuses an install that would silently follow a moved tag,
// and refuses one that would silently change the commit behind a version that
// is already installed.
//
// Two different refusals, because they are two different surprises. The first
// is "the name you searched for now means something else". The second is "you
// already have 1.2.3 and it is not this". Either one, taken silently, is the
// force-push substitution completing.
func (m *Marketplace) checkTagNotMoved(
	ctx context.Context, reg skillregistry.Registry, rel skillregistry.Release,
	status TagStatus, known, acknowledged bool,
) error {
	if !reg.Type.External() {
		return nil
	}
	if known && status.Verdict == skillregistry.TagMoved && !acknowledged {
		return apierr.Conflict("SKILL_EXTERNAL_TAG_MOVED",
			skillregistry.DescribeTagMove(status.Observation)+" "+skillregistry.MovedTagRefusal,
			map[string]any{
				"tag":             status.Observation.Tag,
				"previousCommit":  status.Observation.MovedFromCommit,
				"resolvedCommit":  status.Observation.Commit,
				"repository":      status.Observation.Owner + "/" + status.Observation.Repository,
				"acknowledgeWith": "acknowledgeMovedTag",
			})
	}
	// What is ALREADY on this host, which the ledger does not answer: the
	// ledger is per repository and this is per installed version.
	existing, ok, err := m.store.GetSkillInstallOrigin(ctx, rel.SkillID, rel.Version)
	if err != nil || !ok {
		return nil //nolint:nilerr // no prior install is the ordinary case.
	}
	if existing.Source.Commit == "" || existing.Source.Commit == rel.Source.Commit {
		return nil
	}
	// NOT bypassable by acknowledgement, and that is deliberate. Acknowledging
	// a moved tag says "I mean to take the new commit"; it does not say "and
	// overwrite the copy I already have under the same version number". The
	// catalog refuses that anyway -- one version, one set of bytes, forever --
	// and a flag here that appeared to permit it would produce a refusal one
	// layer down with a message about package contents rather than about the
	// tag, which is the wrong thing to send somebody to look at.
	//
	// The fix is the honest one: uninstall the version, or the publisher
	// publishes a new one.
	return apierr.Conflict("SKILL_EXTERNAL_COMMIT_CHANGED",
		fmt.Sprintf("%s is already installed from commit %s and %s now resolves to %s. AO keeps "+
			"what is installed and its provenance exactly as they are: one version means one set "+
			"of bytes on this host, forever, and quietly replacing them would erase the record of "+
			"what was actually installed. Uninstall %s first if you mean to take the new commit, "+
			"or ask the publisher for a new version",
			rel.Ref(), existing.Source.Commit, rel.Source.Tag, rel.Source.Commit, rel.Ref()),
		map[string]any{
			"installedCommit": existing.Source.Commit,
			"resolvedCommit":  rel.Source.Commit,
		})
}

// checkOfflineExternalPin refuses an offline install whose cached bytes cannot
// be tied to the commit AO recorded for the tag.
//
// # The gap this closes
//
// The artifact cache is addressed by (registry, artifact digest, manifest
// digest), which is exactly right: the key IS the verification. But the
// OFFLINE lookup goes the other way -- it asks for "whatever I have for
// skillId@version" -- and on a forge that question has more than one honest
// answer, because a moved tag means two different commits have both been
// published as 1.0.0. Handing back whichever entry came first would make an
// offline install the one path where a substituted commit is taken silently,
// which is the whole thing this phase exists to prevent.
//
// So an offline external install is pinned by the LEDGER, which is local and
// therefore still readable when the forge is not:
//
//	no ledger row      AO has no record of where this tag pointed. Refuse.
//	ledger disagrees   the cached bytes are from a different commit. Refuse.
//	ledger agrees      proceed; every digest check still runs afterwards.
//
// A release with no tag at all is pinned to a commit and nothing else, so
// there is no name for anybody to have moved and nothing to check here.
func (m *Marketplace) checkOfflineExternalPin(
	ctx context.Context, reg skillregistry.Registry, cached skillregistry.Release,
) error {
	if !reg.Type.External() {
		return nil
	}
	src := cached.Source
	if !src.Declared() {
		return apierr.Forbidden("SKILL_EXTERNAL_NO_COMMIT",
			fmt.Sprintf("AO holds bytes for %s but no record of which commit they came from, so "+
				"it will not install them while the forge is unreachable", cached.Ref()))
	}
	if src.Tag == "" {
		return nil
	}
	if !m.externalAvailable() {
		return apierr.Invalid("SKILL_EXTERNAL_LEDGER_UNAVAILABLE",
			"this installation cannot read its tag ledger, so it cannot confirm which commit these "+
				"cached bytes belong to", nil)
	}
	row, ok, err := m.external.GetSkillExternalTag(
		ctx, reg.ID, src.Owner, src.Repository, src.Tag)
	if err != nil || !ok {
		return apierr.Forbidden("SKILL_EXTERNAL_TAG_UNKNOWN",
			fmt.Sprintf("registry %s cannot be reached and AO has no record of where %s in %s "+
				"pointed, so it cannot tell whether the bytes it holds are the ones that tag means",
				reg.ID, src.Tag, src.Slug()))
	}
	if row.Commit != src.Commit {
		return apierr.Conflict("SKILL_EXTERNAL_TAG_MOVED",
			fmt.Sprintf("registry %s cannot be reached. AO holds bytes for %s from commit %s, and "+
				"the last time it could ask, %s pointed at %s. It will not install one while "+
				"claiming to be the other",
				reg.ID, cached.Ref(), src.Commit, src.Tag, row.Commit),
			map[string]any{"cachedCommit": src.Commit, "recordedCommit": row.Commit})
	}
	return nil
}

// ------------------------------------------------- administrative revocation

// ExternalRevokeRequest is one administrative withdrawal of a place on a forge.
type ExternalRevokeRequest struct {
	Subject   skillregistry.RevocationSubject
	SubjectID string
	Reason    string

	Actor            string
	ActorPermissions []domain.Permission
}

// RevokeExternal withdraws an owner, a repository or a commit.
//
// # Why this is not a feed
//
// A forge publishes no revocation list. There is no endpoint AO could poll to
// learn that a repository was compromised, and building one would mean
// building semantics GitHub does not have and then rendering them as if the
// forge had said them. So this is an administrator here writing down what this
// installation will no longer install from, and every screen says so.
//
// # What it does not do
//
// Uninstall anything, delete files, disable a skill on a project or stop a run
// under way -- the same promise release revocation has carried since ADR 0006.
// It blocks NEW installs and marks what is already here.
func (a *TrustAuthority) RevokeExternal(
	ctx context.Context, req ExternalRevokeRequest,
) (skillregistry.TrustRevocation, error) {
	if err := a.requireAvailable(); err != nil {
		return skillregistry.TrustRevocation{}, err
	}
	if err := requireSettingsManage(req.ActorPermissions,
		"revoking an external source"); err != nil {
		return skillregistry.TrustRevocation{}, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_EXTERNAL_REVOKE_ANONYMOUS",
			"a revocation records who made it", nil)
	}
	if !req.Subject.External() {
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT",
			fmt.Sprintf("%q is not an external subject; use the trust revocation surface for a "+
				"key, a publisher or a root", req.Subject), nil)
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_EXTERNAL_REVOKE_NO_REASON",
			"a revocation must say why; one that does not is indistinguishable from a mistake, "+
				"and it is about to block every install under it", nil)
	}
	subjectID, err := normalizeExternalSubjectID(req.Subject, req.SubjectID)
	if err != nil {
		return skillregistry.TrustRevocation{}, err
	}
	now := a.now()
	rev, err := a.store.UpsertSkillTrustRevocation(ctx, skillregistry.TrustRevocation{
		Subject: req.Subject, SubjectID: subjectID, Reason: reason,
		RevokedAt: now, RevokedBy: req.Actor,
	})
	if err != nil {
		return skillregistry.TrustRevocation{}, err
	}
	a.audit(ctx, store.SkillAuditEntry{
		Actor: req.Actor, Action: store.SkillAuditExternalRevoked,
		Detail: fmt.Sprintf("%s withdrawn: %s. New installs from it are refused; nothing already "+
			"installed was removed, disabled or stopped",
			describeExternalSubject(req.Subject, subjectID), reason),
	})
	return rev, nil
}

// LiftExternalRevocation removes an administrative withdrawal.
//
// It exists for the external subjects ONLY, and the asymmetry is deliberate. A
// signing key somebody else may have held is compromised forever: un-revoking
// one because a form was re-submitted is how a compromise comes back, so
// TrustAuthority.Revoke is one-way and stays that way. An external subject is
// a judgement about a PLACE -- a repository transferred to somebody who was
// then vetted, an account banned during an incident that turned out to be a
// false alarm -- and places get re-vetted.
func (a *TrustAuthority) LiftExternalRevocation(
	ctx context.Context, subject skillregistry.RevocationSubject, subjectID string,
	actor string, perms []domain.Permission,
) error {
	if err := a.requireAvailable(); err != nil {
		return err
	}
	if err := requireSettingsManage(perms, "lifting an external revocation"); err != nil {
		return err
	}
	if !subject.External() {
		return apierr.Forbidden("SKILL_EXTERNAL_LIFT_SUBJECT",
			fmt.Sprintf("%q cannot be lifted. A key, a publisher or a root somebody decided was "+
				"compromised does not come back because a form was re-submitted", subject))
	}
	id, err := normalizeExternalSubjectID(subject, subjectID)
	if err != nil {
		return err
	}
	lifter, ok := a.store.(interface {
		DeleteSkillTrustRevocation(context.Context, skillregistry.RevocationSubject, string) (bool, error)
	})
	if !ok {
		return apierr.Invalid("SKILL_EXTERNAL_LIFT_UNAVAILABLE",
			"this installation's trust store cannot lift a revocation", nil)
	}
	removed, err := lifter.DeleteSkillTrustRevocation(ctx, subject, id)
	if err != nil {
		return err
	}
	if !removed {
		return apierr.NotFound("SKILL_EXTERNAL_REVOCATION_NOT_FOUND",
			fmt.Sprintf("%s is not withdrawn", describeExternalSubject(subject, id)))
	}
	a.audit(ctx, store.SkillAuditEntry{
		Actor: actor, Action: store.SkillAuditExternalRevocationLift,
		Detail: fmt.Sprintf("%s is no longer withdrawn; installs from it are permitted again",
			describeExternalSubject(subject, id)),
	})
	return nil
}

// ListExternalRevocations returns every external withdrawal, newest first.
func (a *TrustAuthority) ListExternalRevocations(
	ctx context.Context,
) ([]skillregistry.TrustRevocation, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	all, err := a.store.ListSkillTrustRevocations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]skillregistry.TrustRevocation, 0, len(all))
	for _, rev := range all {
		if rev.Subject.External() {
			out = append(out, rev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RevokedAt.Equal(out[j].RevokedAt) {
			return out[i].RevokedAt.After(out[j].RevokedAt)
		}
		return out[i].SubjectID < out[j].SubjectID
	})
	return out, nil
}

// normalizeExternalSubjectID enforces one spelling per subject.
//
// Two spellings of the same withdrawal would be two rows, and the install path
// would consult whichever one it happened to build -- which means an
// administrator could revoke "Acme/Thing" and watch installs of "acme/thing"
// carry on.
func normalizeExternalSubjectID(
	subject skillregistry.RevocationSubject, raw string,
) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_NO_SUBJECT",
			"a revocation must name what it withdraws", nil)
	}
	switch subject {
	case skillregistry.SubjectExternalOwner:
		src := skillregistry.GitSource{
			Provider: skillregistry.SourceGitHub, Owner: id,
			Repository: "placeholder", Commit: strings.Repeat("0", 40),
		}
		if err := src.Validate(); err != nil {
			return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT_ID",
				fmt.Sprintf("%q is not a plain account or organization name", raw), nil)
		}
		return id, nil
	case skillregistry.SubjectExternalRepository:
		owner, repo, ok := strings.Cut(id, "/")
		if !ok {
			return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT_ID",
				fmt.Sprintf("a repository is named %q, not %q", "owner/repository", raw), nil)
		}
		src := skillregistry.GitSource{
			Provider: skillregistry.SourceGitHub, Owner: owner, Repository: repo,
			Commit: strings.Repeat("0", 40),
		}
		if err := src.Validate(); err != nil {
			return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT_ID", err.Error(), nil)
		}
		return owner + "/" + repo, nil
	case skillregistry.SubjectExternalCommit:
		slug, sha, ok := strings.Cut(id, "@")
		if !ok {
			return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT_ID",
				fmt.Sprintf("a commit is named %q, not %q; a bare SHA is meaningless without the "+
					"repository it belongs to", "owner/repository@<sha>", raw), nil)
		}
		owner, repo, ok := strings.Cut(slug, "/")
		if !ok {
			return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT_ID",
				fmt.Sprintf("a commit is named %q, not %q", "owner/repository@<sha>", raw), nil)
		}
		src := skillregistry.GitSource{
			Provider: skillregistry.SourceGitHub, Owner: owner, Repository: repo,
			Commit: strings.ToLower(strings.TrimSpace(sha)),
		}
		if err := src.Validate(); err != nil {
			return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT_ID", err.Error(), nil)
		}
		return src.CommitRef(), nil
	}
	return "", apierr.Invalid("SKILL_EXTERNAL_REVOKE_SUBJECT",
		fmt.Sprintf("%q is not an external subject", subject), nil)
}
