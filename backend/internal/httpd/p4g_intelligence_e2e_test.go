package httpd

import (
	"net/http"
	"strings"
	"testing"
)

// p4g_intelligence_e2e_test.go -- Project Intelligence across an organization
// boundary, over real HTTP against the real router.
//
// The property is the one P4-G section 14 asks for, and it is worth proving on
// these routes specifically rather than inferring it from P4-C: a person who
// cannot reach a project must not be able to learn anything about it through
// the intelligence surface either -- not its architecture, not a symbol, not
// how much context it would produce -- and a guessed project id must answer
// exactly as a non-existent one does.
//
// Note what these assertions do NOT depend on: whether the intelligence
// service is wired at all. Authorization runs in front of the handler, so an
// unwired build answers 501 to somebody entitled to ask and 404 to somebody who
// is not. That ordering is the thing being tested; if it ever inverted, a
// not-implemented response would become an existence oracle.

// intelligenceRoutes are every read the Project Intelligence surface offers.
var intelligenceRoutes = []string{
	"/intelligence",
	"/intelligence/architecture",
	"/intelligence/graph?symbol=Anything",
	"/intelligence/search?q=permisos",
	"/intelligence/context?role=planner",
}

func TestP4GIntelligenceIsInvisibleAcrossOrganizations(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	for _, route := range intelligenceRoutes {
		path := "/api/v1/projects/project-b" + route
		status, body := w.do(http.MethodGet, path, a, "")
		if status != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 across an organization boundary (%s)", path, status, body)
		}
		if strings.Contains(body, "NOT_IMPLEMENTED") {
			t.Fatalf("GET %s leaked whether the feature is wired: %s", path, body)
		}
	}
}

// The two routes that cost real work are gated harder -- project.manage rather
// than memory.read -- and must be equally invisible across the boundary.
func TestP4GIntelligenceWritesAreInvisibleAcrossOrganizations(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	for _, route := range []string{"/intelligence/sync", "/intelligence/rebuild"} {
		path := "/api/v1/projects/project-b" + route
		status, body := w.do(http.MethodPost, path, a, "")
		if status != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404 across an organization boundary (%s)", path, status, body)
		}
	}
}

// A project id nobody has must answer exactly as one in another organization
// does. If these two differed, the difference would be the oracle.
func TestP4GIntelligenceGuessedProjectIDsAnswerLikeForeignOnes(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	for _, route := range intelligenceRoutes {
		foreign, _ := w.do(http.MethodGet, "/api/v1/projects/project-b"+route, a, "")
		guessed, _ := w.do(http.MethodGet, "/api/v1/projects/no-such-project"+route, a, "")
		if foreign != guessed {
			t.Fatalf("%s answered %d for a foreign project and %d for a nonexistent one; the difference is an oracle",
				route, foreign, guessed)
		}
	}
}

// Inside the caller's own organization the gate passes and the request reaches
// the handler. This is the other half of the proof: the 404s above are
// authorization, not a route that is simply missing.
func TestP4GIntelligenceIsReachableInsideTheOwnOrganization(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	for _, route := range intelligenceRoutes {
		path := "/api/v1/projects/project-a" + route
		status, body := w.do(http.MethodGet, path, a, "")
		if status == http.StatusNotFound {
			t.Fatalf("GET %s = 404 inside the caller's own organization (%s)", path, body)
		}
	}
}
