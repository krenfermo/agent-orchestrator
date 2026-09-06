package httpd

import (
	"net/http"
	"strings"
	"testing"
)

// p4f_github_e2e_test.go -- GitHub intelligence across an organization
// boundary, over real HTTP against the real router.
//
// Repository metadata is exactly the kind of thing that must not leak across
// tenants: the repository name alone tells somebody what another organization
// is building. So the property is proved on this route specifically rather
// than inferred from P4-C, and it is proved in the shape that matters -- a
// foreign project and a guessed one must be indistinguishable, or the
// difference IS the oracle.

const githubRoutes = "/github"

func TestP4FGitHubIntelligenceIsInvisibleAcrossOrganizations(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	for _, route := range []string{
		githubRoutes,
		githubRoutes + "?branch=feat/secret-project",
		githubRoutes + "?issue=1",
		githubRoutes + "?refresh=true",
	} {
		path := "/api/v1/projects/project-b" + route
		status, body := w.do(http.MethodGet, path, a, "")
		if status != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 across an organization boundary (%s)", path, status, body)
		}
		// A 501 here would say "the feature is not wired", which is a fact
		// about the daemon the caller is entitled to only for projects they can
		// reach. Authorization runs in front of the handler for exactly this.
		if strings.Contains(body, "NOT_IMPLEMENTED") {
			t.Fatalf("GET %s leaked whether the feature is wired: %s", path, body)
		}
		// Nothing about the repository may appear in a refusal.
		for _, leak := range []string{"github.com", "originUrl", "defaultBranch"} {
			if strings.Contains(body, leak) {
				t.Fatalf("GET %s leaked repository metadata (%q): %s", path, leak, body)
			}
		}
	}
}

func TestP4FGitHubGuessedProjectIDsAnswerLikeForeignOnes(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	foreign, foreignBody := w.do(http.MethodGet, "/api/v1/projects/project-b"+githubRoutes, a, "")
	guessed, guessedBody := w.do(http.MethodGet, "/api/v1/projects/no-such-project"+githubRoutes, a, "")
	if foreign != guessed {
		t.Fatalf("answered %d for a foreign project and %d for a nonexistent one; the difference is an oracle",
			foreign, guessed)
	}
	// Compared with the per-request id removed: that field is unique by
	// design, and everything else in the envelope must match exactly.
	if withoutRequestID(foreignBody) != withoutRequestID(guessedBody) {
		t.Fatalf("bodies differ between a foreign and a nonexistent project:\n%s\nvs\n%s", foreignBody, guessedBody)
	}
}

func withoutRequestID(body string) string {
	idx := strings.Index(body, `"requestId"`)
	if idx < 0 {
		return body
	}
	return body[:idx]
}

// The other half of the proof: inside the caller's own organization the gate
// passes and the request reaches the handler, so the 404s above are
// authorization rather than a route that is simply missing.
func TestP4FGitHubIsReachableInsideTheOwnOrganization(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	path := "/api/v1/projects/project-a" + githubRoutes
	status, body := w.do(http.MethodGet, path, a, "")
	if status == http.StatusNotFound {
		t.Fatalf("GET %s = 404 inside the caller's own organization (%s)", path, body)
	}
}
