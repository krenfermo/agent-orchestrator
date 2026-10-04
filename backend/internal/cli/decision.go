package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// resolveDecisionRequest mirrors controllers.ResolveDecisionRequest.
type resolveDecisionRequest struct {
	RunID              string   `json:"runId,omitempty"`
	Answer             string   `json:"answer,omitempty"`
	ReasonSummary      string   `json:"reasonSummary,omitempty"`
	EvidenceReferences []string `json:"evidenceReferences,omitempty"`
	Certainty          string   `json:"certainty,omitempty"`
	RequiresHuman      bool     `json:"requiresHuman,omitempty"`
}

// resolveDecisionResponse mirrors controllers.ResolveDecisionResponse. Only
// the fields the CLI reports on are decoded.
type resolveDecisionResponse struct {
	Resolution struct {
		Status string `json:"status"`
	} `json:"resolution"`
}

type decisionResolveOptions struct {
	session       string
	runID         string
	answer        string
	reason        string
	evidence      []string
	certainty     string
	requiresHuman bool
}

func newDecisionCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decision",
		Short: "Record a cross-provider Decision Resolver's result",
	}
	cmd.AddCommand(newDecisionResolveCommand(ctx))
	return cmd
}

func newDecisionResolveCommand(ctx *commandContext) *cobra.Command {
	var opts decisionResolveOptions
	cmd := &cobra.Command{
		Use:   "resolve [resolver-session-id]",
		Short: "Record a Decision Resolver's result for one auto_resolvable question",
		Args:  atMostOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.resolveDecision(cmd, args, opts)
		},
	}
	// Resolver agents (an LLM harness, same as reviewers) routinely spell
	// flags with underscores rather than hyphens; normalize so both resolve
	// to the same flag, mirroring review.go's own SetNormalizeFunc.
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	cmd.Flags().StringVar(&opts.session, "session", "", "Resolver session id (or pass it as the positional argument)")
	cmd.Flags().StringVar(&opts.runID, "run", "", "Resolution run id (required)")
	cmd.Flags().StringVar(&opts.answer, "answer", "", "The resolver's answer. Required unless --requires-human is set")
	cmd.Flags().StringVar(&opts.reason, "reason", "", "Short reason summary")
	cmd.Flags().StringArrayVar(&opts.evidence, "evidence", nil, "A bounded evidence reference (file path, or file path plus line range). Repeatable")
	cmd.Flags().StringVar(&opts.certainty, "certainty", "", "Required when --answer is set: actual, inferred, or unknown")
	cmd.Flags().BoolVar(&opts.requiresHuman, "requires-human", false, "Set instead of --answer when a safe answer could not be determined")
	return cmd
}

func (c *commandContext) resolveDecision(cmd *cobra.Command, args []string, opts decisionResolveOptions) error {
	session := strings.TrimSpace(opts.session)
	if len(args) == 1 {
		session = strings.TrimSpace(args[0])
	}
	if session == "" {
		return usageError{errors.New("usage: resolver session id is required (positional or --session)")}
	}
	runID := strings.TrimSpace(opts.runID)
	if runID == "" {
		return usageError{errors.New("usage: --run is required")}
	}
	answer := strings.TrimSpace(opts.answer)
	if answer == "" && !opts.requiresHuman {
		return usageError{errors.New("usage: --answer is required unless --requires-human is set")}
	}
	if answer != "" && opts.requiresHuman {
		return usageError{errors.New("usage: --answer and --requires-human cannot both be set")}
	}

	path := "sessions/" + url.PathEscape(session) + "/decisions/resolve"
	var res resolveDecisionResponse
	if err := c.postJSON(cmd.Context(), path, resolveDecisionRequest{
		RunID:              runID,
		Answer:             answer,
		ReasonSummary:      strings.TrimSpace(opts.reason),
		EvidenceReferences: opts.evidence,
		Certainty:          strings.TrimSpace(opts.certainty),
		RequiresHuman:      opts.requiresHuman,
	}, &res); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "recorded decision resolution %s for %s\n", res.Resolution.Status, session)
	return err
}
