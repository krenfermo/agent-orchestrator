package skillregistry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// gitsource.go -- what a release from a git host IS, and the three things it
// deliberately refuses to be.
//
// # A release is a COMMIT, not a branch and not a tag
//
// Phases 10 to 12 could describe a release with two digests and be done: a
// private registry serves one immutable artifact per version, and AO hashes
// what lands. A git host is different in one way that matters more than every
// other difference put together: the names people use to talk about code
// there -- branches and tags -- are MUTABLE. `main` is a different tree every
// afternoon; `v1.2.3` is whatever the publisher last pointed it at, and
// re-pointing it takes one command and leaves no trace on the name.
//
// So a GitSource pins a COMMIT SHA, and everything else in it is either
// derived from that commit or is a label recorded for a human to read. A tag
// is kept because it is how a person finds a version, and it is never what an
// install acts on: resolution turns the tag into a SHA once, records both, and
// from then on the SHA is the release's identity.
//
// # A branch is refused outright
//
// Not "discouraged", not "warned about". A branch has no version semantics, no
// publication moment and no way to be re-fetched later with any confidence
// that the bytes are the same ones somebody approved. A registry entry naming
// one is a configuration error and reads as one.
//
// # What this file does NOT claim
//
// That the commit is signed, that the publisher owns the repository, that
// GitHub verified anything, or that hosting on a well-known forge says
// anything at all about who wrote the code. Those are trust questions and they
// are answered by the signature chain in verify.go, which a GitSource does not
// touch. What a pinned commit buys is that the bytes AO fetches tomorrow are
// the bytes AO fetched today -- integrity across time, which is a real and
// separate property from provenance.

// ErrInvalidSource wraps every git-source validation failure.
var ErrInvalidSource = errors.New("skillregistry: invalid release source")

// ErrMutableRef marks a source that identifies its release by something that
// can change under it -- a branch, a moving tag, a bare "HEAD".
//
// It is its own sentinel because the refusal has a specific fix ("publish a
// tag and let AO pin its commit") that no other configuration error shares,
// and because the negative tests assert on it by identity rather than by
// message.
var ErrMutableRef = errors.New("skillregistry: a mutable ref is not a release identity")

func invalidSourcef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSource, fmt.Sprintf(format, args...))
}

// SourceProvider is which kind of host a release's bytes come from.
type SourceProvider string

const (
	// SourceGitHub is github.com or a GitHub-compatible API this installation
	// was pointed at. It is the only external provider this build implements.
	SourceGitHub SourceProvider = "github"
)

// Valid reports whether p is a provider this build knows.
func (p SourceProvider) Valid() bool { return p == SourceGitHub }

var (
	// GitHub's own rule for an account or organization name: alphanumeric and
	// single hyphens, no leading or trailing hyphen, at most 39 characters.
	// It is enforced here rather than left to the API because an owner is a
	// PATH SEGMENT in every request this package builds, and a segment nobody
	// validated is a segment somebody escapes out of.
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}$`)
	// A repository name. GitHub is more permissive than this (it allows dots
	// and underscores, which this does too) and it is still narrow enough that
	// no entry can carry a slash, a space or a traversal.
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	// A full commit SHA, lowercase. An ABBREVIATED sha is refused: an
	// abbreviation is a prefix, a prefix can become ambiguous as a repository
	// grows, and "the release AO installed is whichever commit matched first"
	// is not an identity.
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// A tag name AO will accept. Deliberately narrower than git's own rule:
	// git permits almost anything, and a tag is a path segment and a display
	// string here.
	tagRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,127}$`)
	// A package subdirectory inside the repository. Slash-separated, relative,
	// no traversal -- checked again by cleanRepoPath, which is what actually
	// enforces it.
	repoPathRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
)

// branchLikeRefs are names that are branches, or are so nearly always branches
// that accepting one would be accepting a moving target.
//
// The check is not "is this in the list" alone -- a fully qualified
// refs/heads/... is refused whatever it is called, and so is any name a caller
// hands over in the tag field that git would resolve through a branch.
var branchLikeRefs = map[string]bool{
	"head": true, "main": true, "master": true, "trunk": true,
	"develop": true, "development": true, "dev": true, "latest": true,
	"stable": true, "release": true, "next": true, "edge": true,
}

// GitSource is where one release's bytes come from on a git host.
//
// Every field is filled by AO during resolution, from answers AO asked for by
// path -- never adopted wholesale from a payload a publisher wrote. The
// provider re-stamps Provider, Owner and Repository from the CONFIGURATION
// after decoding a descriptor, for the same reason Release.RegistryID is
// re-stamped: a descriptor that could name its own repository could claim to
// be somebody else's.
type GitSource struct {
	// Provider is the host kind. Empty means this release did not come from a
	// git host at all, which is what every local and https release is.
	Provider SourceProvider `json:"provider,omitempty"`
	// Owner is the account or organization. It is half of the publisher
	// IDENTITY (see publisheridentity.go) and it is not, on its own, a
	// statement that anybody is trustworthy.
	Owner string `json:"owner,omitempty"`
	// Repository is the repository name, without the owner.
	Repository string `json:"repository,omitempty"`
	// Tag is the release tag, when there was one. It is UX: a person searches
	// for "v1.2.3", and a tag is how they find it.
	//
	// It is NEVER the identity. AO resolves it to a commit once and pins that,
	// and if the tag later points somewhere else AO says so rather than
	// following it.
	Tag string `json:"tag,omitempty"`
	// Commit is the identity: a full 40-character lowercase SHA.
	Commit string `json:"commit,omitempty"`
	// Path is the package root inside the repository, empty for the repository
	// root. It exists because a repository that holds three skills is an
	// ordinary thing and cutting one release per repository would be a rule
	// about layout rather than about safety.
	Path string `json:"path,omitempty"`
	// Visibility is what the host said about the repository when AO last
	// looked: "public" or "private". It is display metadata and a warning
	// surface -- a private repository whose credential is later removed simply
	// stops being readable -- and it is never a trust input.
	Visibility string `json:"visibility,omitempty"`
}

// Declared reports whether this release names a git source at all.
func (g GitSource) Declared() bool { return strings.TrimSpace(string(g.Provider)) != "" }

// Slug is "owner/repository", the identity a person recognises.
func (g GitSource) Slug() string {
	if g.Owner == "" && g.Repository == "" {
		return ""
	}
	return g.Owner + "/" + g.Repository
}

// CommitRef is "owner/repository@<sha>", the identity an INSTALL acts on.
//
// It is the string every external revocation of a commit is keyed by, and the
// one an audit line prints, because a bare SHA is meaningless without the
// repository it belongs to: two repositories can hold the same commit.
func (g GitSource) CommitRef() string {
	if g.Commit == "" {
		return g.Slug()
	}
	return g.Slug() + "@" + g.Commit
}

// ShortCommit is the first twelve characters, for a place where the full SHA
// does not fit.
//
// Twelve and not seven: seven collides in a large repository, and a display
// abbreviation that can collide is one somebody eventually compares two of.
// The full SHA is always shown beside it -- deciding two commits are the same
// one needs every character.
func (g GitSource) ShortCommit() string {
	if len(g.Commit) < 12 {
		return g.Commit
	}
	return g.Commit[:12]
}

// Public reports whether the host called this repository public.
func (g GitSource) Public() bool { return g.Visibility == "public" }

// Validate enforces the source contract.
//
// It is strict in the same way Release.Validate is, and for one extra reason:
// every string here becomes a path segment in a request AO builds, and a
// segment that was never validated is a segment an attacker writes.
func (g GitSource) Validate() error {
	if !g.Declared() {
		return invalidSourcef("no provider is named")
	}
	if !g.Provider.Valid() {
		return invalidSourcef("provider %q is not a source AO knows", g.Provider)
	}
	if !ownerRe.MatchString(g.Owner) {
		return invalidSourcef("owner %q is not a plain account or organization name", g.Owner)
	}
	if !repoRe.MatchString(g.Repository) {
		return invalidSourcef("repository %q is not a plain repository name", g.Repository)
	}
	if g.Repository == "." || g.Repository == ".." {
		return invalidSourcef("repository %q is not a repository name", g.Repository)
	}
	if !commitRe.MatchString(g.Commit) {
		// The single most load-bearing refusal in this file. An abbreviated
		// SHA, an empty one, or an uppercase one all land here, and all three
		// would leave the release pinned to something other than one exact
		// tree.
		return invalidSourcef("commit %q is not a full 40-character lowercase SHA; a release from a "+
			"git host is identified by one exact commit, because every other name on a git host "+
			"can be moved", g.Commit)
	}
	if tag := strings.TrimSpace(g.Tag); tag != "" {
		if err := checkImmutableRefName(tag); err != nil {
			return err
		}
	}
	if path := strings.TrimSpace(g.Path); path != "" {
		if !repoPathRe.MatchString(path) {
			return invalidSourcef("path %q is not a plain relative directory inside the repository", path)
		}
		if _, err := cleanRepoPath(path); err != nil {
			return err
		}
	}
	switch g.Visibility {
	case "", "public", "private":
	default:
		return invalidSourcef("visibility %q is not public or private", g.Visibility)
	}
	return nil
}

// checkImmutableRefName refuses a ref that is a branch or behaves like one.
//
// The three refusals are separate because they have different fixes: a
// refs/heads/ ref is a branch spelled out, a well-known branch name is a
// branch spelled casually, and a malformed name is neither. An operator
// reading "main is a branch" goes and tags a release; one reading "not a tag
// name" goes and looks at their typo.
func checkImmutableRefName(ref string) error {
	lower := strings.ToLower(strings.TrimSpace(ref))
	if strings.HasPrefix(lower, "refs/heads/") {
		return fmt.Errorf("%w: %q is a branch. A branch is a different tree every time somebody "+
			"pushes, so it names no release; publish a tag and AO will pin the commit it points at",
			ErrMutableRef, ref)
	}
	if strings.HasPrefix(lower, "refs/remotes/") || lower == "head" {
		return fmt.Errorf("%w: %q is a moving pointer, not a release", ErrMutableRef, ref)
	}
	// A fully qualified tag is fine and is reduced to its short form by
	// NormalizeTag; anything else fully qualified is not a tag.
	if strings.HasPrefix(lower, "refs/") && !strings.HasPrefix(lower, "refs/tags/") {
		return fmt.Errorf("%w: %q is not a tag ref", ErrMutableRef, ref)
	}
	short := NormalizeTag(ref)
	if branchLikeRefs[strings.ToLower(short)] {
		return fmt.Errorf("%w: %q is a branch name. AO installs from a tagged commit, never from "+
			"whatever a branch points at today", ErrMutableRef, ref)
	}
	if !tagRe.MatchString(short) {
		return invalidSourcef("tag %q is not a plain tag name", ref)
	}
	return nil
}

// CheckImmutableRef is the exported form of the branch refusal, for the
// configuration and request paths that accept a ref from a person.
func CheckImmutableRef(ref string) error { return checkImmutableRefName(ref) }

// NormalizeTag reduces "refs/tags/v1.2.3" to "v1.2.3" and leaves a short tag
// alone. One spelling, everywhere: two forms of the same tag would be two rows
// in the moved-tag ledger and neither would ever notice the other.
func NormalizeTag(ref string) string {
	trimmed := strings.TrimSpace(ref)
	return strings.TrimPrefix(trimmed, "refs/tags/")
}

// cleanRepoPath reduces a package subdirectory to a slash-separated relative
// path, or refuses it.
//
// It is the same shape as safeArtifactPath and it exists separately because it
// runs on CONFIGURATION rather than on an archive entry: a path that traverses
// upward here would make AO ask the host for a tree outside the repository,
// which is a request AO must never build rather than an entry AO must never
// unpack.
func cleanRepoPath(raw string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", nil
	}
	if strings.Contains(trimmed, "\\") || strings.ContainsRune(trimmed, 0) {
		return "", invalidSourcef("path %q is not a slash-separated relative path", raw)
	}
	segments := strings.Split(trimmed, "/")
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "", ".":
			return "", invalidSourcef("path %q has an empty segment", raw)
		case "..":
			return "", invalidSourcef("path %q traverses above the repository root", raw)
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/"), nil
}

// SameRelease reports whether two sources name the same immutable thing.
//
// Only the repository and the commit are compared. The tag is deliberately
// excluded: a release re-tagged under a second name is the SAME bytes, and a
// comparison that included the tag would call it a different release. The
// reverse case -- one tag, two commits -- is what MovedFrom answers, and it is
// the dangerous one.
func (g GitSource) SameRelease(other GitSource) bool {
	return g.Provider == other.Provider &&
		strings.EqualFold(g.Owner, other.Owner) &&
		strings.EqualFold(g.Repository, other.Repository) &&
		g.Commit == other.Commit
}

// TagMoved reports whether these two sources share a tag and disagree about
// the commit behind it.
//
// That is the whole of the moved-tag attack, and it is a fact about two
// OBSERVATIONS rather than about either one of them: neither source is wrong
// on its own, and only a caller holding both can see it.
func (g GitSource) TagMoved(previous GitSource) bool {
	if g.Tag == "" || previous.Tag == "" || g.Tag != previous.Tag {
		return false
	}
	if !strings.EqualFold(g.Owner, previous.Owner) ||
		!strings.EqualFold(g.Repository, previous.Repository) {
		return false
	}
	return g.Commit != "" && previous.Commit != "" && g.Commit != previous.Commit
}

// Describe renders the source for an audit line and a refusal message.
func (g GitSource) Describe() string {
	if !g.Declared() {
		return ""
	}
	parts := []string{string(g.Provider) + " " + g.Slug()}
	if g.Tag != "" {
		parts = append(parts, "tag "+g.Tag)
	}
	if g.Commit != "" {
		parts = append(parts, "commit "+g.Commit)
	}
	if g.Path != "" {
		parts = append(parts, "path "+g.Path)
	}
	if g.Visibility != "" {
		parts = append(parts, g.Visibility)
	}
	return strings.Join(parts, ", ")
}
