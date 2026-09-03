package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// workflow_usage.go — "what did this cost", from a terminal.
//
// The one rule the output obeys is the one the whole checkpoint obeys: a number
// never appears without the claim it is entitled to make. Estimated totals get a
// "~", a cost with no rate card prints "unknown" rather than "$0.00", and a role
// AO cannot meter prints "not observable" with the reason rather than a zero
// that would read as "this was free". A run with no ledger rows at all prints
// "no usage data recorded" — which is the honest answer for a legacy run, and
// the answer a fabricated 0 would have hidden.

// usageTokensPayload mirrors controllers.UsageTokenTotalsResponse.
type usageTokensPayload struct {
	Input         int64  `json:"input"`
	UncachedInput int64  `json:"uncachedInput"`
	CacheRead     int64  `json:"cacheRead"`
	CacheWrite    int64  `json:"cacheWrite"`
	Output        int64  `json:"output"`
	Reasoning     *int64 `json:"reasoning"`
	Total         int64  `json:"total"`
	Events        int64  `json:"events"`
}

// usageCostPayload mirrors controllers.UsageCostResponse.
type usageCostPayload struct {
	Known          bool     `json:"known"`
	Basis          string   `json:"basis"`
	Currency       string   `json:"currency"`
	Amount         float64  `json:"amount"`
	PricingSource  string   `json:"pricingSource"`
	PricingVersion string   `json:"pricingVersion"`
	EffectiveDate  string   `json:"effectiveDate"`
	UnpricedModels []string `json:"unpricedModels"`
}

type usageRolePayload struct {
	Role               string             `json:"role"`
	Cycle              int64              `json:"cycle"`
	Repair             bool               `json:"repair"`
	Provider           string             `json:"provider"`
	Harness            string             `json:"harness"`
	Model              string             `json:"model"`
	Tokens             usageTokensPayload `json:"tokens"`
	Cost               usageCostPayload   `json:"cost"`
	Source             string             `json:"source"`
	Observable         bool               `json:"observable"`
	UnobservableReason string             `json:"unobservableReason"`
}

type usageModelPayload struct {
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Tokens   usageTokensPayload `json:"tokens"`
	Cost     usageCostPayload   `json:"cost"`
	Source   string             `json:"source"`
}

type usageRunLinePayload struct {
	WorkflowRunID string             `json:"workflowRunId"`
	Tokens        usageTokensPayload `json:"tokens"`
	Cost          usageCostPayload   `json:"cost"`
}

type usageBudgetPayload struct {
	State        string   `json:"state"`
	Scope        string   `json:"scope"`
	Reason       string   `json:"reason"`
	TokenBudget  int64    `json:"tokenBudget"`
	CostBudget   float64  `json:"costBudget"`
	WarnPercent  int      `json:"warnPercent"`
	TokensUsed   int64    `json:"tokensUsed"`
	TokenPercent *float64 `json:"tokenPercent"`
	CostPercent  *float64 `json:"costPercent"`
}

type workflowUsagePayload struct {
	WorkflowRunID     string                `json:"workflowRunId"`
	Recorded          bool                  `json:"recorded"`
	Source            string                `json:"source"`
	Totals            usageTokensPayload    `json:"totals"`
	Cost              usageCostPayload      `json:"cost"`
	BaseTokens        usageTokensPayload    `json:"baseTokens"`
	RepairTokens      usageTokensPayload    `json:"repairTokens"`
	BaseCost          usageCostPayload      `json:"baseCost"`
	RepairCost        usageCostPayload      `json:"repairCost"`
	Roles             []usageRolePayload    `json:"roles"`
	Models            []usageModelPayload   `json:"models"`
	Unobservable      []usageRolePayload    `json:"unobservable"`
	Children          []usageRunLinePayload `json:"children"`
	FamilyTotals      usageTokensPayload    `json:"familyTotals"`
	FamilyCost        usageCostPayload      `json:"familyCost"`
	ApproximateEvents int64                 `json:"approximateEvents"`
	TotalEvents       int64                 `json:"totalEvents"`
	Budget            usageBudgetPayload    `json:"budget"`
	// Complete reports that every provider-backed role reported its spend, so
	// the total is the bill rather than a floor.
	Complete         bool   `json:"complete"`
	IncompleteReason string `json:"incompleteReason"`
}

type projectUsagePayload struct {
	ProjectID                string              `json:"projectId"`
	Period                   string              `json:"period"`
	From                     string              `json:"from"`
	To                       string              `json:"to"`
	Recorded                 bool                `json:"recorded"`
	Source                   string              `json:"source"`
	PeriodBasis              string              `json:"periodBasis"`
	Totals                   usageTokensPayload  `json:"totals"`
	Cost                     usageCostPayload    `json:"cost"`
	Workflows                int64               `json:"workflows"`
	Roles                    []usageRolePayload  `json:"roles"`
	Models                   []usageModelPayload `json:"models"`
	AverageTokensPerWorkflow *int64              `json:"averageTokensPerWorkflow"`
}

func newWorkflowUsageCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "usage <workflow-id>",
		Short: "Show what a workflow run has spent, and how well AO actually knows it",
		Long: "Prints the run's token totals and cost, broken down by role (planner, workers, reviewer,\n" +
			"repair, question resolver) and by provider/model, plus base execution against repair.\n\n" +
			"Every figure states its own provenance. Tokens marked `provider_reported` were parsed from the\n" +
			"provider's own transcript. Cost is CALCULATED from a named rate card and is a list-price\n" +
			"equivalent, not a bill — AO cannot see how a provider actually charges the account behind a\n" +
			"harness. A model no rate covers prints cost `unknown`, never $0.00, and a role AO cannot meter\n" +
			"prints `not observable` with the reason, never 0 tokens.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := strings.TrimSpace(args[0])
			if runID == "" {
				return usageError{errors.New("usage: workflow id is required")}
			}
			var res workflowUsagePayload
			if err := ctx.getJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/usage", &res); err != nil {
				return err
			}
			return printWorkflowUsage(cmd.OutOrStdout(), res)
		},
	}
}

// line and linef write into an in-memory builder or a tabwriter, neither of
// which can fail in a way a CLI could act on: a strings.Builder never errors,
// and a tabwriter reports its own on Flush. They exist so the formatting below
// reads as formatting rather than as thirty discarded error returns.
func line(w io.Writer, text string) { _, _ = io.WriteString(w, text+"\n") }

func linef(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func printWorkflowUsage(w io.Writer, res workflowUsagePayload) error {
	out := &strings.Builder{}
	linef(out, "workflow %s\n", res.WorkflowRunID)
	if !res.Recorded {
		line(out, "\nNo usage data recorded.")
		line(out, "This run has no metered provider events. That is not zero spend — it is an absence:")
		line(out, "a run created before usage accounting, or one whose provider AO cannot meter.")
		if len(res.Unobservable) > 0 {
			writeUnobservable(out, res.Unobservable)
		}
		_, err := io.WriteString(w, out.String())
		return err
	}

	linef(out, "\nTotal      %s  (%s)\n", formatTokens(res.Totals, res.Source), formatCost(res.Cost))
	linef(out, "  input    %s   output %s\n",
		humanTokens(res.Totals.Input), humanTokens(res.Totals.Output))
	if res.Totals.CacheRead > 0 || res.Totals.CacheWrite > 0 {
		linef(out, "  cache    read %s   write %s\n",
			humanTokens(res.Totals.CacheRead), humanTokens(res.Totals.CacheWrite))
	}
	if res.Totals.Reasoning != nil {
		linef(out, "  reasoning %s\n", humanTokens(*res.Totals.Reasoning))
	}

	// Base against repair, always printed when there IS repair: this is the
	// number that says whether re-work is what made the run expensive.
	if res.RepairTokens.Total > 0 {
		linef(out, "\nBase execution  %s  (%s)\n", humanTokens(res.BaseTokens.Total), formatCost(res.BaseCost))
		linef(out, "Repair         +%s  (%s)\n", humanTokens(res.RepairTokens.Total), formatCost(res.RepairCost))
	}

	if len(res.Roles) > 0 {
		line(out, "\nBy role")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		line(tw, "  ROLE\tPROVIDER\tMODEL\tTOKENS\tCOST\tSOURCE")
		for _, r := range res.Roles {
			label := r.Role
			if r.Repair && r.Cycle > 0 {
				label += " (cycle " + strconv.FormatInt(r.Cycle, 10) + ")"
			}
			linef(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
				label, orDash(r.Provider), orDash(r.Model),
				formatTokens(r.Tokens, r.Source), formatCost(r.Cost), r.Source)
		}
		_ = tw.Flush()
	}

	if len(res.Models) > 0 {
		line(out, "\nBy provider / model")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		line(tw, "  PROVIDER\tMODEL\tTOKENS\tCOST")
		for _, m := range res.Models {
			linef(tw, "  %s\t%s\t%s\t%s\n",
				orDash(m.Provider), orDash(m.Model), formatTokens(m.Tokens, m.Source), formatCost(m.Cost))
		}
		_ = tw.Flush()
	}

	if len(res.Children) > 0 {
		line(out, "\nChild runs")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		line(tw, "  RUN\tTOKENS\tCOST")
		for _, ch := range res.Children {
			linef(tw, "  %s\t%s\t%s\n", ch.WorkflowRunID, humanTokens(ch.Tokens.Total), formatCost(ch.Cost))
		}
		_ = tw.Flush()
		linef(out, "  family total  %s  (%s)\n", humanTokens(res.FamilyTotals.Total), formatCost(res.FamilyCost))
	}

	writeUnobservable(out, res.Unobservable)

	if res.ApproximateEvents > 0 {
		linef(out,
			"\nRole attribution: %d of %d events carried no provider timestamp and were attributed to the\n"+
				"run's first role window. The TOTAL above is unaffected; only the split between roles is approximate.\n",
			res.ApproximateEvents, res.TotalEvents)
	}
	writeBudget(out, res.Budget)
	if res.Cost.Known {
		linef(out, "\nCost basis: %s, from %s (%s, effective %s). A calculated cost is a list-price\n"+
			"equivalent of the tokens spent, not a bill.\n",
			res.Cost.Basis, res.Cost.PricingSource, res.Cost.PricingVersion, res.Cost.EffectiveDate)
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func writeUnobservable(out *strings.Builder, roles []usageRolePayload) {
	if len(roles) == 0 {
		return
	}
	seen := map[string]bool{}
	var names []string
	for _, r := range roles {
		if !seen[r.Role] {
			seen[r.Role] = true
			names = append(names, r.Role)
		}
	}
	sort.Strings(names)
	linef(out, "\nNot observable: %s\n", strings.Join(names, ", "))
	line(out, "These roles ran and spent tokens AO cannot meter — they execute in runtime panes rather")
	line(out, "than AO sessions, so no usage binding exists for them. Their spend is missing from the")
	line(out, "totals above; the totals are a lower bound, not the whole bill.")
}

func writeBudget(out *strings.Builder, b usageBudgetPayload) {
	switch b.State {
	case "", "unset":
		return
	case "ok":
		if b.TokenPercent != nil {
			linef(out, "\nBudget (%s): %.0f%% of %s tokens used.\n", b.Scope, *b.TokenPercent, humanTokens(b.TokenBudget))
		}
	case "warning":
		linef(out, "\nBudget (%s): WARNING — %s. %s\n", b.Scope, b.Reason, budgetPercentText(b))
	case "exhausted":
		linef(out, "\nBudget (%s): EXHAUSTED — %s. %s\n", b.Scope, b.Reason, budgetPercentText(b))
		line(out, "AO will not START new work for this run. Anything already running was left to finish.")
	}
}

func budgetPercentText(b usageBudgetPayload) string {
	var parts []string
	if b.TokenPercent != nil {
		parts = append(parts, fmt.Sprintf("%.0f%% of %s tokens", *b.TokenPercent, humanTokens(b.TokenBudget)))
	}
	if b.CostPercent != nil {
		parts = append(parts, fmt.Sprintf("%.0f%% of $%.2f", *b.CostPercent, b.CostBudget))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ") + " used."
}

func newProjectUsageCommand(ctx *commandContext) *cobra.Command {
	var period string
	cmd := &cobra.Command{
		Use:   "usage <project-id>",
		Short: "Show a project's token and cost totals for a period",
		Long: "Totals are bucketed by the instant AO DISPATCHED the work — a fact AO recorded itself — not by\n" +
			"any provider's billing period. Cost follows the same rules as `ao workflow usage`: calculated\n" +
			"from a named rate card, or `unknown`, never a fabricated zero.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID := strings.TrimSpace(args[0])
			if projectID == "" {
				return usageError{errors.New("usage: project id is required")}
			}
			switch period {
			case "today", "7d", "30d", "all":
			default:
				return usageError{fmt.Errorf("usage: --range must be one of today, 7d, 30d, all (got %q)", period)}
			}
			var res projectUsagePayload
			path := "projects/" + url.PathEscape(projectID) + "/usage?range=" + url.QueryEscape(period)
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			return printProjectUsage(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&period, "range", "7d", "Period to roll up: today, 7d, 30d or all")
	return cmd
}

func printProjectUsage(w io.Writer, res projectUsagePayload) error {
	out := &strings.Builder{}
	linef(out, "project %s — %s\n", res.ProjectID, res.Period)
	if !res.Recorded {
		line(out, "\nNo usage data recorded for this period.")
		_, err := io.WriteString(w, out.String())
		return err
	}
	linef(out, "\nTotal      %s  (%s)\n", formatTokens(res.Totals, res.Source), formatCost(res.Cost))
	linef(out, "Workflows  %d\n", res.Workflows)
	if res.AverageTokensPerWorkflow != nil {
		linef(out, "Average    %s per workflow\n", humanTokens(*res.AverageTokensPerWorkflow))
	} else {
		// Not "0 per workflow": a division with no denominator has no answer.
		line(out, "Average    unknown (no workflow spent anything in this period)")
	}
	if len(res.Roles) > 0 {
		line(out, "\nBy role")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		line(tw, "  ROLE\tTOKENS\tCOST")
		for _, r := range res.Roles {
			linef(tw, "  %s\t%s\t%s\n", r.Role, formatTokens(r.Tokens, r.Source), formatCost(r.Cost))
		}
		_ = tw.Flush()
	}
	if len(res.Models) > 0 {
		line(out, "\nBy provider / model")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		line(tw, "  PROVIDER\tMODEL\tTOKENS\tCOST")
		for _, m := range res.Models {
			linef(tw, "  %s\t%s\t%s\t%s\n", orDash(m.Provider), orDash(m.Model),
				formatTokens(m.Tokens, m.Source), formatCost(m.Cost))
		}
		_ = tw.Flush()
	}
	linef(out, "\nPeriod basis: %s (%s to %s).\n", res.PeriodBasis, orDash(res.From), res.To)
	_, err := io.WriteString(w, out.String())
	return err
}

// formatTokens renders a token total WITH its measurement claim. An estimated
// figure gets a leading "~" so it can never be mistaken for a provider count,
// and an unknown one prints "unknown" rather than a zero.
func formatTokens(t usageTokensPayload, source string) string {
	switch source {
	case "provider_reported":
		return humanTokens(t.Total)
	case "unknown", "":
		return "unknown"
	default:
		return "~" + humanTokens(t.Total)
	}
}

// formatCost renders money, or says it is not known. It never prints $0.00 for
// an absent rate: "free" and "unpriced" are different claims.
func formatCost(c usageCostPayload) string {
	if !c.Known {
		return "cost unknown"
	}
	currency := c.Currency
	if currency == "" {
		currency = "USD"
	}
	text := fmt.Sprintf("%s %.2f calculated", currency, c.Amount)
	if len(c.UnpricedModels) > 0 {
		text += fmt.Sprintf(" (partial: no rate for %s)", strings.Join(c.UnpricedModels, ", "))
	}
	return text
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}
