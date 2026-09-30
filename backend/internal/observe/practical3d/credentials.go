package practical3d

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ProviderCredentials injects the operator's provider credential into an
// outgoing proxied request. It runs only in the supervisor, outside every
// sandbox: agents launch with a placeholder key, cannot read the operator's
// credential stores (sandbox), and never see a credential in their
// environment, arguments, prompt or files. Credentials are never logged or
// stored; only SHA-256 account references leave this type.
type ProviderCredentials interface {
	// Inject replaces any client-supplied credential headers with the
	// operator's credential for protocol ("anthropic" or "openai") and
	// returns the account reference it attests ("" when the provider
	// attests it in its response instead).
	Inject(protocol string, h http.Header) (accountRef string, err error)
}

// PlaceholderKey is the only "credential" an agent process ever holds.
const PlaceholderKey = "ao-3d-practical-proxy-injects-credentials"

// OperatorCredentials reads the operator's logins: Claude Code's OAuth
// credential from the login keychain, and Codex's ChatGPT tokens from its
// auth file. Each is re-read at most every refresh interval so a login the
// operator's own CLIs refresh is picked up; an expired credential fails
// closed.
type OperatorCredentials struct {
	CodexAuthFile string // ~/.codex/auth.json
	KeychainUser  string // login keychain account of "Claude Code-credentials"
	Now           func() time.Time

	mu        sync.Mutex
	anthropic cachedCredential
	openai    cachedCredential
}

type cachedCredential struct {
	token, account string
	expires        time.Time
	read           time.Time
}

const credentialRefresh = 60 * time.Second

var keychainUser = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (o *OperatorCredentials) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Inject implements ProviderCredentials.
func (o *OperatorCredentials) Inject(protocol string, h http.Header) (string, error) {
	for _, k := range []string{"Authorization", "X-Api-Key", "Chatgpt-Account-Id"} {
		h.Del(k)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch protocol {
	case "anthropic":
		c, err := o.fresh(&o.anthropic, o.readAnthropic)
		if err != nil {
			return "", err
		}
		h.Set("Authorization", "Bearer "+c.token)
		beta := h.Get("Anthropic-Beta")
		if !strings.Contains(beta, "oauth-2025-04-20") {
			if beta != "" {
				beta += ","
			}
			h.Set("Anthropic-Beta", beta+"oauth-2025-04-20")
		}
		return "", nil // attested by the anthropic-organization-id response header
	case "openai":
		c, err := o.fresh(&o.openai, o.readOpenAI)
		if err != nil {
			return "", err
		}
		h.Set("Authorization", "Bearer "+c.token)
		h.Set("Chatgpt-Account-Id", c.account)
		return sha256Hex([]byte(c.account)), nil
	}
	return "", fmt.Errorf("no credential for protocol %q", protocol)
}

// AccountRef returns the attested account reference of a protocol whose
// credential names its account (openai), without injecting anything.
func (o *OperatorCredentials) AccountRef(protocol string) (string, error) {
	if protocol != "openai" {
		return "", fmt.Errorf("protocol %q attests its account in the provider response", protocol)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c, err := o.fresh(&o.openai, o.readOpenAI)
	if err != nil {
		return "", err
	}
	return sha256Hex([]byte(c.account)), nil
}

func (o *OperatorCredentials) fresh(c *cachedCredential, read func() (cachedCredential, error)) (cachedCredential, error) {
	now := o.now()
	if c.token == "" || now.Sub(c.read) > credentialRefresh || (!c.expires.IsZero() && !now.Before(c.expires)) {
		n, err := read()
		if err != nil {
			return cachedCredential{}, err
		}
		n.read = now
		*c = n
	}
	if !c.expires.IsZero() && !now.Before(c.expires) {
		return cachedCredential{}, errors.New("operator provider credential expired (log in again with the provider CLI)")
	}
	return *c, nil
}

func (o *OperatorCredentials) readAnthropic() (cachedCredential, error) {
	user := o.KeychainUser
	if user == "" {
		user = os.Getenv("USER")
	}
	if !keychainUser.MatchString(user) {
		return cachedCredential{}, errors.New("invalid keychain account name")
	}
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials", "-a", user, "-w").Output() //nolint:gosec // fixed binary and arguments; the account name is validated

	if err != nil {
		return cachedCredential{}, errors.New("claude code credential not found in the login keychain")
	}
	var c struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(out, &c) != nil || c.ClaudeAiOauth.AccessToken == "" {
		return cachedCredential{}, errors.New("claude code keychain item holds no OAuth access token")
	}
	cc := cachedCredential{token: c.ClaudeAiOauth.AccessToken}
	if c.ClaudeAiOauth.ExpiresAt > 0 {
		cc.expires = time.UnixMilli(c.ClaudeAiOauth.ExpiresAt)
	}
	return cc, nil
}

func (o *OperatorCredentials) readOpenAI() (cachedCredential, error) {
	raw, err := os.ReadFile(o.CodexAuthFile)
	if err != nil {
		return cachedCredential{}, errors.New("codex login not readable")
	}
	var a struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Tokens.AccessToken == "" || a.Tokens.AccountID == "" {
		return cachedCredential{}, errors.New("codex login holds no ChatGPT tokens")
	}
	return cachedCredential{token: a.Tokens.AccessToken, account: a.Tokens.AccountID}, nil
}
