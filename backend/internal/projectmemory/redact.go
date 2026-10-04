package projectmemory

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/repoaccess"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// redact.go — Frente 3 / 3B: no memory item is persisted unredacted.
//
// Items carry excerpts of repository files (docker-compose, CI workflows,
// config, README, instruction files) and, since P2-C, text an AGENT wrote
// (task outcomes, decisions, risks). Any of them can contain a credential.
// Every item write goes through Repository.PutProjectMemoryItem, so the
// repository handed to the Indexer and the Service is wrapped here and each
// item's free text is redacted on the way in. Redaction is deterministic, so a
// reconfirmed fact keeps its content hash and the no-op path stays a no-op.
//
// Rows written before 3B are not rewritten by this; they are redacted again
// where they leave AO (ContextPack.Render and the HTTP item DTOs).

type redactingRepository struct {
	Repository
}

// withRedaction wraps repo so every item write is redacted. Wrapping twice is
// harmless (redaction is idempotent) but is avoided.
func withRedaction(repo Repository) Repository {
	if repo == nil {
		return nil
	}
	if _, ok := repo.(redactingRepository); ok {
		return repo
	}
	return redactingRepository{Repository: repo}
}

func (r redactingRepository) PutProjectMemoryItem(
	ctx context.Context, item domain.ProjectMemoryItem, now time.Time,
) (store.ProjectMemoryWriteOutcome, error) {
	return r.Repository.PutProjectMemoryItem(ctx, RedactItem(item), now)
}

// RedactItem returns item with its summary, content and metadata values
// passed through the repository redaction boundary.
func RedactItem(item domain.ProjectMemoryItem) domain.ProjectMemoryItem {
	item.Summary = repoaccess.RedactString(item.Summary)
	item.Content = repoaccess.RedactString(item.Content)
	if len(item.Metadata) > 0 {
		md := make(map[string]string, len(item.Metadata))
		for k, v := range item.Metadata {
			md[k] = repoaccess.RedactString(v)
		}
		item.Metadata = md
	}
	return item
}
