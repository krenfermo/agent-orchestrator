// Package providerauth is the single canonical answer to one question every
// unattended provider launch must ask before it spends anything:
//
//	from WHERE will this subprocess get its credential, and can it get it
//	with nobody sitting in front of the machine?
//
// THE INCIDENT (2026-09-06, wf-4e3d187b). A real workflow stopped at
// `planner_auth_unavailable` while macOS displayed "security wants to use the
// keychain 'login'". Three things about that failure shaped this package.
//
// The prompt was unanswerable. The keychain it named was not the person's
// login keychain; it was AO's own per-user `login.keychain-db` inside the
// isolated runtime home, whose password is a random secret AO generated. macOS
// titles the dialog with the keychain's FILE name, so it looked exactly like
// the one credential the person did know the password to.
//
// AO could not see it coming. The existing preflight asked the claude-code
// adapter `AuthStatus(ctx)`, which reads the DAEMON's own ~/.claude.json and
// the DAEMON's own ANTHROPIC_API_KEY -- never the environment the launch would
// actually run in. With per-user runtime isolation active the launch resolves a
// completely different HOME, config dir and keychain, so the preflight was
// answering a question about a different process than the one about to start,
// and cheerfully reported "auth: ok" for a launch that could not authenticate.
//
// And it BLOCKED rather than failed. An unattended subprocess that hits a GUI
// dialog does not exit; it sits there until a deadline kills it. Every such
// launch costs a full planner budget to learn nothing.
//
// So: this package resolves the auth mode from the LAUNCH environment, never
// the daemon's; it classifies "would require interaction" as a state of its
// own, distinct from both "works" and "no credentials"; and it is the one
// contract planner, worker, reviewer and repair all read, so no role can be
// fixed while another still depends on the popup.
//
// # Secrets
//
// Nothing in this package returns, logs, or embeds a credential value. It
// reports the NAME of the variable that carries one, the PATH of a helper or a
// keychain, and an OS status sentence. Probe deliberately never reads a token:
// it establishes that a source exists and that the store holding it opens.
package providerauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
)

// Mode is HOW a provider subprocess obtains its credential. These are the
// mechanisms Claude Code itself documents; AO does not invent auth paths, and
// in particular never copies an OAuth token out of a keychain into a file or
// an environment variable of its own.
type Mode string

const (
	// ModeEnvironment means a credential is handed to the subprocess in its own
	// environment (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN). Fully
	// unattended, and the mode an automation host should prefer.
	ModeEnvironment Mode = "environment"
	// ModeHelper means the provider's configured apiKeyHelper program prints a
	// credential on demand. Unattended as long as the program is executable
	// and does not itself prompt.
	ModeHelper Mode = "helper"
	// ModeCloudProvider is Bedrock/Vertex, where the credential comes from the
	// cloud SDK's own chain rather than from Anthropic. AO cannot verify that
	// chain and never claims to.
	ModeCloudProvider Mode = "cloud_provider"
	// ModeKeychain is the provider's own interactive login, persisted in the OS
	// credential store (macOS Keychain) or a credentials file. Establishing it
	// is interactive by nature; USING it is not, provided the store opens.
	ModeKeychain Mode = "keychain"
	// ModeUnknown means AO could not tell. Never a refusal.
	ModeUnknown Mode = "unknown"
)

// Status is whether the resolved mode can produce a credential with nobody
// present. It is deliberately four-valued: collapsing RequiresInteraction into
// Unavailable would lose the only distinction that tells a person whether to
// go and answer a dialog or go and configure a credential, and collapsing
// Unknown into either would either ground working installs or bless broken
// ones.
type Status string

const (
	// StatusAvailable means a credential source exists and the store holding it
	// opens without a person.
	StatusAvailable Status = "available"
	// StatusRequiresInteraction means launching would open a dialog or a login
	// prompt nobody is there to answer. The launch must be refused, not
	// attempted: attempting it burns a full budget and ends in a timeout.
	StatusRequiresInteraction Status = "requires_interaction"
	// StatusUnavailable means there is affirmatively no credential source the
	// launch could use.
	StatusUnavailable Status = "unavailable"
	// StatusUnknown means AO could not determine it. Treated as ready everywhere,
	// because the cost of a wrong "unknown" is a failure AO fails to warn
	// about, while the cost of a wrong refusal is AO refusing to work.
	StatusUnknown Status = "unknown"
)

// Contract is the resolved answer for exactly one launch. Every field is safe
// to persist into a durable stop and to render to a person.
type Contract struct {
	Mode   Mode
	Status Status
	// Source names WHERE the credential comes from: a variable NAME, a helper
	// path, a keychain path, a config directory. Never a value.
	Source string
	// Reason is AO's own one-line explanation, always populated for a
	// non-available status so a durable stop never says only "auth failed".
	Reason string
}

// Unattended reports whether a launch may proceed. Unknown proceeds: see
// StatusUnknown.
func (c Contract) Unattended() bool {
	return c.Status == StatusAvailable || c.Status == StatusUnknown
}

// Request is one launch's auth question. Env is the MERGED environment the
// subprocess will actually receive -- not the daemon's -- which is the entire
// correction this package exists to make.
type Request struct {
	Harness domain.AgentHarness
	Env     map[string]string
	// Required pins the mode an operator demands (AO_PROVIDER_AUTH_MODE).
	// Empty means "whatever is configured", resolved in preference order.
	// A pinned mode that is not satisfiable is a refusal, not a fallback:
	// an operator who says "use an API key" must not silently get a launch
	// that reaches for somebody's keychain instead.
	Required Mode
	// HostHome is the daemon user's real home directory, used to tell an
	// isolated runtime home from the host's own. Empty resolves it from the
	// OS; tests set it explicitly.
	HostHome string
}

// Probe resolves the contract for one launch.
//
// The order is preference order, and it is also the order of decreasing
// certainty: an environment credential is a fact about the argv AO is about to
// exec, a helper is a fact about a file AO can stat, and the keychain is a
// fact about an OS store whose contents AO deliberately does not read.
func Probe(ctx context.Context, req Request) Contract {
	if err := ctx.Err(); err != nil {
		return Contract{Mode: ModeUnknown, Status: StatusUnknown, Reason: "auth probe cancelled before it ran"}
	}
	if !isClaudeFamily(req.Harness) {
		// A harness AO has no credential model for produces Unknown, which is
		// ready. Refusing launches for providers this package has never been
		// taught about would be strictly worse than not checking them.
		return Contract{
			Mode:   ModeUnknown,
			Status: StatusUnknown,
			Reason: fmt.Sprintf("AO has no credential model for harness %q; auth was not checked", req.Harness),
		}
	}

	candidates := []func(Request) (Contract, bool){
		probeEnvironment,
		probeCloudProvider,
		probeHelper,
		probeKeychain,
	}
	if req.Required != "" {
		only, ok := probeForMode(req.Required)
		if !ok {
			return Contract{
				Mode:   ModeUnknown,
				Status: StatusUnavailable,
				Reason: fmt.Sprintf("configured provider auth mode %q is not one AO implements", req.Required),
			}
		}
		candidates = []func(Request) (Contract, bool){only}
	}
	for _, probe := range candidates {
		if c, ok := probe(req); ok {
			return c
		}
	}
	if req.Required != "" {
		return Contract{
			Mode:   req.Required,
			Status: StatusUnavailable,
			Reason: fmt.Sprintf("provider auth mode %q is configured but no credential source for it was found in the launch environment", req.Required),
		}
	}
	return Contract{
		Mode:   ModeUnknown,
		Status: StatusUnknown,
		Reason: "no credential source AO recognises was found in the launch environment; the provider's own login may still work",
	}
}

// probeForMode maps a pinned mode onto its single probe.
func probeForMode(m Mode) (func(Request) (Contract, bool), bool) {
	switch m {
	case ModeEnvironment:
		return probeEnvironment, true
	case ModeHelper:
		return probeHelper, true
	case ModeCloudProvider:
		return probeCloudProvider, true
	case ModeKeychain:
		return probeKeychain, true
	}
	return nil, false
}

// environmentCredentialVars are the variable NAMES Claude Code reads a
// credential from. Order matters only for which name is reported.
var environmentCredentialVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// probeEnvironment is the fully unattended path: a credential already present
// in the environment the subprocess will receive.
func probeEnvironment(req Request) (Contract, bool) {
	for _, name := range environmentCredentialVars {
		if strings.TrimSpace(req.Env[name]) == "" {
			continue
		}
		// The NAME is the evidence. The value is never touched again.
		return Contract{
			Mode:   ModeEnvironment,
			Status: StatusAvailable,
			Source: name,
			Reason: fmt.Sprintf("the launch environment carries %s, so the provider authenticates without any interactive step", name),
		}, true
	}
	return Contract{}, false
}

// cloudProviderVars are the toggles that move Claude Code onto a cloud
// provider's own credential chain.
var cloudProviderVars = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"}

// probeCloudProvider recognises Bedrock/Vertex and stops there. The credential
// lives in the AWS/GCP chain, which AO does not model and must not pretend to
// have checked -- but it is affirmatively NOT the keychain, so recognising it
// keeps AO from refusing a Bedrock launch over a macOS keychain it never uses.
func probeCloudProvider(req Request) (Contract, bool) {
	for _, name := range cloudProviderVars {
		if !truthy(req.Env[name]) {
			continue
		}
		return Contract{
			Mode:   ModeCloudProvider,
			Status: StatusUnknown,
			Source: name,
			Reason: fmt.Sprintf("%s selects a cloud provider's own credential chain, which AO does not check", name),
		}, true
	}
	return Contract{}, false
}

// probeHelper resolves the provider's configured apiKeyHelper.
//
// A helper is unattended only if it can actually run, so a configured helper
// that is missing or not executable is an affirmative refusal rather than a
// fallback onto some other mode: the operator declared where credentials come
// from, and silently reaching elsewhere is how a launch ends up at a dialog.
func probeHelper(req Request) (Contract, bool) {
	settingsPath, ok := claudeSettingsPath(req.Env)
	if !ok {
		return Contract{}, false
	}
	helper, ok := readAPIKeyHelper(settingsPath)
	if !ok || strings.TrimSpace(helper) == "" {
		return Contract{}, false
	}
	program := helperProgram(helper)
	if err := executableFile(program); err != nil {
		return Contract{
			Mode:   ModeHelper,
			Status: StatusUnavailable,
			Source: settingsPath,
			Reason: fmt.Sprintf("apiKeyHelper is configured in %s but its program %q cannot be run: %v", settingsPath, program, err),
		}, true
	}
	return Contract{
		Mode:   ModeHelper,
		Status: StatusAvailable,
		Source: program,
		Reason: fmt.Sprintf("apiKeyHelper %q supplies the credential, so the provider authenticates without any interactive step", program),
	}, true
}

// probeKeychain is the last resort and the one the incident lives in: the
// provider's own OAuth login, persisted in the OS credential store.
//
// It answers exactly one thing -- whether the store the launch resolves to can
// be OPENED without a person -- and deliberately does not read what is inside
// it. Reading a keychain item is itself a prompting operation on a locked
// keychain, so a probe that tried to verify the credential would be capable of
// causing the very dialog it exists to predict.
func probeKeychain(req Request) (Contract, bool) {
	home := strings.TrimSpace(req.Env["HOME"])
	configDir := strings.TrimSpace(req.Env["CLAUDE_CONFIG_DIR"])
	if home == "" && configDir == "" {
		return Contract{}, false
	}
	isolated := isolatedHome(home, req.HostHome)

	report := runtimehome.InspectKeychain(home)
	switch report.State {
	case runtimehome.KeychainRequiresInteraction:
		// The definitive refusal, and the whole reason this package exists.
		return Contract{
			Mode:   ModeKeychain,
			Status: StatusRequiresInteraction,
			Source: report.Path,
			Reason: fmt.Sprintf("the launch would resolve AO's own provider keychain at %s, which AO cannot open with its stored secret (%s); macOS would raise an unlock dialog no unattended run can answer", report.Path, report.Detail),
		}, true
	case runtimehome.KeychainOK:
		return Contract{
			Mode:   ModeKeychain,
			Status: StatusAvailable,
			Source: report.Path,
			Reason: fmt.Sprintf("AO's per-user provider keychain at %s opens without interaction", report.Path),
		}, true
	}

	// No AO-managed keychain in the launch path.
	//
	// On the HOST's own home that is the normal, working desktop case: the
	// provider's credential lives in the person's real login keychain, which
	// AO does not inspect and must not claim to know about. Unknown is ready.
	if !isolated {
		return Contract{
			Mode:   ModeKeychain,
			Status: StatusUnknown,
			Source: home,
			Reason: "the launch keeps the host user's own home and credential store, which AO does not inspect",
		}, true
	}

	// An ISOLATED home with no AO keychain is different: AO built this home,
	// so it knows nothing else populated it. The only remaining source is the
	// provider's credentials file inside the isolated config dir.
	if credentialsFilePresent(configDir, home) {
		return Contract{
			Mode:   ModeKeychain,
			Status: StatusAvailable,
			Source: configDir,
			Reason: fmt.Sprintf("the isolated provider profile at %s holds a stored credential", configDir),
		}, true
	}
	return Contract{
		Mode:   ModeKeychain,
		Status: StatusUnavailable,
		Source: configDir,
		Reason: fmt.Sprintf("the launch runs in AO's isolated runtime home %s, which holds no provider credential: connect the provider for this user, or configure an unattended credential (ANTHROPIC_API_KEY / apiKeyHelper)", home),
	}, true
}

// isolatedHome reports whether the launch HOME is one AO substituted rather
// than the daemon user's own. It is derived from the paths rather than passed
// in so every caller gets the same answer without new plumbing; hostHome is
// injectable for tests.
func isolatedHome(home, hostHome string) bool {
	if home == "" {
		return false
	}
	if hostHome == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			// Cannot tell. Treating it as the host's home keeps the "unknown
			// is not a refusal" rule: the alternative would refuse launches
			// on a machine where AO simply could not read a home directory.
			return false
		}
		hostHome = resolved
	}
	return filepath.Clean(home) != filepath.Clean(hostHome)
}

// credentialsFilePresent reports whether the provider's own credentials file
// exists for this launch. Existence only: the file holds an OAuth token and is
// never opened here.
func credentialsFilePresent(configDir, home string) bool {
	for _, dir := range []string{configDir, filepath.Join(home, ".claude")} {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// claudeSettingsPath resolves the settings file the launch will read, from the
// launch env's own CLAUDE_CONFIG_DIR/HOME rather than the daemon's.
func claudeSettingsPath(env map[string]string) (string, bool) {
	if dir := strings.TrimSpace(env["CLAUDE_CONFIG_DIR"]); dir != "" {
		return filepath.Join(dir, "settings.json"), true
	}
	if home := strings.TrimSpace(env["HOME"]); home != "" {
		return filepath.Join(home, ".claude", "settings.json"), true
	}
	return "", false
}

// readAPIKeyHelper extracts only the apiKeyHelper field. The settings file may
// hold unrelated configuration; nothing else is retained.
func readAPIKeyHelper(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var settings struct {
		APIKeyHelper string `json:"apiKeyHelper"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return "", false
	}
	return settings.APIKeyHelper, true
}

// helperProgram takes the executable out of a helper command line. The setting
// is a command, not a path, so a helper declared as "/usr/local/bin/get-key
// --profile ao" must still be checked as its program.
func helperProgram(helper string) string {
	fields := strings.Fields(strings.TrimSpace(helper))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// executableFile reports whether path is a regular file with an execute bit.
func executableFile(path string) error {
	if path == "" {
		return fmt.Errorf("no program named")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory, not an executable")
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("found but not executable (mode %s)", info.Mode().Perm())
	}
	return nil
}

// truthy reads the on/off conventions Claude Code's own toggles accept.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// isClaudeFamily reports whether this harness uses Claude Code's credential
// model. Deliberately a narrow match, mirroring providerpreflight.
func isClaudeFamily(harness domain.AgentHarness) bool {
	return harness == domain.HarnessClaudeCode
}

// ParseMode validates an operator-supplied auth mode. "auto" and the empty
// string both mean "resolve in preference order".
func ParseMode(raw string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(raw))); m {
	case "", "auto":
		return "", nil
	case ModeEnvironment, ModeHelper, ModeCloudProvider, ModeKeychain:
		return m, nil
	default:
		return "", fmt.Errorf("invalid provider auth mode %q: must be auto, environment, helper, cloud_provider or keychain", raw)
	}
}
