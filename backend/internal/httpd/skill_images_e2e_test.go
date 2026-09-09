package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// skill_images_e2e_test.go -- the administrative surface for the image trust
// root, over the real router with the real permission gate.
//
// The question every test here asks is the same one: can somebody who should
// not be able to decide what this installation executes, decide it anyway.

const e2eDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func approveBody(digest string) string {
	return `{"tenantId":"default","projectId":"medusa","skillId":"security-audit",` +
		`"version":"1.2.0","modeId":"static-code","tool":"ao.static-scan/v1",` +
		`"reference":"alpine","digest":"` + digest + `",` +
		`"note":"pulled by hand and diffed against the upstream manifest","confirm":true}`
}

// Requirement: only settings.manage may approve. A project administrator is
// deliberately NOT enough -- deciding that this installation will execute
// somebody's bytes is an installation-level act however narrow the scope.
func TestSkillImages_ApprovalIsGatedOnSettingsManage(t *testing.T) {
	w := newSkillsWorld(t)
	body := approveBody(e2eDigest)

	w.expect(http.MethodPost, "/api/v1/skills/images", w.login("member"), body, http.StatusForbidden)
	w.expect(http.MethodPost, "/api/v1/skills/images", w.login("viewer"), body, http.StatusForbidden)
	w.expect(http.MethodPost, "/api/v1/skills/images", nil, body, http.StatusUnauthorized)

	// Even a project ADMINISTRATOR on medusa cannot approve: the gate is
	// installation-wide, not project-wide.
	w.grantProjectRole(w.member, "medusa", "admin")
	w.expect(http.MethodPost, "/api/v1/skills/images", w.login("member"), body, http.StatusForbidden)

	ownerCookie := w.login("owner")
	w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, body, http.StatusCreated)

	// And nothing the refused calls sent was recorded.
	list := w.expect(http.MethodGet, "/api/v1/skills/images", ownerCookie, "", http.StatusOK)
	if strings.Count(list, e2eDigest) != 1 {
		t.Fatalf("approvals = %s", list)
	}
}

// The listing carries the trust model and the revocation policy in the response
// itself. A client rendering this list is where somebody decides whether to
// trust it, so the words that qualify it travel with the data.
func TestSkillImages_ListingCarriesTheTrustModelAndTheRevocationPolicy(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, approveBody(e2eDigest), http.StatusCreated)

	body := w.expect(http.MethodGet, "/api/v1/skills/images", ownerCookie, "", http.StatusOK)
	var out struct {
		Approvals []struct {
			ID             string `json:"id"`
			Digest         string `json:"digest"`
			ApprovedBy     string `json:"approvedBy"`
			Note           string `json:"note"`
			Active         bool   `json:"active"`
			InactiveReason string `json:"inactiveReason"`
		} `json:"approvals"`
		TrustModel       string `json:"trustModel"`
		RevocationPolicy string `json:"revocationPolicy"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(out.Approvals) != 1 || out.Approvals[0].Digest != e2eDigest {
		t.Fatalf("approvals = %+v", out.Approvals)
	}
	if !out.Approvals[0].Active || out.Approvals[0].InactiveReason != "" {
		t.Fatalf("a fresh approval reads as inactive: %+v", out.Approvals[0])
	}
	if out.Approvals[0].ApprovedBy == "" || out.Approvals[0].Note == "" {
		t.Fatalf("the approval does not say who or why: %+v", out.Approvals[0])
	}
	// The trust model is stated, and it says what an approval is NOT.
	for _, must := range []string{"NOT a publisher signature", "pulls nothing"} {
		if !strings.Contains(out.TrustModel, must) {
			t.Fatalf("trustModel = %q, missing %q", out.TrustModel, must)
		}
	}
	// The revocation policy states the non-promises, not just the promise.
	for _, must := range []string{
		"stops new executions immediately",
		"does NOT stop a container that is already running",
		"does NOT recall secrets already delivered",
	} {
		if !strings.Contains(out.RevocationPolicy, must) {
			t.Fatalf("revocationPolicy is missing %q", must)
		}
	}
}

// Requirement: approving requires an explicit confirmation, and it is not a
// default. A form that submitted without it must be refused, not accepted.
func TestSkillImages_ApprovalRequiresExplicitConfirmation(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	unconfirmed := strings.Replace(approveBody(e2eDigest), `"confirm":true`, `"confirm":false`, 1)

	body := w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, unconfirmed, http.StatusBadRequest)
	if !strings.Contains(body, "SKILL_IMAGE_APPROVAL_UNCONFIRMED") {
		t.Fatalf("body = %s", body)
	}
	// Omitting the field entirely is the same refusal: absence is not consent.
	omitted := strings.Replace(approveBody(e2eDigest), `,"confirm":true`, "", 1)
	w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, omitted, http.StatusBadRequest)

	list := w.expect(http.MethodGet, "/api/v1/skills/images", ownerCookie, "", http.StatusOK)
	if strings.Contains(list, e2eDigest) {
		t.Fatalf("an unconfirmed approval was recorded: %s", list)
	}
}

// A tag is not a digest, and the surface refuses it rather than resolving it.
func TestSkillImages_RefusesATagAndAnAmbiguousScope(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")

	for name, body := range map[string]string{
		"a tag":            approveBody("alpine:3.19"),
		"latest":           approveBody("latest"),
		"a short digest":   approveBody("sha256:abc"),
		"a wildcard scope": strings.Replace(approveBody(e2eDigest), `"version":"1.2.0"`, `"version":"1.*"`, 1),
		"no mode":          strings.Replace(approveBody(e2eDigest), `"modeId":"static-code"`, `"modeId":""`, 1),
		"no note":          strings.Replace(approveBody(e2eDigest), `"note":"pulled by hand and diffed against the upstream manifest"`, `"note":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			code, out := w.do(http.MethodPost, "/api/v1/skills/images", ownerCookie, body)
			if code == http.StatusCreated {
				t.Fatalf("%s was accepted: %s", name, out)
			}
		})
	}
	list := w.expect(http.MethodGet, "/api/v1/skills/images", ownerCookie, "", http.StatusOK)
	if !strings.Contains(list, `"approvals":[]`) {
		t.Fatalf("something was recorded: %s", list)
	}
}

// Revoking is gated the same way as approving, and it stays visible afterwards
// with the reason it is no longer usable.
func TestSkillImages_RevocationIsGatedAndStaysVisible(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	created := w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie,
		approveBody(e2eDigest), http.StatusCreated)
	var approval struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created), &approval); err != nil || approval.ID == "" {
		t.Fatalf("decode: %v\n%s", err, created)
	}
	path := "/api/v1/skills/images/" + approval.ID

	// A permission that cannot approve cannot un-approve either.
	w.expect(http.MethodDelete, path, w.login("member"), "", http.StatusForbidden)
	w.expect(http.MethodDelete, path, w.login("viewer"), "", http.StatusForbidden)
	w.expect(http.MethodDelete, path, nil, "", http.StatusUnauthorized)

	w.expect(http.MethodDelete, path, ownerCookie, "", http.StatusOK)
	// Twice is a conflict rather than a silent success: an operator needs to
	// know whether their action did anything.
	w.expect(http.MethodDelete, path, ownerCookie, "", http.StatusConflict)
	// Something that was never approved is a not-found.
	w.expect(http.MethodDelete, "/api/v1/skills/images/skimg-nope", ownerCookie, "", http.StatusNotFound)

	body := w.expect(http.MethodGet, "/api/v1/skills/images", ownerCookie, "", http.StatusOK)
	var out struct {
		Approvals []struct {
			Active         bool   `json:"active"`
			RevokedAt      string `json:"revokedAt"`
			InactiveReason string `json:"inactiveReason"`
		} `json:"approvals"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Approvals) != 1 {
		t.Fatalf("the revoked approval vanished from the history: %s", body)
	}
	if out.Approvals[0].Active || out.Approvals[0].RevokedAt == "" {
		t.Fatalf("approval = %+v", out.Approvals[0])
	}
	if !strings.Contains(out.Approvals[0].InactiveReason, "revoked") {
		t.Fatalf("inactiveReason = %q; expired and revoked must stay distinguishable",
			out.Approvals[0].InactiveReason)
	}
}

// Re-approving the same scope REPLACES. Two digests for one scope would make
// "which image may this run" a question with two answers.
func TestSkillImages_ReApprovingReplaces(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	second := "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, approveBody(e2eDigest), http.StatusCreated)
	w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, approveBody(second), http.StatusCreated)

	body := w.expect(http.MethodGet, "/api/v1/skills/images", ownerCookie, "", http.StatusOK)
	if strings.Count(body, `"digest":"`) != 1 {
		t.Fatalf("re-approval accumulated: %s", body)
	}
	if !strings.Contains(body, second) || strings.Contains(body, e2eDigest) {
		t.Fatalf("the replacement did not take: %s", body)
	}
}

// The surface approves; it does not run. There is deliberately no route that
// does both, because one click doing both would collapse two decisions a
// reviewer is supposed to make separately.
func TestSkillImages_ApprovingIsNotRunning(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	w.expect(http.MethodPost, "/api/v1/skills/images", ownerCookie, approveBody(e2eDigest), http.StatusCreated)

	// The skill is not even installed, let alone activated. An approval changed
	// nothing about that.
	list := w.expect(http.MethodGet, "/api/v1/skills", ownerCookie, "", http.StatusOK)
	if strings.Contains(list, `"id":"security-audit"`) {
		t.Fatalf("approving an image installed a skill: %s", list)
	}
	// And there is no route that would run it from here.
	for _, path := range []string{
		"/api/v1/skills/images/run",
		"/api/v1/skills/images/" + e2eDigest + "/run",
	} {
		code, _ := w.do(http.MethodPost, path, ownerCookie, "{}")
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Fatalf("%s answered %d; approving must not be a way to execute", path, code)
		}
	}
}
