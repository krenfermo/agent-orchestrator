package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// marketplaceCLIServer is a fake daemon answering the registry and marketplace
// routes. Each test asserts the wire call the CLI made as well as what it
// printed: a command that renders correctly and requests the wrong thing is
// still wrong.
func marketplaceCLIServer(t *testing.T, capture *skillsCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capture.method = r.Method
		capture.path = r.URL.RequestURI()
		capture.body = string(raw)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills/registries":
			_, _ = io.WriteString(w, `{"registries":[
				{"id":"ao-fixture","displayName":"AO Fixture","type":"local","location":"/srv/reg",
				 "enabled":true,"trustPolicy":"digest","trustPolicyEnforceable":true,"priority":10},
				{"id":"strict","displayName":"Strict","type":"local","location":"/srv/strict",
				 "enabled":true,"trustPolicy":"signed","trustPolicyEnforceable":false,"priority":20}
			],"trustModel":"Installing verifies INTEGRITY."}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/skills/registries/ao-fixture":
			_, _ = io.WriteString(w, `{"id":"ao-fixture","displayName":"AO Fixture","type":"local",
				"location":"/srv/reg","enabled":true,"trustPolicy":"digest",
				"trustPolicyEnforceable":true,"priority":10}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/skills/registries/strict":
			_, _ = io.WriteString(w, `{"id":"strict","displayName":"Strict","type":"local",
				"location":"/srv/strict","enabled":true,"trustPolicy":"signed",
				"trustPolicyEnforceable":false,"priority":10}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/skills/registries/ao-fixture":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills/marketplace":
			_, _ = io.WriteString(w, `{"releases":[
				{"registryId":"ao-fixture","skillId":"security-audit","name":"Security Audit",
				 "version":"0.2.0","publisher":"agent-orchestrator","description":"On-demand audit.",
				 "riskLevel":"critical","manifestDigest":"aa","artifactDigest":"bb",
				 "requestedCapabilities":["repo.read","report.write"],"executionModes":[],
				 "aoMinVersion":"0.11.0","compatibility":"compatible","publishedAt":"2026-01-01T00:00:00Z",
				 "trust":"unverified","trustExplanation":"Nothing has been checked.",
				 "installed":false,"installedVersion":"0.1.0","updateAvailable":true}
			],"notes":[{"registryId":"broken","reason":"registry.json is missing"}],
			"installNotice":"This installs the Skill but does not enable it on any project."}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills/marketplace/ao-fixture/security-audit":
			_, _ = io.WriteString(w, `{"release":
				{"registryId":"ao-fixture","registryName":"AO Fixture","skillId":"security-audit",
				 "name":"Security Audit","version":"0.1.0","publisher":"agent-orchestrator",
				 "description":"On-demand audit.","riskLevel":"critical",
				 "manifestDigest":"aa","artifactDigest":"bb",
				 "signatureFormat":"cosign","keyId":"kid-1",
				 "requestedCapabilities":["repo.read","net.egress"],
				 "executionModes":[{"id":"static-code","name":"Static","riskLevel":"medium",
				   "capabilities":["repo.read"]}],
				 "aoMinVersion":"0.11.0","compatibility":"compatible",
				 "publishedAt":"2026-01-01T00:00:00Z","trust":"unverified",
				 "trustExplanation":"AO has not fetched these bytes.","installed":false},
			 "versions":[
				{"skillId":"security-audit","version":"0.2.0","publishedAt":"2026-02-01T00:00:00Z",
				 "trust":"unverified","compatibility":"compatible","aoMinVersion":"0.11.0"},
				{"skillId":"security-audit","version":"0.1.0","publishedAt":"2026-01-01T00:00:00Z",
				 "trust":"revoked","revoked":true,"revocationReason":"key compromised",
				 "compatibility":"compatible","aoMinVersion":"0.11.0"}],
			 "installNotice":"This installs the Skill but does not enable it on any project."}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/skills/marketplace/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"install":{"id":"security-audit","version":"0.1.0","digest":"cc"},
				"origin":{"skillId":"security-audit","version":"0.1.0","registryId":"ao-fixture",
				 "publisher":"agent-orchestrator","artifactDigest":"bb","trust":"verified",
				 "trustExplanation":"AO fetched the bytes and computed both digests itself.",
				 "compatibility":"unknown"},
				"updated":false,"nextStep":"Installed. Choose a project to enable it."}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/skills/updates":
			_, _ = io.WriteString(w, `{"statuses":[
				{"skillId":"security-audit","version":"0.1.0",
				 "origin":{"registryId":"ao-fixture","trust":"revoked"},
				 "latestVersion":"","updateAvailable":false,"revokedNow":true,
				 "revocationReason":"key compromised"},
				{"skillId":"dependency-check","version":"1.0.0",
				 "origin":{"registryId":"ao-fixture","trust":"verified"},
				 "latestVersion":"1.2.0","updateAvailable":true,"revokedNow":false}
			],"revocationPolicy":"AO does NOT uninstall it."}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func marketplaceCLI(t *testing.T) (*skillsCapture, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	capture := &skillsCapture{}
	srv := marketplaceCLIServer(t, capture)
	writeRunFileFor(t, cfg, srv)
	return capture, Deps{ProcessAlive: func(int) bool { return true }}
}

// An empty registry list says what that MEANS. "No registries" alone reads
// like a setup step nobody got to.
func TestSkillRegistryList_RendersPolicyAndNamesAnUnenforceableOne(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "registry", "list")
	if err != nil {
		t.Fatalf("registry list: %v (%s)", err, errOut)
	}
	if capture.path != "/api/v1/skills/registries" {
		t.Fatalf("path = %q", capture.path)
	}
	if !strings.Contains(out, "ao-fixture") || !strings.Contains(out, "trust policy: digest") {
		t.Fatalf("output = %q", out)
	}
	// The strictest-looking setting must not read as if it were working.
	if !strings.Contains(out, "NOT ENFORCEABLE") {
		t.Fatalf("a signature-requiring registry was rendered as if it worked: %q", out)
	}
	if !strings.Contains(out, "Installing verifies INTEGRITY") {
		t.Fatalf("the daemon's trust model was not shown: %q", out)
	}
}

func TestSkillRegistryAdd_SendsTheConfigurationAndSaysNothingWasInstalled(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "registry", "add", "ao-fixture",
		"--name", "AO Fixture", "--location", "/srv/reg", "--priority", "10")
	if err != nil {
		t.Fatalf("registry add: %v (%s)", err, errOut)
	}
	if capture.method != http.MethodPut || capture.path != "/api/v1/skills/registries/ao-fixture" {
		t.Fatalf("%s %s", capture.method, capture.path)
	}
	var sent saveSkillRegistryRequest
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("decode body: %v (%s)", err, capture.body)
	}
	if sent.DisplayName != "AO Fixture" || sent.Location != "/srv/reg" ||
		sent.Type != "local" || sent.TrustPolicy != "digest" || !sent.Enabled {
		t.Fatalf("sent = %#v", sent)
	}
	if !strings.Contains(out, "nothing was installed") {
		t.Fatalf("output = %q", out)
	}
}

// Configuring a registry whose policy this build cannot satisfy must warn: it
// installs nothing, and an operator who is not told will think it is working.
func TestSkillRegistryAdd_WarnsAboutAnUnenforceablePolicy(t *testing.T) {
	_, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "registry", "add", "strict",
		"--name", "Strict", "--location", "/srv/strict", "--trust-policy", "signed")
	if err != nil {
		t.Fatalf("registry add: %v (%s)", err, errOut)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "nothing can be installed") {
		t.Fatalf("output = %q", out)
	}
}

func TestSkillRegistryAdd_RequiresNameAndLocation(t *testing.T) {
	_, deps := marketplaceCLI(t)

	if _, _, err := executeCLI(t, deps, "skills", "registry", "add", "x", "--location", "/srv"); err == nil {
		t.Fatal("a registry with no --name was accepted")
	}
	if _, _, err := executeCLI(t, deps, "skills", "registry", "add", "x", "--name", "X"); err == nil {
		t.Fatal("a registry with no --location was accepted")
	}
}

func TestSkillRegistryRemove_SaysProvenanceWasKept(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "registry", "remove", "ao-fixture")
	if err != nil {
		t.Fatalf("registry remove: %v (%s)", err, errOut)
	}
	if capture.method != http.MethodDelete || capture.path != "/api/v1/skills/registries/ao-fixture" {
		t.Fatalf("%s %s", capture.method, capture.path)
	}
	if !strings.Contains(out, "provenance were kept") {
		t.Fatalf("output = %q", out)
	}
}

func TestSkillsMarketplaceSearch_SendsFiltersAndNamesAnUnreadableRegistry(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "search", "security",
		"--capability", "repo.read", "--include-revoked", "--limit", "5")
	if err != nil {
		t.Fatalf("search: %v (%s)", err, errOut)
	}
	for _, want := range []string{"q=security", "capability=repo.read", "includeRevoked=true", "limit=5"} {
		if !strings.Contains(capture.path, want) {
			t.Fatalf("request %q is missing %q", capture.path, want)
		}
	}
	if !strings.Contains(out, "security-audit") || !strings.Contains(out, "update from 0.1.0") {
		t.Fatalf("output = %q", out)
	}
	// A search hashed nothing, and the rendering says so.
	if !strings.Contains(out, "trust=unverified") {
		t.Fatalf("output = %q", out)
	}
	// An unreadable registry is NAMED, not silently absent.
	if !strings.Contains(out, "registry broken could not be read") {
		t.Fatalf("an unreadable registry vanished from the output: %q", out)
	}
	if !strings.Contains(out, "does not enable it on any project") {
		t.Fatalf("the install notice was not shown: %q", out)
	}
}

func TestSkillsMarketplaceShow_RendersProvenanceAsAClaim(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "show", "security-audit",
		"--registry", "ao-fixture")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, errOut)
	}
	if capture.path != "/api/v1/skills/marketplace/ao-fixture/security-audit" {
		t.Fatalf("path = %q", capture.path)
	}
	for _, want := range []string{
		"security-audit@0.1.0", "publisher:", "capabilities:  repo.read net.egress",
		"mode static-code", "compatibility: compatible", "trust:         unverified",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q is missing %q", out, want)
		}
	}
	// A line that just printed "signature: present" would read as a check.
	if !strings.Contains(out, "CLAIMED; AO verifies no signature") {
		t.Fatalf("a declared signature was rendered as if AO checked it: %q", out)
	}
	// The version list is where somebody finds out a version was withdrawn.
	if !strings.Contains(out, "REVOKED: key compromised") {
		t.Fatalf("a withdrawn version was not marked: %q", out)
	}
}

func TestSkillsMarketplaceShow_RequiresARegistry(t *testing.T) {
	_, deps := marketplaceCLI(t)
	if _, _, err := executeCLI(t, deps, "skills", "marketplace", "show", "security-audit"); err == nil {
		t.Fatal("show without --registry was accepted")
	}
}

// There is no "latest": a version is required and exact.
func TestSkillsMarketplaceInstall_RequiresAnExactVersion(t *testing.T) {
	_, deps := marketplaceCLI(t)

	_, _, err := executeCLI(t, deps, "skills", "marketplace", "install", "security-audit",
		"--registry", "ao-fixture")
	if err == nil {
		t.Fatal("an install with no --version was accepted")
	}
	if !strings.Contains(err.Error(), "there is no latest") {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := executeCLI(t, deps, "skills", "marketplace", "install", "security-audit",
		"--version", "0.1.0"); err == nil {
		t.Fatal("an install with no --registry was accepted")
	}
}

func TestSkillsMarketplaceInstall_ReportsTrustAndTheNextStep(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "install", "security-audit",
		"--registry", "ao-fixture", "--version", "0.1.0")
	if err != nil {
		t.Fatalf("install: %v (%s)", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/skills/marketplace/install" {
		t.Fatalf("%s %s", capture.method, capture.path)
	}
	var sent installSkillReleaseRequest
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("decode body: %v (%s)", err, capture.body)
	}
	if sent.RegistryID != "ao-fixture" || sent.SkillID != "security-audit" ||
		sent.Version != "0.1.0" || sent.AsUpdate {
		t.Fatalf("sent = %#v", sent)
	}
	if !strings.Contains(out, "trust:     verified") {
		t.Fatalf("output = %q", out)
	}
	// The compatibility check not RUNNING is a different fact from it passing.
	if !strings.Contains(out, "compatibility check did not run") {
		t.Fatalf("an unknown compatibility verdict was not surfaced: %q", out)
	}
	if !strings.Contains(out, "Choose a project to enable it") {
		t.Fatalf("output = %q", out)
	}
}

func TestSkillsMarketplaceInstall_UpdateFlagIsSent(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	if _, errOut, err := executeCLI(t, deps, "skills", "marketplace", "install", "security-audit",
		"--registry", "ao-fixture", "--version", "0.2.0", "--update"); err != nil {
		t.Fatalf("install: %v (%s)", err, errOut)
	}
	var sent installSkillReleaseRequest
	if err := json.Unmarshal([]byte(capture.body), &sent); err != nil {
		t.Fatalf("decode body: %v (%s)", err, capture.body)
	}
	if !sent.AsUpdate {
		t.Fatalf("--update did not reach the daemon: %#v", sent)
	}
}

func TestSkillsMarketplaceUpdates_ShowsRevocationAndItsNonPromise(t *testing.T) {
	capture, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "updates")
	if err != nil {
		t.Fatalf("updates: %v (%s)", err, errOut)
	}
	if capture.path != "/api/v1/skills/updates" {
		t.Fatalf("path = %q", capture.path)
	}
	if !strings.Contains(out, "update available: 1.2.0") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, "REVOKED by the registry: key compromised") {
		t.Fatalf("output = %q", out)
	}
	// The non-promise matters as much as the promise.
	if !strings.Contains(out, "does NOT uninstall it") {
		t.Fatalf("the revocation policy was not shown: %q", out)
	}
}

// The marketplace has no verb that runs anything, and this reads the command
// tree rather than probing it: a negative claim ("nothing here executes") is
// not proved by one invocation failing.
//
// Running a skill stays where it was, under `ao skills run`, which needs the
// skill installed, activated on a project, its capabilities granted and an
// approved image for the exact scope. Adding a shortcut from a marketplace
// listing would collapse decisions that exist to be made separately.
func TestSkillsMarketplace_RegistersNoExecutionVerb(t *testing.T) {
	root := NewRootCommand(Deps{})
	find := func(path ...string) *cobra.Command {
		cur := root
		for _, name := range path {
			var next *cobra.Command
			for _, sub := range cur.Commands() {
				if sub.Name() == name {
					next = sub
					break
				}
			}
			if next == nil {
				t.Fatalf("command %v is not registered", path)
			}
			cur = next
		}
		return cur
	}
	forbidden := map[string]bool{"run": true, "exec": true, "execute": true, "dry-run": true}
	for _, group := range [][]string{{"skills", "marketplace"}, {"skills", "registry"}} {
		for _, sub := range find(group...).Commands() {
			if forbidden[sub.Name()] {
				t.Fatalf("%v registers %q; the marketplace must have no execution verb",
					group, sub.Name())
			}
		}
	}
	// And the verbs that DO exist are the four this phase built.
	var got []string
	for _, sub := range find("skills", "marketplace").Commands() {
		got = append(got, sub.Name())
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "install,search,show,updates" {
		t.Fatalf("marketplace verbs = %v", got)
	}
}

// offlineMarketplaceCLI is a daemon whose registry could not be reached: the
// releases are cached, and every field that says so is populated.
func offlineMarketplaceCLI(t *testing.T) Deps {
	t.Helper()
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/skills/marketplace":
			_, _ = io.WriteString(w, `{"releases":[
				{"registryId":"corp","skillId":"security-audit","name":"Security Audit",
				 "version":"0.2.0","publisher":"agent-orchestrator","description":"On-demand audit.",
				 "riskLevel":"critical","manifestDigest":"aa","artifactDigest":"bb",
				 "requestedCapabilities":["repo.read"],"executionModes":[],
				 "aoMinVersion":"0.11.0","compatibility":"compatible",
				 "publishedAt":"2026-01-01T00:00:00Z","trust":"unverified",
				 "trustExplanation":"Nothing has been checked.","installed":false,
				 "metadataFreshness":"offline","metadataOffline":true,
				 "metadataAsOf":"2026-06-01T12:00:00Z"}
			],"notes":[],
			"sources":[{"registryId":"corp","registryName":"Corp","freshness":"offline",
			 "offline":true,"fetchedAt":"2026-06-01T12:00:00Z",
			 "explanation":"AO could not reach this registry."}],
			"offline":true,
			"freshnessNotice":"A registry could not be reached, so some of what is listed is cached.",
			"installNotice":"This installs the Skill but does not enable it on any project."}`)
		case "/api/v1/skills/marketplace/corp/security-audit":
			_, _ = io.WriteString(w, `{"release":
				{"registryId":"corp","registryName":"Corp","skillId":"security-audit",
				 "name":"Security Audit","version":"0.2.0","publisher":"agent-orchestrator",
				 "description":"On-demand audit.","riskLevel":"critical",
				 "manifestDigest":"aa","artifactDigest":"bb","requestedCapabilities":["repo.read"],
				 "executionModes":[],"aoMinVersion":"0.11.0","compatibility":"compatible",
				 "publishedAt":"2026-01-01T00:00:00Z","trust":"unverified",
				 "trustExplanation":"Nothing has been checked.","installed":false,
				 "metadataFreshness":"stale","metadataOffline":true,
				 "metadataAsOf":"2026-06-01T12:00:00Z"},
			 "versions":[],
			 "source":{"registryId":"corp","freshness":"stale","offline":true,
			  "fetchedAt":"2026-06-01T12:00:00Z",
			  "explanation":"AO could not reach this registry, and what is shown was cached longer ago."},
			 "offline":true,
			 "freshnessNotice":"A registry could not be reached.",
			 "installNotice":"This installs the Skill but does not enable it on any project."}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	return Deps{ProcessAlive: func(int) bool { return true }}
}

// TestSkillsMarketplaceSearch_SaysWhenResultsAreCached is Check 19 at the
// terminal. A search whose results came from cache must not print the same
// thing as one whose results came from the registry.
func TestSkillsMarketplaceSearch_SaysWhenResultsAreCached(t *testing.T) {
	deps := offlineMarketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "search", "security")
	if err != nil {
		t.Fatalf("search: %v (%s)", err, errOut)
	}
	for _, want := range []string{
		// On the row itself, beside trust and never folded into it.
		"metadata=offline",
		// And with the moment the registry last actually answered.
		"NOT CURRENT: cached metadata, as of 2026-06-01T12:00:00Z",
		// Per registry, at the bottom, with the daemon's own sentence.
		"registry corp metadata=offline as-of=2026-06-01T12:00:00Z",
		"AO could not reach this registry.",
		"A registry could not be reached, so some of what is listed is cached.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q is missing %q", out, want)
		}
	}
	// The results are still there. Offline hides nothing; it labels.
	if !strings.Contains(out, "security-audit") {
		t.Fatalf("the cached results were dropped: %q", out)
	}
	// And the freshness did not touch what AO says it verified.
	if !strings.Contains(out, "trust=unverified") {
		t.Fatalf("output = %q", out)
	}
}

// TestSkillsMarketplaceShow_SaysWhenTheDetailIsStale is the same contract on
// the detail view, where "stale" is the older of the two offline states.
func TestSkillsMarketplaceShow_SaysWhenTheDetailIsStale(t *testing.T) {
	deps := offlineMarketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "show", "security-audit",
		"--registry", "corp")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, errOut)
	}
	for _, want := range []string{
		"metadata:      stale",
		"as of 2026-06-01T12:00:00Z",
		"AO could not reach this registry, and what is shown was cached longer ago.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q is missing %q", out, want)
		}
	}
	// Freshness sits below trust and does not replace it.
	if !strings.Contains(out, "trust:         unverified") {
		t.Fatalf("output = %q", out)
	}
}

// TestSkillsMarketplaceSearch_SaysNothingWhenEverythingIsLive: a line printed
// on every search is a line people stop reading, and then stop seeing when it
// changes.
func TestSkillsMarketplaceSearch_SaysNothingWhenEverythingIsLive(t *testing.T) {
	_, deps := marketplaceCLI(t)

	out, errOut, err := executeCLI(t, deps, "skills", "marketplace", "search", "security")
	if err != nil {
		t.Fatalf("search: %v (%s)", err, errOut)
	}
	if strings.Contains(out, "NOT CURRENT") || strings.Contains(out, "metadata=offline") {
		t.Fatalf("a live search claimed to be cached: %q", out)
	}
	if !strings.Contains(out, "metadata=live") {
		t.Fatalf("a live search did not state its freshness: %q", out)
	}
}
