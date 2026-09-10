package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// skill_external_store.go -- the two durable facts an external registry needs
// that no earlier phase had a place for.
//
//	skill_external_tags        where a tag pointed, and whether it moved
//	skill_trust_revocations    (widened) an owner, a repository or a commit
//	                           this installation will not install from
//
// # Why the tag ledger is not part of an install
//
// AO has to be able to notice that v1.2.3 moved whether or not anybody ever
// installed it, and it has to keep noticing after the install is removed. A
// column on skill_install_origins could do neither.
//
// # Why a moved tag is never repaired
//
// The row keeps the commit the tag moved AWAY from. An overwrite would make
// "this tag moved in March" unprovable ten minutes later, and it is exactly
// the fact an incident review needs. The same rule holds one level up: an
// installed release's recorded commit is never rewritten either, because the
// bytes on this host really did come from that commit.

// SkillExternalTag is one remembered (tag -> commit) mapping.
type SkillExternalTag = skillregistry.TagObservation

// UpsertSkillExternalTag records where a tag points now.
//
// The caller decides what the row should say -- skillregistry.CompareTag turns
// a fresh observation and the stored one into the verdict and the row -- so
// this method writes and does not judge. Two callers deciding "did it move"
// differently is how one of them ends up wrong.
func (s *Store) UpsertSkillExternalTag(
	ctx context.Context, obs SkillExternalTag,
) (SkillExternalTag, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillExternalTag(ctx, gen.UpsertSkillExternalTagParams{
		RegistryID:      obs.RegistryID,
		Owner:           obs.Owner,
		Repository:      obs.Repository,
		Tag:             obs.Tag,
		CommitSha:       obs.Commit,
		FirstObservedAt: obs.FirstObservedAt.UTC(),
		LastObservedAt:  obs.LastObservedAt.UTC(),
		MovedFromCommit: obs.MovedFromCommit,
		MovedAt:         timePtrToNullTime(obs.MovedAt),
	})
	if err != nil {
		return SkillExternalTag{}, fmt.Errorf("upsert skill external tag: %w", err)
	}
	return externalTagFromRow(row), nil
}

// GetSkillExternalTag returns what AO recorded for one tag.
func (s *Store) GetSkillExternalTag(
	ctx context.Context, registryID, owner, repository, tag string,
) (SkillExternalTag, bool, error) {
	row, err := s.qr.GetSkillExternalTag(ctx, gen.GetSkillExternalTagParams{
		RegistryID: registryID,
		Owner:      owner,
		Repository: repository,
		Tag:        tag,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SkillExternalTag{}, false, nil
		}
		return SkillExternalTag{}, false, fmt.Errorf("get skill external tag: %w", err)
	}
	return externalTagFromRow(row), true, nil
}

// ListSkillExternalTags returns every tag AO has recorded for one registry.
func (s *Store) ListSkillExternalTags(
	ctx context.Context, registryID string,
) ([]SkillExternalTag, error) {
	rows, err := s.qr.ListSkillExternalTags(ctx, registryID)
	if err != nil {
		return nil, fmt.Errorf("list skill external tags: %w", err)
	}
	out := make([]SkillExternalTag, 0, len(rows))
	for _, row := range rows {
		out = append(out, externalTagFromRow(row))
	}
	return out, nil
}

// ListMovedSkillExternalTags returns every tag AO has seen move.
//
// It is a separate query rather than a filter in Go because it is what a
// marketplace listing consults on every render: one row per moved tag, across
// every registry, rather than the whole ledger.
func (s *Store) ListMovedSkillExternalTags(ctx context.Context) ([]SkillExternalTag, error) {
	rows, err := s.qr.ListMovedSkillExternalTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("list moved skill external tags: %w", err)
	}
	out := make([]SkillExternalTag, 0, len(rows))
	for _, row := range rows {
		out = append(out, externalTagFromRow(row))
	}
	return out, nil
}

func externalTagFromRow(row gen.SkillExternalTag) SkillExternalTag {
	return SkillExternalTag{
		RegistryID:      row.RegistryID,
		Owner:           row.Owner,
		Repository:      row.Repository,
		Tag:             row.Tag,
		Commit:          row.CommitSha,
		FirstObservedAt: row.FirstObservedAt,
		LastObservedAt:  row.LastObservedAt,
		MovedFromCommit: row.MovedFromCommit,
		MovedAt:         nullTimeToPtr(row.MovedAt),
	}
}

// ExternalRevocation reports an administrative withdrawal of an owner, a
// repository or a commit.
//
// It is a THIN wrapper over GetSkillTrustRevocation and it exists for one
// reason: it refuses a non-external subject. The two questions live in one
// table because both are "what did an administrator here decide", and a caller
// that could ask this method about a signing key would be a caller who thinks
// key revocation is scoped to forges.
func (s *Store) ExternalRevocation(
	ctx context.Context, subject skillregistry.RevocationSubject, subjectID string,
) (skillregistry.TrustRevocation, bool, error) {
	if !subject.External() {
		return skillregistry.TrustRevocation{}, false,
			fmt.Errorf("skill external revocation: %q is not an external subject", subject)
	}
	id := strings.TrimSpace(subjectID)
	if id == "" {
		return skillregistry.TrustRevocation{}, false, nil
	}
	return s.GetSkillTrustRevocation(ctx, subject, id)
}

// DeleteSkillTrustRevocation lifts an administrative revocation.
//
// The Go layer above refuses to lift a key, publisher or root revocation --
// a key somebody else may have held is compromised forever, and un-revoking
// one because a form was re-submitted is how a compromise comes back. The
// external subjects are different in kind: they are a judgement about a place,
// and places get re-vetted.
func (s *Store) DeleteSkillTrustRevocation(
	ctx context.Context, subject skillregistry.RevocationSubject, subjectID string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteSkillTrustRevocation(ctx, gen.DeleteSkillTrustRevocationParams{
		Subject: string(subject), SubjectID: subjectID,
	})
	if err != nil {
		return false, fmt.Errorf("delete skill trust revocation: %w", err)
	}
	return n > 0, nil
}
