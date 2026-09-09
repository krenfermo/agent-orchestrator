package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The trust root's CLI. Each test asserts the WIRE CALL, not only the rendered
// text: the point of these commands is which decision reaches the daemon, and a
// pretty output over a wrong body would be the worst outcome.

func skillImagesCLIServer(t *testing.T, capture *skillsCapture, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capture.method = r.Method
		capture.path = r.URL.RequestURI()
		capture.body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func skillImagesCLI(t *testing.T, status int, body string) (*skillsCapture, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	capture := &skillsCapture{}
	srv := skillImagesCLIServer(t, capture, status, body)
	writeRunFileFor(t, cfg, srv)
	return capture, Deps{ProcessAlive: func(int) bool { return true }}
}

const approvalListBody = `{"approvals":[
 {"id":"skimg-1","tenantId":"default","projectId":"medusa","skillId":"security-audit",
  "version":"1.2.0","modeId":"static-code","tool":"ao.static-scan/v1","reference":"alpine",
  "digest":"sha256:aaaa","approvedBy":"admin","approvedAt":"2026-09-09T10:00:00Z",
  "note":"diffed against upstream","active":true},
 {"id":"skimg-2","tenantId":"default","projectId":"medusa","skillId":"security-audit",
  "version":"1.1.0","modeId":"static-code","tool":"ao.static-scan/v1","reference":"alpine",
  "digest":"sha256:bbbb","approvedBy":"admin","approvedAt":"2026-09-01T10:00:00Z",
  "revokedAt":"2026-09-05T10:00:00Z","note":"superseded","active":false,
  "inactiveReason":"revoked"}],
 "trustModel":"An approval is not a publisher signature.",
 "revocationPolicy":"Revoking stops new executions."}`

// Revoked and expired approvals must appear. A list that showed only the active
// ones would answer "what did we approve" with "what is approved right now".
func TestSkillImagesList_ShowsRevokedOnesToo(t *testing.T) {
	capture, deps := skillImagesCLI(t, 0, approvalListBody)

	out, errOut, err := executeCLI(t, deps, "skills", "images", "list")
	if err != nil {
		t.Fatalf("list: %v (%s)", err, errOut)
	}
	if capture.path != "/api/v1/skills/images" {
		t.Fatalf("path = %q", capture.path)
	}
	for _, want := range []string{
		"skimg-1", "active",
		"skimg-2", "INACTIVE (revoked)",
		"sha256:aaaa", "default/medusa security-audit@1.2.0",
		"mode=static-code", "tool=ao.static-scan/v1",
		"by      admin",
		// The non-promises travel with the listing, not only with the docs.
		"not a publisher signature",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output is missing %q:\n%s", want, out)
		}
	}
}

func TestSkillImagesList_ScopesToOneProject(t *testing.T) {
	capture, deps := skillImagesCLI(t, 0, `{"approvals":[]}`)

	out, _, err := executeCLI(t, deps, "skills", "images", "list", "--project", "medusa")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if capture.path != "/api/v1/skills/images?projectId=medusa" {
		t.Fatalf("path = %q", capture.path)
	}
	// An empty catalog says what that MEANS, not just that it is empty.
	if !strings.Contains(out, "nothing may execute") {
		t.Fatalf("empty listing should say nothing may execute; got %q", out)
	}
}

func TestSkillImagesApprove_SendsTheWholeScopeAndTheConfirmation(t *testing.T) {
	capture, deps := skillImagesCLI(t, http.StatusOK,
		`{"approval":{"id":"skimg-9","tenantId":"default","projectId":"medusa",
		  "skillId":"security-audit","version":"1.2.0","modeId":"static-code",
		  "tool":"ao.static-scan/v1","digest":"sha256:cccc","approvedBy":"admin",
		  "approvedAt":"2026-09-09T11:00:00Z","active":true}}`)

	out, errOut, err := executeCLI(t, deps, "skills", "images", "approve",
		"--tenant", "default", "--project", "medusa", "--skill", "security-audit",
		"--version", "1.2.0", "--mode", "static-code", "--tool", "ao.static-scan/v1",
		"--reference", "alpine", "--digest", "sha256:cccc",
		"--note", "diffed against upstream", "--expires-in", "24h", "--confirm")
	if err != nil {
		t.Fatalf("approve: %v (%s)", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/skills/images" {
		t.Fatalf("%s %s", capture.method, capture.path)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	for k, want := range map[string]any{
		"tenantId": "default", "projectId": "medusa", "skillId": "security-audit",
		"version": "1.2.0", "modeId": "static-code", "tool": "ao.static-scan/v1",
		"reference": "alpine", "digest": "sha256:cccc",
		"note": "diffed against upstream", "confirm": true,
		// 24h, expressed in the unit the wire uses.
		"expiresInSeconds": float64(86400),
	} {
		if sent[k] != want {
			t.Fatalf("body[%s] = %v, want %v", k, sent[k], want)
		}
	}
	if !strings.Contains(out, "approved skimg-9") {
		t.Fatalf("output = %q", out)
	}
}

// No expiry is the documented default for a base image. It must be OMITTED, not
// sent as zero: a zero the daemon read as "expires now" would be a silent
// difference between "no expiry" and "already expired".
func TestSkillImagesApprove_OmitsExpiryWhenNoneIsGiven(t *testing.T) {
	capture, deps := skillImagesCLI(t, http.StatusOK, `{"approval":{"id":"skimg-9"}}`)

	if _, errOut, err := executeCLI(t, deps, "skills", "images", "approve",
		"--tenant", "default", "--project", "medusa", "--skill", "security-audit",
		"--version", "1.2.0", "--mode", "static-code", "--tool", "ao.static-scan/v1",
		"--reference", "alpine", "--digest", "sha256:cccc",
		"--note", "checked", "--confirm"); err != nil {
		t.Fatalf("approve: %v (%s)", err, errOut)
	}
	if strings.Contains(capture.body, "expiresInSeconds") {
		t.Fatalf("expiry must be omitted when none was given: %s", capture.body)
	}
}

// Every scope field is required, and so is the confirmation. These are usage
// errors: they must not reach the daemon at all.
func TestSkillImagesApprove_RefusesLocallyBeforeAnyRequest(t *testing.T) {
	full := []string{
		"--tenant", "default", "--project", "medusa", "--skill", "security-audit",
		"--version", "1.2.0", "--mode", "static-code", "--tool", "ao.static-scan/v1",
		"--reference", "alpine", "--digest", "sha256:cccc", "--note", "checked",
	}
	drop := func(flag string) []string {
		out := make([]string, 0, len(full))
		for i := 0; i < len(full); i += 2 {
			if full[i] == flag {
				continue
			}
			out = append(out, full[i], full[i+1])
		}
		return append(out, "--confirm")
	}

	for _, missing := range []string{
		"--tenant", "--project", "--skill", "--version",
		"--mode", "--tool", "--reference", "--digest", "--note",
	} {
		t.Run("missing "+missing, func(t *testing.T) {
			capture, deps := skillImagesCLI(t, 0, `{}`)
			args := append([]string{"skills", "images", "approve"}, drop(missing)...)
			if _, _, err := executeCLI(t, deps, args...); err == nil {
				t.Fatalf("approved with %s missing", missing)
			}
			if strings.Contains(capture.path, "skills/images") {
				t.Fatalf("an approval reached the daemon anyway: %s %s", capture.method, capture.path)
			}
		})
	}

	t.Run("no confirmation", func(t *testing.T) {
		capture, deps := skillImagesCLI(t, 0, `{}`)
		args := append([]string{"skills", "images", "approve"}, full...)
		if _, _, err := executeCLI(t, deps, args...); err == nil {
			t.Fatal("approved without --confirm")
		}
		if strings.Contains(capture.path, "skills/images") {
			t.Fatalf("an approval reached the daemon anyway: %s %s", capture.method, capture.path)
		}
	})
}

// A digest this host does not hold is refused by the DAEMON, and the CLI has to
// surface that rather than reporting success.
func TestSkillImagesApprove_SurfacesTheDaemonsRefusal(t *testing.T) {
	_, deps := skillImagesCLI(t, http.StatusBadRequest,
		`{"code":"SKILL_IMAGE_NOT_PRESENT",
		  "message":"sha256:cccc is not on this host, and AO does not pull to find out"}`)

	_, errOut, err := executeCLI(t, deps, "skills", "images", "approve",
		"--tenant", "default", "--project", "medusa", "--skill", "security-audit",
		"--version", "1.2.0", "--mode", "static-code", "--tool", "ao.static-scan/v1",
		"--reference", "alpine", "--digest", "sha256:cccc", "--note", "checked", "--confirm")
	if err == nil {
		t.Fatal("a refused approval reported success")
	}
	if !strings.Contains(err.Error()+errOut, "not on this host") {
		t.Fatalf("the refusal must reach the operator: %v %s", err, errOut)
	}
}

func TestSkillImagesRevoke_RequiresConfirmationAndStatesTheNonPromises(t *testing.T) {
	t.Run("without confirmation", func(t *testing.T) {
		capture, deps := skillImagesCLI(t, 0, `{}`)
		if _, _, err := executeCLI(t, deps, "skills", "images", "revoke", "skimg-1"); err == nil {
			t.Fatal("revoked without --confirm")
		}
		if strings.Contains(capture.path, "skills/images") {
			t.Fatalf("a revocation reached the daemon anyway: %s %s", capture.method, capture.path)
		}
	})

	t.Run("with confirmation", func(t *testing.T) {
		capture, deps := skillImagesCLI(t, 0, `{"ok":true}`)
		out, errOut, err := executeCLI(t, deps, "skills", "images", "revoke", "skimg-1", "--confirm")
		if err != nil {
			t.Fatalf("revoke: %v (%s)", err, errOut)
		}
		if capture.method != http.MethodDelete || capture.path != "/api/v1/skills/images/skimg-1" {
			t.Fatalf("%s %s", capture.method, capture.path)
		}
		// The non-promises must be stated where the operator reads the result,
		// not only in the docs they did not open.
		for _, want := range []string{"not killed", "not recalled"} {
			if !strings.Contains(out, want) {
				t.Fatalf("revoke output must state the non-promises; missing %q:\n%s", want, out)
			}
		}
	})
}
