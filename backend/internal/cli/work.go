package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// work.go — `ao work report`, the worker's side of P5-A phase 2B.
//
// A worker agent uses this to tell AO what it says it did. Nothing it sends is
// evidence and the command says so in its own help text, because the one way
// this feature could do harm is if an agent came to believe that reporting a
// pass was the same as having one. AO runs the task's planned checks itself,
// before any reviewer is dispatched, and judges the change on that.
//
// The command mirrors `ao review submit` deliberately: same client helpers,
// same session-addressed route, same underscore-tolerant flag normalization
// (agents routinely spell --follow_up), and the same "read the payload from a
// path or from stdin" convention so nothing has to be written into the worktree
// where it could be committed onto the worker's branch.

// submitWorkReportTestClaim mirrors controllers.SubmitWorkReportTestClaim.
type submitWorkReportTestClaim struct {
	Command        string `json:"command"`
	ClaimedOutcome string `json:"claimedOutcome,omitempty"`
	Note           string `json:"note,omitempty"`
}

// submitWorkReportCriterion mirrors controllers.SubmitWorkReportCriterion.
type submitWorkReportCriterion struct {
	Criterion string `json:"criterion"`
	Addressed bool   `json:"addressed"`
	Note      string `json:"note,omitempty"`
}

// submitWorkReportRequest mirrors controllers.SubmitWorkReportRequest.
type submitWorkReportRequest struct {
	Summary             string                      `json:"summary,omitempty"`
	ClaimedChangedPaths []string                    `json:"claimedChangedPaths,omitempty"`
	Criteria            []submitWorkReportCriterion `json:"criteria,omitempty"`
	TestsReported       []submitWorkReportTestClaim `json:"testsReported,omitempty"`
	Limitations         []string                    `json:"limitations,omitempty"`
	Risks               []string                    `json:"risks,omitempty"`
	FollowUp            []string                    `json:"followUp,omitempty"`
	Commit              string                      `json:"commit,omitempty"`
}

// workReportResponse mirrors controllers.WorkReportResponse.
type workReportResponse struct {
	WorkflowRunID  string   `json:"workflowRunId"`
	WorkflowStepID string   `json:"workflowStepId"`
	Superseded     bool     `json:"superseded,omitempty"`
	Version        string   `json:"version"`
	Truncated      []string `json:"truncated,omitempty"`
}

type workReportOptions struct {
	session     string
	report      string
	summary     string
	tests       []string
	criteria    []string
	limitations []string
	risks       []string
	followUp    []string
	commit      string
}

func newWorkCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Report on the work an AO worker session is doing",
	}
	cmd.AddCommand(newWorkReportCommand(ctx))
	return cmd
}

func newWorkReportCommand(ctx *commandContext) *cobra.Command {
	var opts workReportOptions
	cmd := &cobra.Command{
		Use:   "report [worker-session-id]",
		Short: "Record what this worker says it did (a declaration, not evidence)",
		Long: `Record a structured report of what this worker says it did.

The report is a DECLARATION. AO does not treat it as proof of anything: it runs
this task's own planned checks itself, before any reviewer is dispatched, and
judges the change on what it observes. Saying a test passed does not make it
so, and does not buy a lighter review.

What the report IS good for is telling a reviewer where to look, and — most
usefully — what you did NOT finish. A limitation, a risk or an unaddressed
criterion you declare here makes AO review the change MORE carefully, never
less.

The session defaults to AO_SESSION_ID, which AO sets in every pane it launches,
so inside a worker pane no argument is needed.

A report is accepted only until AO has decided how deeply to review the change.
Report before you finish, not after.`,
		Args: atMostOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.submitWorkReport(cmd, args, opts)
		},
	}
	// Agents routinely spell flags with underscores (--follow_up); normalize so
	// both resolve to the same flag. Same rule as `ao review submit`.
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
	cmd.Flags().StringVar(&opts.session, "session", "", "Worker session id (defaults to $AO_SESSION_ID)")
	cmd.Flags().StringVar(&opts.report, "json", "", "Whole report as JSON: a path, or - to read from stdin (so nothing is written into the worktree)")
	cmd.Flags().StringVar(&opts.summary, "summary", "", "What you changed, in prose")
	cmd.Flags().StringArrayVar(&opts.tests, "test", nil, "A command you ran, as 'command=outcome' where outcome is passed, failed or skipped (repeatable)")
	cmd.Flags().StringArrayVar(&opts.criteria, "criterion", nil, "An acceptance criterion, as 'text=addressed' or 'text=unaddressed' (repeatable)")
	cmd.Flags().StringArrayVar(&opts.limitations, "limitation", nil, "Something you did not cover (repeatable)")
	cmd.Flags().StringArrayVar(&opts.risks, "risk", nil, "A risk you are aware of (repeatable)")
	cmd.Flags().StringArrayVar(&opts.followUp, "follow-up", nil, "Work you are leaving for later (repeatable)")
	cmd.Flags().StringVar(&opts.commit, "commit", "", "Commit this report describes, when there is one")
	return cmd
}

func (c *commandContext) submitWorkReport(cmd *cobra.Command, args []string, opts workReportOptions) error {
	session := strings.TrimSpace(opts.session)
	if len(args) == 1 {
		session = strings.TrimSpace(args[0])
	}
	if session == "" {
		// The pane AO launched this worker into always carries it.
		session = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	}
	if session == "" {
		return usageError{errors.New("usage: worker session id is required (positional, --session, or $AO_SESSION_ID)")}
	}

	req, err := buildWorkReportRequest(cmd, opts)
	if err != nil {
		return err
	}

	path := "sessions/" + url.PathEscape(session) + "/work-report"
	var res workReportResponse
	if err := c.postJSON(cmd.Context(), path, req, &res); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	verb := "recorded"
	if res.Superseded {
		verb = "recorded (replacing an earlier report)"
	}
	if _, err := fmt.Fprintf(out, "%s work report for %s on run %s\n", verb, session, res.WorkflowRunID); err != nil {
		return err
	}
	if len(res.Truncated) > 0 {
		// Say it out loud: a worker that thinks it delivered a full report and
		// actually delivered a clipped one would not otherwise find out.
		if _, err := fmt.Fprintf(out, "note: AO shortened these fields to stay within its bounds: %s\n",
			strings.Join(res.Truncated, ", ")); err != nil {
			return err
		}
	}
	return nil
}

// buildWorkReportRequest assembles the report from --json, or from the
// individual flags. The two are mutually exclusive: silently merging a JSON
// document with flag fragments would make the request the caller sent
// unreconstructable from what they typed.
func buildWorkReportRequest(cmd *cobra.Command, opts workReportOptions) (submitWorkReportRequest, error) {
	raw := strings.TrimSpace(opts.report)
	flagsUsed := strings.TrimSpace(opts.summary) != "" || len(opts.tests) > 0 || len(opts.criteria) > 0 ||
		len(opts.limitations) > 0 || len(opts.risks) > 0 || len(opts.followUp) > 0 || strings.TrimSpace(opts.commit) != ""

	if raw != "" {
		if flagsUsed {
			return submitWorkReportRequest{}, usageError{errors.New(
				"usage: --json cannot be combined with --summary, --test, --criterion, --limitation, --risk, --follow-up or --commit")}
		}
		return readWorkReportJSON(cmd, raw)
	}
	if !flagsUsed {
		return submitWorkReportRequest{}, usageError{errors.New(
			"usage: nothing to report — pass --json, or at least one of --summary, --test, --criterion, --limitation, --risk or --follow-up")}
	}

	req := submitWorkReportRequest{
		Summary:     strings.TrimSpace(opts.summary),
		Limitations: trimAll(opts.limitations),
		Risks:       trimAll(opts.risks),
		FollowUp:    trimAll(opts.followUp),
		Commit:      strings.TrimSpace(opts.commit),
	}
	for _, spec := range opts.tests {
		claim, err := parseTestClaim(spec)
		if err != nil {
			return submitWorkReportRequest{}, err
		}
		req.TestsReported = append(req.TestsReported, claim)
	}
	for _, spec := range opts.criteria {
		crit, err := parseCriterion(spec)
		if err != nil {
			return submitWorkReportRequest{}, err
		}
		req.Criteria = append(req.Criteria, crit)
	}
	return req, nil
}

func readWorkReportJSON(cmd *cobra.Command, path string) (submitWorkReportRequest, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return submitWorkReportRequest{}, usageError{fmt.Errorf("read work report: %w", err)}
	}
	var req submitWorkReportRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return submitWorkReportRequest{}, usageError{fmt.Errorf("parse work report JSON: %w", err)}
	}
	return req, nil
}

// parseTestClaim reads "command=outcome". The outcome vocabulary is deliberately
// the plain words a person would use; the wire form's "claimed_" prefix is added
// here, so the CLI never asks an agent to type the word "claimed" and never lets
// it type anything that reads like a verified result.
func parseTestClaim(spec string) (submitWorkReportTestClaim, error) {
	command, outcome, ok := splitLastEquals(spec)
	if !ok || strings.TrimSpace(command) == "" {
		return submitWorkReportTestClaim{}, usageError{fmt.Errorf(
			"usage: --test must be 'command=outcome' (passed, failed or skipped), got %q", spec)}
	}
	var claimed string
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "passed", "pass", "ok":
		claimed = "claimed_passed"
	case "failed", "fail":
		claimed = "claimed_failed"
	case "skipped", "skip":
		claimed = "claimed_skipped"
	default:
		return submitWorkReportTestClaim{}, usageError{fmt.Errorf(
			"usage: --test outcome must be passed, failed or skipped, got %q", outcome)}
	}
	return submitWorkReportTestClaim{Command: strings.TrimSpace(command), ClaimedOutcome: claimed}, nil
}

// parseCriterion reads "text=addressed" / "text=unaddressed".
func parseCriterion(spec string) (submitWorkReportCriterion, error) {
	text, state, ok := splitLastEquals(spec)
	if !ok || strings.TrimSpace(text) == "" {
		return submitWorkReportCriterion{}, usageError{fmt.Errorf(
			"usage: --criterion must be 'text=addressed' or 'text=unaddressed', got %q", spec)}
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "addressed", "done", "yes", "true":
		return submitWorkReportCriterion{Criterion: strings.TrimSpace(text), Addressed: true}, nil
	case "unaddressed", "not-done", "no", "false":
		return submitWorkReportCriterion{Criterion: strings.TrimSpace(text), Addressed: false}, nil
	default:
		return submitWorkReportCriterion{}, usageError{fmt.Errorf(
			"usage: --criterion state must be addressed or unaddressed, got %q", state)}
	}
}

// splitLastEquals splits on the LAST '=' so a command containing one — which
// test invocations routinely do — keeps it.
func splitLastEquals(spec string) (string, string, bool) {
	i := strings.LastIndex(spec, "=")
	if i < 0 {
		return "", "", false
	}
	return spec[:i], spec[i+1:], true
}

func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
