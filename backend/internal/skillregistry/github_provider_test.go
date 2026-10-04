package skillregistry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/githubtest"
)

// github_provider_test.go -- the provider against a fixture forge.
//
// Every test here answers one question from the phase-13 negative matrix, and
// the fixture is the only party allowed to misbehave: nothing in this file
// reaches the network, and the forge's bad behaviour is switched on per test
// rather than baked into a second, broken fixture.

func newForge(t *testing.T) (*githubtest.Server, *githubtest.Repo) {
	t.Helper()
	forge := githubtest.New(t)
	repo := forge.AddRepo("acme", "skills", false)
	return forge, repo
}

func openForgeProvider(
	t *testing.T, forge *githubtest.Server, reg skillregistry.Registry,
) *skillregistry.GitHubProvider {
	t.Helper()
	p, err := skillregistry.NewGitHubProvider(context.Background(), reg, nil, forge.Options())
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	return p
}

// TestSearchResolvesTagToCommitAndDownloadsNothing is the two properties the
// whole phase rests on, asserted together because they are one design: a
// search pins a commit, and a search moves no package bytes.
func TestSearchResolvesTagToCommitAndDownloadsNothing(t *testing.T) {
	forge, repo := newForge(t)
	published := repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
	})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))

	found, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("Search returned %d releases, want 1", len(found))
	}
	got := found[0].Source
	if got.Commit != published.Commit {
		t.Fatalf("search pinned commit %q, want %q", got.Commit, published.Commit)
	}
	if got.Tag != published.Tag {
		t.Fatalf("search recorded tag %q, want %q", got.Tag, published.Tag)
	}
	if got.Owner != "acme" || got.Repository != "skills" {
		t.Fatalf("search recorded %s, want acme/skills", got.Slug())
	}
	if got.Visibility != "public" {
		t.Fatalf("visibility is %q, want public", got.Visibility)
	}
	if forge.FetchedArchive() {
		t.Fatal("a search fetched an archive; searching must move no package bytes")
	}
	if found[0].RegistryID != "ext" {
		t.Fatalf("registry id is %q; AO stamps its own", found[0].RegistryID)
	}
}

// TestDescriptorCannotClaimAnotherRepository is the re-stamp rule: a
// descriptor is a file a stranger wrote, and letting it name its own owner
// would put a lie into every provenance row.
func TestDescriptorCannotClaimAnotherRepository(t *testing.T) {
	forge, repo := newForge(t)
	repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
		MutateRelease: func(rel *skillregistry.Release) {
			rel.Source = skillregistry.GitSource{
				Provider:   skillregistry.SourceGitHub,
				Owner:      "totally-not-acme",
				Repository: "elsewhere",
				Commit:     strings.Repeat("f", 40),
			}
		},
	})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	found, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("Search returned %d releases, want 1", len(found))
	}
	if slug := found[0].Source.Slug(); slug != "acme/skills" {
		t.Fatalf("the descriptor's claim survived: source is %s, want acme/skills", slug)
	}
	if found[0].Source.Commit == strings.Repeat("f", 40) {
		t.Fatal("the descriptor chose its own commit; AO must stamp the one it resolved")
	}
}

// TestBranchAsReleaseIsRefused is FASE A: a branch is not a release identity,
// and a listing that dropped one silently would hide the refusal from the
// person who has to go and fix the publishing.
func TestBranchAsReleaseIsRefused(t *testing.T) {
	for _, name := range []string{"main", "master", "refs/heads/release", "HEAD"} {
		if err := skillregistry.CheckImmutableRef(name); !errors.Is(err, skillregistry.ErrMutableRef) {
			t.Fatalf("CheckImmutableRef(%q) = %v, want ErrMutableRef", name, err)
		}
	}
	forge, repo := newForge(t)
	repo.Publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"})
	repo.Tag("main", githubtest.CommitSHA("branchy"))
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	found, err := p.Search(context.Background(), skillregistry.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, rel := range found {
		if rel.Source.Tag == "main" {
			t.Fatal("a release published on a branch name was offered")
		}
	}
}

// TestMovedTagIsVisibleInTheResolution is FASE J's raw material: the provider
// re-resolves the tag every time, so the commit it returns is the one the tag
// points at NOW and a caller holding the previous one can see the move.
func TestMovedTagIsVisibleInTheResolution(t *testing.T) {
	forge, repo := newForge(t)
	spec := githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"}
	first := repo.Publish(t, spec)
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))

	before, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ResolveExactRelease: %v", err)
	}
	if before.Source.Commit != first.Commit {
		t.Fatalf("resolved %s, want %s", before.Source.Commit, first.Commit)
	}

	second := repo.Republish(t, spec, githubtest.CommitSHA("second"))
	after, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ResolveExactRelease after the move: %v", err)
	}
	if after.Source.Commit != second.Commit {
		t.Fatalf("after the force-push AO resolved %s, want %s",
			after.Source.Commit, second.Commit)
	}
	if !after.Source.TagMoved(before.Source) {
		t.Fatal("TagMoved did not see the move; that comparison is the whole detection")
	}
}

// TestFetchArtifactPinsTheCommit is FASE C: the bytes come from the commit the
// release names, and an archive of a different commit is caught by the digest
// rather than by trust in the transport.
func TestFetchArtifactPinsTheCommit(t *testing.T) {
	forge, repo := newForge(t)
	published := repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
	})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	dir := t.TempDir()
	if err := p.FetchArtifact(context.Background(), published.Release, dir); err != nil {
		t.Fatalf("FetchArtifact: %v", err)
	}
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	if digest != published.Release.ArtifactDigest {
		t.Fatalf("fetched tree hashed to %s and the release declares %s",
			digest, published.Release.ArtifactDigest)
	}
	report := p.LastArchive()
	if report.StrippedRoot == "" {
		t.Fatal("no archive root was stripped; a forge archive always has one")
	}
	if report.Skipped == 0 {
		t.Fatal("the .ao descriptor directory was not excluded from the package")
	}
}

// TestWrongCommitArchiveFailsTheDigest is the case where nothing about the
// transport is wrong: a 200, a well-formed archive, real files, and not the
// tree that was resolved.
func TestWrongCommitArchiveFailsTheDigest(t *testing.T) {
	forge, repo := newForge(t)
	good := repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
	})
	other := repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.1.0", Publisher: "acme", Extra: "different",
	})
	forge.Fail(githubtest.Failures{ArchiveOfCommit: other.Commit})

	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	dir := t.TempDir()
	if err := p.FetchArtifact(context.Background(), good.Release, dir); err != nil {
		t.Fatalf("FetchArtifact: %v", err)
	}
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	if digest == good.Release.ArtifactDigest {
		t.Fatal("an archive of a different commit produced the pinned digest")
	}
}

// TestHostileArchivesAreRefused is FASE I, one table for the eight shapes.
func TestHostileArchivesAreRefused(t *testing.T) {
	cases := []struct {
		name string
		fail githubtest.Failures
		want string
	}{
		{"symlink", githubtest.Failures{ArchiveSymlink: true}, "symlink"},
		{"hardlink", githubtest.Failures{ArchiveHardlink: true}, "hard link"},
		{"traversal", githubtest.Failures{ArchiveTraversal: true}, "traverses"},
		{"device node", githubtest.Failures{ArchiveDevice: true}, "device node"},
		{"named pipe", githubtest.Failures{ArchiveFifo: true}, "FIFO"},
		// The traversal's blunter sibling: no "..", just a leading slash.
		{"absolute path", githubtest.Failures{ArchiveAbsolutePath: true}, "absolute path"},
		// Two entries for one path: whichever is read last wins, and which one
		// that is is the archive's choice rather than AO's.
		{"duplicate entry", githubtest.Failures{ArchiveDuplicateEntry: true}, "appears twice"},
		{"decompression bomb", githubtest.Failures{ArchiveBomb: true}, "budget"},
		{"entry flood", githubtest.Failures{ArchiveEntryFlood: true}, "entries"},
		{"two roots", githubtest.Failures{ArchiveTwoRoots: true}, "top-level"},
		{"login page", githubtest.Failures{HTMLArchive: true}, "text/html"},
		// The compressed-body ceiling, which is a DIFFERENT control from the
		// bomb above: one bounds what AO downloads and the other bounds what
		// it expands to, and a system with only one of them is missing half
		// the problem.
		{"oversized body", githubtest.Failures{OversizedArchive: true}, "larger than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forge, repo := newForge(t)
			published := repo.Publish(t, githubtest.Spec{
				SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
			})
			forge.Fail(tc.fail)
			p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
			err := p.FetchArtifact(context.Background(), published.Release, t.TempDir())
			if err == nil {
				t.Fatalf("%s was unpacked; every one of these is refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal for %s says %q, which does not name %q",
					tc.name, err.Error(), tc.want)
			}
		})
	}
}

// TestRateLimitIsNotAnAuthFailure is FASE H: a forge answers 403 for both, and
// sending somebody to rotate a token when they need to wait is how a control
// gets switched off.
func TestRateLimitIsNotAnAuthFailure(t *testing.T) {
	forge, repo := newForge(t)
	repo.Publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"})

	forge.Fail(githubtest.Failures{RateLimited: true})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	_, err := p.Search(context.Background(), skillregistry.Query{})
	if !errors.Is(err, skillregistry.ErrRateLimited) {
		t.Fatalf("a rate-limited 403 produced %v, want ErrRateLimited", err)
	}
	if errors.Is(err, skillregistry.ErrRegistryAuth) {
		t.Fatal("a rate limit was reported as a credential problem")
	}

	forge.Fail(githubtest.Failures{Forbidden: true})
	p2 := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	_, err = p2.Search(context.Background(), skillregistry.Query{})
	if !errors.Is(err, skillregistry.ErrRegistryAuth) {
		t.Fatalf("a plain 403 produced %v, want ErrRegistryAuth", err)
	}
	if errors.Is(err, skillregistry.ErrRateLimited) {
		t.Fatal("a credential refusal was reported as a rate limit")
	}
}

// TestPrivateRepositoryNeedsItsCredential is FASE G, from the client's side:
// the forge answers 401 without one and the error names the REF, never a value.
func TestPrivateRepositoryNeedsItsCredential(t *testing.T) {
	forge, repo := newForge(t)
	repo.Publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"})
	forge.RequireBearer("ghp-secret-value")

	reg := forge.Registry("ext", "acme", "skills")
	p := openForgeProvider(t, forge, reg)
	_, err := p.Search(context.Background(), skillregistry.Query{})
	if !errors.Is(err, skillregistry.ErrRegistryAuth) {
		t.Fatalf("an unauthenticated read produced %v, want ErrRegistryAuth", err)
	}
	if strings.Contains(err.Error(), "ghp-secret-value") {
		t.Fatal("the error quoted the credential value")
	}
}

// TestUnreachableForgeIsOfflineNotEmpty holds the phase-11 distinction for the
// new transport: "AO could not ask" and "there is nothing there" are different
// answers, and only one of them means somebody should go and look.
func TestUnreachableForgeIsOfflineNotEmpty(t *testing.T) {
	forge, repo := newForge(t)
	repo.Publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("warm-up search: %v", err)
	}
	forge.Stop()
	_, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0")
	if err == nil {
		t.Fatal("an install read succeeded against a forge that is not running")
	}
	if !errors.Is(err, skillregistry.ErrRegistryUnreachable) {
		t.Fatalf("a stopped forge produced %v, want ErrRegistryUnreachable", err)
	}
}

// TestProbeConnectedMeansTheRightRepository holds the probe contract: a 200 is
// not enough, and a forge answering about a different repository is
// INVALID_RESPONSE rather than green.
func TestProbeConnectedMeansTheRightRepository(t *testing.T) {
	forge, repo := newForge(t)
	repo.Publish(t, githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	got := p.Probe(context.Background(), nil)
	if got.State != skillregistry.ProbeConnected {
		t.Fatalf("probe state is %s (%s), want CONNECTED", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "says nothing about who wrote the code") {
		t.Fatalf("the probe detail reads as an endorsement: %q", got.Detail)
	}

	missing := openForgeProvider(t, forge, forge.Registry("ext", "acme", "absent"))
	if state := missing.Probe(context.Background(), nil).State; state == skillregistry.ProbeConnected {
		t.Fatal("a repository that does not exist probed as CONNECTED")
	}
}

// TestAnnotatedTagIsPeeledOnce covers the shape most repositories actually
// publish, and the refusal for a chain longer than one peel.
func TestAnnotatedTagIsPeeledOnce(t *testing.T) {
	forge, repo := newForge(t)
	published := repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme", Annotated: true,
	})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	rel, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ResolveExactRelease: %v", err)
	}
	if rel.Source.Commit != published.Commit {
		t.Fatalf("an annotated tag peeled to %s, want %s", rel.Source.Commit, published.Commit)
	}

	forge.Fail(githubtest.Failures{TagPointsAtTree: true})
	p2 := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	if _, err := p2.ResolveExactRelease(context.Background(), "security-audit", "1.0.0"); err == nil {
		t.Fatal("a tag pointing at a tree resolved to a release")
	}
}

// TestSubdirectoryPackageRoot covers a repository that holds more than one
// skill: the package is a subtree, and everything outside it is not package
// content.
func TestSubdirectoryPackageRoot(t *testing.T) {
	forge, repo := newForge(t)
	published := repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
		Path: "skills/security-audit", Tag: "security-audit/v1.0.0",
	})
	p := openForgeProvider(t, forge, forge.Registry("ext", "acme", "skills"))
	rel, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.0.0")
	if err != nil {
		t.Fatalf("ResolveExactRelease: %v", err)
	}
	if rel.Source.Path != "skills/security-audit" {
		t.Fatalf("package path is %q", rel.Source.Path)
	}
	dir := t.TempDir()
	if err := p.FetchArtifact(context.Background(), rel, dir); err != nil {
		t.Fatalf("FetchArtifact: %v", err)
	}
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	if digest != published.Release.ArtifactDigest {
		t.Fatalf("subtree hashed to %s, want %s", digest, published.Release.ArtifactDigest)
	}
}
