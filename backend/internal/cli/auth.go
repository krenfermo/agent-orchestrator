package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

// auth.go — `ao auth login|status|logout`: how the CLI obtains, inspects and
// gives up an identity on an installation that requires single sign-on.
//
// The problem this closes. P4-B documented that under AO_AUTH_MODE=oidc a
// cookie-less CLI request resolves no principal, so every permission-gated
// route answers 401 NOT_AUTHENTICATED, and left "CLI tokens are a later
// slice". That left `ao send`, `ao workflow resume` and `ao workflow recover
// status` unusable on exactly the installations that took SSO seriously.
//
// The mechanism is the one the daemon already has, not a new one. The Electron
// supervisor signs in by starting an OIDC Authorization Code + PKCE flow with
// clientKind "desktop", handing the authorization request to the system
// browser, and then picking the resulting session up over loopback by
// presenting a handoff secret that never left this machine and never reached
// the provider (see ssosvc and controllers/auth_sso.go). `ao auth login` is a
// second loopback client of that same flow. Consequences that matter:
//
//   - The CLI never mints an identity. Only the daemon issues sessions, and
//     only after the identity provider authenticated a human.
//   - No browser cookie is copied and no token is invented.
//   - RBAC, tenant isolation and per-project grants are untouched: the CLI
//     ends up holding an ordinary session for an ordinary user, and every
//     route re-authorizes it exactly as it does a browser request.
//   - The credential is revocable and expiring, not a permanent secret in
//     plaintext: `ao auth logout` revokes it server-side, and signing the
//     account out from the app revokes it too.
//
// On a trusted-local installation none of this is needed, and `ao auth login`
// says so rather than manufacturing a login nobody asked for.

// sessionCookieName mirrors httpd/identity.SessionCookieName. It is duplicated
// rather than imported to keep the CLI free of httpd imports (AGENTS.md);
// TestSessionCookieNameMatchesDaemon pins the two together so a rename cannot
// silently un-authenticate the CLI.
const sessionCookieName = "ao_session"

// loginPollInterval is how often `ao auth login` asks the daemon whether the
// browser half has finished. The daemon's own desktop pickup polls at the same
// cadence; it is a loopback call against an in-memory-cheap lookup.
const loginPollInterval = 2 * time.Second

// loginMaxWait bounds the wait for a person to finish at the provider, so a
// login abandoned in a browser tab cannot leave a CLI hanging forever. The
// flow's own expiry usually fires first and is preferred when it is sooner.
const loginMaxWait = 10 * time.Minute

// handoffSecretBytes yields a 64-character hex secret. ssosvc requires at
// least 32 characters for a loopback handoff.
const handoffSecretBytes = 32

// authProvidersResponse mirrors controllers.AuthProvidersResponse.
type authProvidersResponse struct {
	Mode            string `json:"mode"`
	PasswordEnabled bool   `json:"passwordEnabled"`
	OIDC            *struct {
		DisplayName string `json:"displayName"`
		StartPath   string `json:"startPath"`
	} `json:"oidc"`
}

// oidcStartRequest/oidcStartResponse mirror controllers.OIDCStart*.
type oidcStartRequest struct {
	ClientKind    string `json:"clientKind"`
	HandoffSecret string `json:"handoffSecret"`
}

type oidcStartResponse struct {
	AuthorizationURL string    `json:"authorizationUrl"`
	FlowID           string    `json:"flowId"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// oidcClaimRequest/oidcClaimResponse mirror controllers.OIDCClaim*.
type oidcClaimRequest struct {
	FlowID        string `json:"flowId"`
	HandoffSecret string `json:"handoffSecret"`
}

type oidcClaimResponse struct {
	Status string    `json:"status"`
	User   *userView `json:"user"`
}

// userView mirrors controllers.UserView.
type userView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Username    string `json:"username"`
	Status      string `json:"status"`
	Role        string `json:"role"`
}

// meResponse mirrors controllers.MeResponse.
type meResponse struct {
	Status      string    `json:"status"`
	User        *userView `json:"user"`
	AuthMethod  string    `json:"authMethod"`
	Issuer      string    `json:"issuer"`
	Permissions []string  `json:"permissions"`
}

// logoutResponse mirrors controllers.LogoutResponse.
type logoutResponse struct {
	OK                    bool   `json:"ok"`
	ProviderEndSessionURL string `json:"providerEndSessionUrl"`
}

type authLoginOptions struct {
	noBrowser bool
}

func newAuthCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Sign the CLI in to this installation",
		Long: "On an installation that requires single sign-on, `ao` needs an identity of its own before any\n" +
			"permission-gated command will work. These commands obtain one through the daemon's existing\n" +
			"loopback sign-in handoff — the same flow the desktop app uses — and never fabricate a token,\n" +
			"copy a browser cookie, or weaken the daemon's authorization.",
	}
	cmd.AddCommand(newAuthLoginCommand(ctx))
	cmd.AddCommand(newAuthStatusCommand(ctx))
	cmd.AddCommand(newAuthLogoutCommand(ctx))
	return cmd
}

func newAuthLoginCommand(ctx *commandContext) *cobra.Command {
	var opts authLoginOptions
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through this installation's identity provider",
		Long: "Starts an OIDC Authorization Code + PKCE login, opens it in your browser, and — once you have\n" +
			"signed in there — picks the resulting AO session up over loopback using a secret that never left\n" +
			"this machine and never reached the provider.\n\n" +
			"The session is stored under the daemon's data directory with owner-only permissions. It expires,\n" +
			"and `ao auth logout` revokes it. On a trusted-local installation this command does nothing: the\n" +
			"CLI already resolves to the installation owner there.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.authLogin(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.noBrowser, "no-browser", false,
		"Print the sign-in URL instead of opening a browser (for a headless or remote shell)")
	return cmd
}

func newAuthStatusCommand(ctx *commandContext) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show which identity the CLI is acting as",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.authStatus(cmd, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print the raw identity response")
	return cmd
}

func newAuthLogoutCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the CLI's session and forget it",
		Long: "Revokes the session server-side and deletes the local credential. Idempotent: signing out when\n" +
			"there is nothing to sign out of succeeds.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctx.authLogout(cmd)
		},
	}
}

func (c *commandContext) authLogin(cmd *cobra.Command, opts authLoginOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var providers authProvidersResponse
	if err := c.getJSON(ctx, "auth/providers", &providers); err != nil {
		return err
	}
	if providers.OIDC == nil {
		// Not an error: this installation has not asked for SSO, so the CLI
		// already has the authority it has always had. Saying "signed in"
		// would be a lie, and failing would be a false alarm.
		_, err := fmt.Fprintf(out,
			"This installation does not use single sign-on (mode %s), so `ao` needs no separate login:\n"+
				"a loopback CLI request already resolves to the installation owner.\n",
			orUnknown(providers.Mode))
		return err
	}

	secret, err := newHandoffSecret()
	if err != nil {
		return err
	}
	var start oidcStartResponse
	if err := c.postJSON(ctx, "auth/oidc/start", oidcStartRequest{
		// The loopback handoff kind: the authorization request goes to the
		// system browser and the session is minted only when this process
		// presents the secret below, over loopback. Identical in every
		// security-relevant respect to the desktop supervisor's login.
		ClientKind:    "desktop",
		HandoffSecret: secret,
	}, &start); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "Sign in to AO with %s:\n\n  %s\n\n", providers.OIDC.DisplayName, start.AuthorizationURL)
	if opts.noBrowser {
		_, _ = fmt.Fprintln(out, "Open that URL, complete sign-in, and this command will finish on its own.")
	} else if err := c.openInBrowser(ctx, start.AuthorizationURL); err != nil {
		// A browser that will not open is not a failed login: the URL is
		// already printed and the flow is already valid.
		_, _ = fmt.Fprintf(out, "(could not open a browser automatically: %v — open the URL above)\n", err)
	}
	_, _ = fmt.Fprintln(out, "Waiting for sign-in to complete…")

	cred, err := c.awaitLoginHandoff(ctx, start, secret)
	if err != nil {
		return err
	}
	if err := writeCredential(cfg.DataDir, cred); err != nil {
		return err
	}

	who := cred.Email
	if who == "" {
		who = cred.UserID
	}
	_, err = fmt.Fprintf(out, "Signed in as %s. The credential is stored at %s (owner-only) and expires %s.\n",
		who, credentialPath(cfg.DataDir), cred.ExpiresAt.Local().Format(time.RFC3339))
	return err
}

// awaitLoginHandoff polls the daemon's loopback pickup until the person has
// finished at the provider. It returns the credential to store; it never
// writes anything itself, so an interrupted login leaves no partial state.
func (c *commandContext) awaitLoginHandoff(ctx context.Context, start oidcStartResponse, secret string) (storedCredential, error) {
	deadline := c.deps.Now().Add(loginMaxWait)
	if !start.ExpiresAt.IsZero() && start.ExpiresAt.Before(deadline) {
		deadline = start.ExpiresAt
	}
	for {
		var claim oidcClaimResponse
		var cookies []*http.Cookie
		err := c.doJSONPathFull(ctx, http.MethodPost, "/api/v1/auth/oidc/claim",
			oidcClaimRequest{FlowID: start.FlowID, HandoffSecret: secret}, &claim, nil, commandTimeout, &cookies)
		if err != nil {
			return storedCredential{}, err
		}
		if claim.Status == "complete" {
			return credentialFromClaim(claim, cookies, c.deps.Now())
		}
		if !c.deps.Now().Before(deadline) {
			return storedCredential{}, errors.New(
				"sign-in was not completed in time — run `ao auth login` again")
		}
		select {
		case <-ctx.Done():
			return storedCredential{}, ctx.Err()
		default:
		}
		c.deps.Sleep(loginPollInterval)
	}
}

// credentialFromClaim reads the session out of the pickup response. The token
// arrives ONLY as a Set-Cookie header on this loopback response — the daemon
// deliberately never puts it in a JSON body — so its absence is a hard error
// rather than something to work around.
func credentialFromClaim(claim oidcClaimResponse, cookies []*http.Cookie, now time.Time) (storedCredential, error) {
	for _, ck := range cookies {
		if ck.Name != sessionCookieName || ck.Value == "" {
			continue
		}
		cred := storedCredential{Token: ck.Value, ExpiresAt: ck.Expires, SavedAt: now}
		if claim.User != nil {
			cred.UserID, cred.Email = claim.User.ID, claim.User.Email
		}
		return cred, nil
	}
	return storedCredential{}, errors.New("the daemon completed sign-in but issued no session cookie")
}

func (c *commandContext) authStatus(cmd *cobra.Command, jsonOutput bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cred, hasCred, err := readCredential(cfg.DataDir)
	if err != nil {
		return err
	}

	var me meResponse
	if err := c.getJSON(ctx, "auth/me", &me); err != nil {
		var apiErr apiResponseError
		if errors.As(err, &apiErr) && apiErr.unauthenticated() && hasCred {
			// A stored credential the daemon will not accept is its own
			// finding: it says "sign in again", not "you never signed in".
			_, werr := fmt.Fprintf(out,
				"Not authenticated. A stored credential exists at %s but the daemon rejected it "+
					"(expired, revoked, or issued by a different installation). Run `ao auth login`.\n",
				credentialPath(cfg.DataDir))
			return werr
		}
		return err
	}
	if jsonOutput {
		return writeJSON(out, me)
	}

	_, _ = fmt.Fprintf(out, "status:      %s\n", orUnknown(me.Status))
	if me.User != nil {
		_, _ = fmt.Fprintf(out, "user:        %s", orUnknown(me.User.Email))
		if me.User.DisplayName != "" {
			_, _ = fmt.Fprintf(out, " (%s)", me.User.DisplayName)
		}
		_, _ = fmt.Fprintf(out, "\nrole:        %s\n", orUnknown(me.User.Role))
	}
	_, _ = fmt.Fprintf(out, "auth method: %s\n", orUnknown(me.AuthMethod))
	if me.Issuer != "" {
		_, _ = fmt.Fprintf(out, "issuer:      %s\n", me.Issuer)
	}
	if hasCred {
		_, _ = fmt.Fprintf(out, "credential:  %s (expires %s)\n",
			credentialPath(cfg.DataDir), cred.ExpiresAt.Local().Format(time.RFC3339))
	} else {
		_, _ = fmt.Fprintf(out, "credential:  none stored — this identity comes from the daemon itself\n")
	}
	if len(me.Permissions) > 0 {
		_, _ = fmt.Fprintf(out, "permissions: %s\n", strings.Join(me.Permissions, ", "))
	}
	return nil
}

func (c *commandContext) authLogout(cmd *cobra.Command) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	_, hasCred, err := readCredential(cfg.DataDir)
	if err != nil {
		return err
	}
	if !hasCred {
		_, werr := fmt.Fprintln(out, "No CLI credential is stored; nothing to sign out of.")
		return werr
	}

	// Revoke first, forget second. The reverse order would leave a live
	// session nobody can revoke because the token it needs is gone.
	var res logoutResponse
	if err := c.postJSON(ctx, "auth/logout", struct{}{}, &res); err != nil {
		return err
	}
	if err := removeCredential(cfg.DataDir); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Signed out. The session was revoked and the local credential deleted.")
	if res.ProviderEndSessionURL != "" {
		_, _ = fmt.Fprintf(out,
			"To end the identity provider's own session too, open:\n  %s\n", res.ProviderEndSessionURL)
	}
	return nil
}

// newHandoffSecret mints the loopback pickup secret. It is generated here, on
// this machine, and is presented only to the local daemon: it is never sent to
// the identity provider and never written to disk.
func newHandoffSecret() (string, error) {
	buf := make([]byte, handoffSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate handoff secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// openInBrowser hands the authorization URL to the desktop browser. Best
// effort by design — the URL is printed either way, so a headless host or a
// missing opener degrades to "open this yourself", never to a failed login.
func (c *commandContext) openInBrowser(ctx context.Context, url string) error {
	name, args := browserOpenCommand(url)
	if name == "" {
		return errors.New("no browser opener is known for this platform")
	}
	if out, err := c.deps.CommandOutput(ctx, name, args...); err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return fmt.Errorf("%s: %s", err, trimmed)
		}
		return err
	}
	return nil
}

func browserOpenCommand(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return "xdg-open", []string{url}
	}
}
