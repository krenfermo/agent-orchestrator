package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// adminResetPasswordOptions holds ao admin reset-password's flags.
type adminResetPasswordOptions struct {
	email    string
	password string
}

// adminResetPasswordRequest mirrors the daemon's request body for the
// loopback-only POST /api/v1/auth/admin/reset-password (Checkpoint 8P-E.8).
// Kept local rather than importing httpd/controllers, matching this
// package's existing convention (see session_switch.go's switchAgentRequest).
type adminResetPasswordRequest struct {
	Email       string `json:"email"`
	NewPassword string `json:"newPassword"`
}

type adminResetPasswordResponse struct {
	OK bool `json:"ok"`
}

func newAdminCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Local installation-recovery tooling",
	}
	cmd.AddCommand(newAdminResetPasswordCommand(ctx))
	return cmd
}

// newAdminResetPasswordCommand lets the operator of this machine set a new
// password for an existing account without a session — the recovery path
// for a migrated/QA installation whose password is unknown. It only ever
// talks to the running daemon's loopback API (never the sqlite file
// directly), matching this package's doc comment ("commands discover the
// local daemon, call its loopback HTTP API"), and the server-side route is
// itself unreachable off this machine (lan_listener.go blocks
// /api/v1/auth/admin on the LAN listener) — the same "local machine access
// is trusted" boundary AO_BOOTSTRAP_ADMIN_* env vars already rely on.
func newAdminResetPasswordCommand(ctx *commandContext) *cobra.Command {
	var opts adminResetPasswordOptions
	cmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Reset an existing account's password (local recovery only)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.adminResetPassword(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.email, "email", "", "Email of the account to reset (required)")
	cmd.Flags().StringVar(&opts.password, "password", "", "New password. If omitted, you will be prompted (input is hidden)")
	return cmd
}

func (c *commandContext) adminResetPassword(cmd *cobra.Command, opts adminResetPasswordOptions) error {
	email := strings.TrimSpace(opts.email)
	if email == "" {
		return usageError{fmt.Errorf("--email is required")}
	}

	password := opts.password
	if password == "" {
		var err error
		password, err = c.promptPassword()
		if err != nil {
			return err
		}
	}
	if len(password) < 8 {
		return usageError{fmt.Errorf("password must be at least 8 characters")}
	}

	var out adminResetPasswordResponse
	if err := c.postJSON(cmd.Context(), "auth/admin/reset-password", adminResetPasswordRequest{
		Email:       email,
		NewPassword: password,
	}, &out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.deps.Out, "Password reset for %s. Existing sessions were signed out.\n", email)
	return nil
}

// promptPassword reads a password twice (with confirmation) from the
// terminal without echoing it. Falls back to a plain (echoed) read if
// stdin isn't an interactive terminal, e.g. under test or when piped.
func (c *commandContext) promptPassword() (string, error) {
	_, _ = fmt.Fprint(c.deps.Out, "New password: ")
	pw1, err := readSecretLine(c.deps.In)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprint(c.deps.Out, "\nConfirm password: ")
	pw2, err := readSecretLine(c.deps.In)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintln(c.deps.Out)
	if pw1 != pw2 {
		return "", fmt.Errorf("passwords did not match")
	}
	return pw1, nil
}

// readSecretLine reads one line without echoing it when stdin is a real
// terminal (via golang.org/x/term); otherwise it reads a plain line, so
// scripted/non-interactive callers (tests, pipes) still work.
func readSecretLine(in interface{ Read([]byte) (int, error) }) (string, error) {
	if f, ok := in.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			line = append(line, buf[0])
		}
		if err != nil {
			// EOF (or any read error) ends the line. What was read before it is
			// still what the caller typed, so the read error is not propagated:
			// a password terminated by EOF rather than a newline is a password,
			// and the confirmation compare above is what rejects a truncated one.
			break
		}
	}
	//nolint:nilerr // a read error ends the line; the bytes read are still the answer.
	return strings.TrimRight(string(line), "\r"), nil
}
