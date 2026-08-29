package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// workflow_placement_write_test.go — P1-E §B/§C at the CLI boundary.
//
// Two properties, and they are the ones an operator's safety rests on:
//
//   - what the command SENDS is exactly what they typed, so a placement is
//     never quietly substituted between the terminal and the daemon;
//   - what the command PRINTS distinguishes "this took effect" from "this was
//     recorded and changes nothing yet", because an operator who reads the
//     second as the first will walk away believing a run has moved.

func TestWorkflowPlacementOverrideSendsTheRequestedPlacementVerbatim(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendPrimaryRequest(&requests, r)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-1/placement/override" {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = io.WriteString(w, `{"override":{"placement":"direct_branch","requestedBy":"operator","state":"requested"},`+
				`"appliesAtFreeze":true,"requiresTransition":false}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "placement", "override", "wf-1",
		"--placement", "direct_branch", "--reason", "the worktree cannot be created here")
	if err != nil {
		t.Fatalf("placement override failed: %v stderr=%s", err, errOut)
	}
	if body["placement"] != "direct_branch" {
		t.Fatalf("placement = %q, want the value typed, sent verbatim", body["placement"])
	}
	if body["reason"] != "the worktree cannot be created here" {
		t.Fatalf("reason = %q, want it recorded verbatim", body["reason"])
	}
	if !strings.Contains(out, "applies:  at the freeze") {
		t.Fatalf("output does not say the request will actually be used:\n%s", out)
	}
	want := []string{"POST /api/v1/workflows/wf-1/placement/override"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%#v want %#v", requests, want)
	}
}

// The case an operator must not misread: a placement is already frozen, so the
// request changed nothing. The command has to say so and name the next step.
func TestWorkflowPlacementOverrideSaysWhenItChangedNothing(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-2/placement/override" {
			_, _ = io.WriteString(w, `{"override":{"placement":"isolated_worktree","requestedBy":"operator","state":"requested"},`+
				`"appliesAtFreeze":false,"requiresTransition":true,`+
				`"currentPlacement":{"type":"direct_branch","placementGeneration":1,"state":"active"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "placement", "override", "wf-2", "--placement", "isolated_worktree")
	if err != nil {
		t.Fatalf("placement override failed: %v stderr=%s", err, errOut)
	}
	for _, want := range []string{"NOT YET", "direct_branch", "gen 1", "transition"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q; an operator could read this as having taken effect:\n%s", want, out)
		}
	}
}

func TestWorkflowPlacementTransitionSendsItsGuardsAndReportsTheProof(t *testing.T) {
	cfg := setConfigEnv(t)
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-3/placement/transition" {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"applied":true,"alreadyApplied":false,`+
				`"transition":{"fromGeneration":1,"toGeneration":2,"fromType":"isolated_worktree",`+
				`"toType":"direct_branch","requestedBy":"operator","state":"applied"},`+
				`"quiescence":{"quiesced":true,"digest":"run_state=running sha256=abc123"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "placement", "transition", "wf-3",
		"--placement", "direct_branch", "--expect-state", "active", "--expect-generation", "1")
	if err != nil {
		t.Fatalf("placement transition failed: %v stderr=%s", err, errOut)
	}
	// The guards are the whole reason a transition is safe to retry from a page
	// the operator has had open for a while; dropping either silently would
	// turn a refusal into a move.
	if body["expectedState"] != "active" {
		t.Fatalf("expectedState = %v, want the asserted state sent", body["expectedState"])
	}
	if body["expectedGeneration"] != float64(1) {
		t.Fatalf("expectedGeneration = %v, want 1", body["expectedGeneration"])
	}
	for _, want := range []string{"gen 1 isolated_worktree -> gen 2 direct_branch", "authorized by", "proof:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
}

// A repeated transition reports what already happened. It must not read as a
// second move, and it must not read as a failure.
func TestWorkflowPlacementTransitionReportsAnAlreadyAppliedTransition(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-4/placement/transition" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"applied":false,"alreadyApplied":true,`+
				`"transition":{"fromGeneration":1,"toGeneration":2,"state":"applied"},`+
				`"quiescence":{"quiesced":true}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "placement", "transition", "wf-4", "--placement", "direct_branch")
	if err != nil {
		t.Fatalf("a repeated transition must not be an error: %v stderr=%s", err, errOut)
	}
	if !strings.Contains(out, "already done") {
		t.Fatalf("output does not report the transition that already happened:\n%s", out)
	}
}

// A refusal names the authority. It is a 409 with the reason in the message, and
// the CLI must surface that rather than a generic conflict.
func TestWorkflowPlacementTransitionSurfacesTheRefusingAuthority(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/wf-5/placement/transition" {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"conflict","code":"PLACEMENT_TRANSITION_REFUSED",`+
				`"message":"held_capacity_claim: an authority still owns the current placement"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)

	_, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
		"workflow", "placement", "transition", "wf-5", "--placement", "auto")
	if err == nil {
		t.Fatal("a refused transition exited zero; the operator would think it moved")
	}
	if !strings.Contains(err.Error()+errOut, "held_capacity_claim") {
		t.Fatalf("the refusing authority is not in the error: %v / %s", err, errOut)
	}
}

// An unreadable placement is refused before the round trip, so a typo costs a
// usage error rather than a request. It is never coerced to `auto`.
func TestWorkflowPlacementCommandsRefuseAnUnknownPlacement(t *testing.T) {
	for _, sub := range []string{"override", "transition"} {
		t.Run(sub, func(t *testing.T) {
			cfg := setConfigEnv(t)
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				appendPrimaryRequest(&requests, r)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)

			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }},
				"workflow", "placement", sub, "wf-6", "--placement", "somewhere_else")
			if err == nil {
				t.Fatal("an unknown placement was accepted")
			}
			// The placement route is never reached: the value is refused before
			// the request, so a typo costs a usage error rather than a round
			// trip -- and it is never coerced to `auto` on the way.
			for _, req := range requests {
				if strings.Contains(req, "/placement/") {
					t.Fatalf("an unknown placement reached the daemon: %s", req)
				}
			}
			if !strings.Contains(err.Error(), "auto, direct_branch, isolated_worktree") {
				t.Fatalf("the refusal does not name the accepted values: %v", err)
			}
		})
	}
}
