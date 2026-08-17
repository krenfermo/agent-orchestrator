package command

import "testing"

// TestMergeEnv_IsolatedRuntimeHomeWins is Checkpoint 8P-B.1's planner
// isolation proof: before this checkpoint, Planner.Generate never set
// cmd.Env at all, so the planner subprocess silently inherited the
// daemon's own real environment (Go's exec.Cmd default). mergeEnv is what
// closes that gap -- the workflow owner's isolated runtime-home env must
// override the real inherited env, key by key.
func TestMergeEnv_IsolatedRuntimeHomeWins(t *testing.T) {
	base := []string{"HOME=/real/host/home", "PATH=/usr/bin", "UNRELATED=x"}
	overrides := map[string]string{"HOME": "/ao/users/user-a/runtime-home", "CLAUDE_CONFIG_DIR": "/ao/users/user-a/providers/claude-code"}
	out := mergeEnv(base, overrides)

	seen := map[string]string{}
	for _, kv := range out {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				seen[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	if seen["HOME"] != "/ao/users/user-a/runtime-home" {
		t.Fatalf("isolated HOME did not win: %+v", seen)
	}
	if seen["CLAUDE_CONFIG_DIR"] != "/ao/users/user-a/providers/claude-code" {
		t.Fatalf("isolated CLAUDE_CONFIG_DIR missing: %+v", seen)
	}
	if seen["PATH"] != "/usr/bin" {
		t.Fatalf("unrelated inherited var was dropped: %+v", seen)
	}
	for _, kv := range out {
		if kv == "HOME=/real/host/home" {
			t.Fatal("real host HOME leaked into the merged env alongside the override")
		}
	}
}

func TestMergeEnv_NilOverrides_PreservesBase(t *testing.T) {
	base := []string{"HOME=/real/host/home", "PATH=/usr/bin"}
	out := mergeEnv(base, nil)
	if len(out) != len(base) {
		t.Fatalf("nil overrides must be a no-op, got %v", out)
	}
}
