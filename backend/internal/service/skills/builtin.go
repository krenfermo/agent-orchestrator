package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// BuiltinActor is the audit actor for packages AO installs itself.
const BuiltinActor = "ao:builtin"

// BuiltinOutcome says what EnsureBuiltins did with one package.
type BuiltinOutcome struct {
	SkillID string
	Version string
	// Action is "installed", "present" (same bytes already installed),
	// "conflict" (that version is installed with DIFFERENT bytes and was left
	// untouched) or "error".
	Action string
	Detail string
}

// EnsureBuiltins makes every package embedded in this build available.
//
// It is idempotent and conservative:
//   - the same version with the same digest already installed is left alone,
//     and writes no audit row, so a boot is not an event in the trail;
//   - the same version with DIFFERENT bytes is never overwritten: an
//     activation pinned that version and must not silently start meaning
//     something else. It is reported, not fixed;
//   - nothing is ever enabled. Installing makes a package available; granting
//     it to a project remains a person's audited decision.
func (s *Service) EnsureBuiltins(ctx context.Context) []BuiltinOutcome {
	out := make([]BuiltinOutcome, 0, len(skillcatalog.BuiltinPackageIDs))
	for _, id := range skillcatalog.BuiltinPackageIDs {
		out = append(out, s.ensureBuiltin(ctx, id))
	}
	return out
}

func (s *Service) ensureBuiltin(ctx context.Context, id string) BuiltinOutcome {
	res := BuiltinOutcome{SkillID: id, Action: "error"}
	if err := os.MkdirAll(filepath.Dir(s.root), 0o750); err != nil {
		res.Detail = fmt.Sprintf("create skills dir: %v", err)
		return res
	}
	tmp, err := os.MkdirTemp(filepath.Dir(s.root), ".builtin-")
	if err != nil {
		res.Detail = fmt.Sprintf("create staging dir: %v", err)
		return res
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	src := filepath.Join(tmp, id)
	if err := skillcatalog.MaterializeBuiltin(id, src); err != nil {
		res.Detail = err.Error()
		return res
	}
	pkg, err := skillcatalog.LoadPackage(src)
	if err != nil {
		res.Detail = fmt.Sprintf("embedded package does not verify: %v", err)
		return res
	}
	res.Version = pkg.Manifest.Version
	existing, ok, err := s.store.GetSkillInstall(ctx, pkg.Manifest.ID, pkg.Manifest.Version)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	if ok {
		if existing.Digest == pkg.Digest {
			res.Action = "present"
			return res
		}
		res.Action = "conflict"
		res.Detail = fmt.Sprintf("%s@%s is installed with digest %s; this build embeds %s. The installed "+
			"copy was left untouched", id, pkg.Manifest.Version, existing.Digest, pkg.Digest)
		return res
	}
	if _, err := s.Install(ctx, InstallRequest{
		SourceDir: src, Actor: BuiltinActor,
		SourceLabel: fmt.Sprintf("the %s@%s package embedded in this AO build", id, pkg.Manifest.Version),
	}); err != nil {
		res.Detail = err.Error()
		return res
	}
	res.Action = "installed"
	return res
}
