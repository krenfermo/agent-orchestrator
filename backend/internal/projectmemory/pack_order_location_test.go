package projectmemory

import (
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Two clones of one commit (different paths, so different repo identities
// and row ids) must rank equal-scored facts identically: the pack digest is
// reproducible only if the tie-break does not depend on where a checkout
// lives.
func TestSortSelectedIsIndependentOfRepoIdentity(t *testing.T) {
	build := func(repoID string) []SelectedItem {
		var out []SelectedItem
		for _, key := range []string{"internal/store", "internal/auth", "internal/version", "web/src/api"} {
			k := domain.ProjectMemoryKey{ProjectID: "p", RepoID: repoID, Type: domain.ProjectMemoryType("module"), Scope: domain.ProjectMemoryScope("module"), Key: key}
			out = append(out, SelectedItem{Item: domain.ProjectMemoryItem{ID: k.ID(), Key: k}, Score: 1})
		}
		return out
	}
	order := func(items []SelectedItem) []string {
		sortSelected(items, map[domain.ProjectMemoryType]int{})
		var keys []string
		for _, it := range items {
			keys = append(keys, it.Item.Key.Key)
		}
		return keys
	}
	a, b := order(build("repo-at-/tmp/one")), order(build("repo-at-/tmp/two"))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("order depends on repo identity: %v vs %v", a, b)
	}
	if !reflect.DeepEqual(a, []string{"internal/auth", "internal/store", "internal/version", "web/src/api"}) {
		t.Fatalf("tie-break is not by location-free key: %v", a)
	}
}
