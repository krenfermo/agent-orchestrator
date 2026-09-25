package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// Frente 3 / 3C: `ao workflow exploration <id>` -- what each agent of a run
// looked at, beside what it spent. The DTOs below mirror
// controllers.WorkflowExplorationResponse by hand, the CLI's deliberate
// boundary (AGENTS.md).

type explorationMetricPayload struct {
	Value  *int64 `json:"value"`
	Basis  string `json:"basis"`
	Method string `json:"method"`
}

type explorationRatioPayload struct {
	Value  *float64 `json:"value"`
	Basis  string   `json:"basis"`
	Method string   `json:"method"`
}

type agentExplorationPayload struct {
	Role        string   `json:"role"`
	Cycle       int64    `json:"cycle"`
	SubjectKind string   `json:"subjectKind"`
	SubjectID   string   `json:"subjectId"`
	Harness     string   `json:"harness"`
	Models      []string `json:"models"`

	ModelCalls           explorationMetricPayload `json:"modelCalls"`
	InputTokens          explorationMetricPayload `json:"inputTokens"`
	OutputTokens         explorationMetricPayload `json:"outputTokens"`
	CachedInputTokens    explorationMetricPayload `json:"cachedInputTokens"`
	FirstCallInputTokens explorationMetricPayload `json:"firstCallInputTokens"`
	ToolCalls            explorationMetricPayload `json:"toolCalls"`
	FileReads            explorationMetricPayload `json:"fileReads"`
	UniqueFilesRead      explorationMetricPayload `json:"uniqueFilesRead"`
	RepeatedReads        explorationMetricPayload `json:"repeatedReads"`
	Searches             explorationMetricPayload `json:"searches"`
	Listings             explorationMetricPayload `json:"listings"`
	Commands             explorationMetricPayload `json:"commands"`
	ExploreCommands      explorationMetricPayload `json:"exploreCommands"`
	ExplorationOps       explorationMetricPayload `json:"explorationOps"`
	Edits                explorationMetricPayload `json:"edits"`
	ShellEdits           explorationMetricPayload `json:"shellEdits"`
	Unattributed         explorationMetricPayload `json:"unattributedCommands"`
	HarnessTokens1st     explorationMetricPayload `json:"harnessTokensFirstCall"`
	UniqueFilesEdited    explorationMetricPayload `json:"uniqueFilesEdited"`
	OpsBeforeFirstEdit   explorationMetricPayload `json:"opsBeforeFirstEdit"`
	CallsBeforeFirstEdit explorationMetricPayload `json:"callsBeforeFirstEdit"`
	RepoBytesObserved    explorationMetricPayload `json:"repoBytesObserved"`
	AOContextBytes       explorationMetricPayload `json:"aoContextBytes"`
	HarnessContextBytes  explorationMetricPayload `json:"harnessContextBytes"`
	ExplorationRatio     explorationRatioPayload  `json:"explorationRatio"`
	AOContextRatio       explorationRatioPayload  `json:"aoContextRatio"`
	HarnessContextRatio  explorationRatioPayload  `json:"harnessContextRatio"`
	ActiveSpanMs         explorationMetricPayload `json:"activeSpanMs"`
	PathScopes           []struct {
		Scope string `json:"scope"`
		Count int64  `json:"count"`
	} `json:"pathScopes"`
	TopFiles []struct {
		Path  string `json:"path"`
		Reads int64  `json:"reads"`
	} `json:"topFiles"`
}

type runQualityPayload struct {
	FinalState         string                   `json:"finalState"`
	DurationMs         explorationMetricPayload `json:"durationMs"`
	Attempts           explorationMetricPayload `json:"attempts"`
	Retries            explorationMetricPayload `json:"retries"`
	ProviderFailovers  explorationMetricPayload `json:"providerFailovers"`
	VerifyRuns         explorationMetricPayload `json:"verifyRuns"`
	VerifyPassed       *bool                    `json:"verifyPassed"`
	ChecksPassed       explorationMetricPayload `json:"checksPassed"`
	ChecksFailed       explorationMetricPayload `json:"checksFailed"`
	ReviewRuns         explorationMetricPayload `json:"reviewRuns"`
	FinalReviewVerdict string                   `json:"finalReviewVerdict"`
	FixCycles          explorationMetricPayload `json:"fixCycles"`
}

type workflowExplorationPayload struct {
	RunID     string                    `json:"runId"`
	ProjectID string                    `json:"projectId"`
	Recorded  bool                      `json:"recorded"`
	Agents    []agentExplorationPayload `json:"agents"`
	Totals    agentExplorationPayload   `json:"totals"`
	Quality   runQualityPayload         `json:"quality"`
}

func newWorkflowExplorationCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "exploration <workflow-id>",
		Short: "Show what each agent of a run looked at (files, searches, edits) beside what it spent",
		Long: "Prints, per agent (role x session/pane), the files it read, how many were distinct, repeated\n" +
			"reads, searches, listings, commands, edits, exploration before the first edit, bytes returned to\n" +
			"the model by origin, model calls and tokens -- then the run's quality signals.\n\n" +
			"Every figure carries its basis. `~` marks DERIVED (an inference); `n/a` marks UNAVAILABLE (the\n" +
			"harness does not expose it), never 0. Paths are project-relative and only for files inside the\n" +
			"project that pass the repository boundary; no content, command or prompt is stored.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := strings.TrimSpace(args[0])
			if runID == "" {
				return usageError{errors.New("usage: workflow id is required")}
			}
			var raw json.RawMessage
			if err := ctx.getJSON(cmd.Context(), "workflows/"+url.PathEscape(runID)+"/exploration", &raw); err != nil {
				return err
			}
			if asJSON {
				_, err := cmd.OutOrStdout().Write(append(raw, '\n'))
				return err
			}
			var res workflowExplorationPayload
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			return printWorkflowExploration(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON response")
	return cmd
}

// fmtMetric renders one metric: the number, `~number` when derived, `n/a`
// when unavailable.
func fmtMetric(m explorationMetricPayload) string {
	if m.Basis == "unavailable" || m.Value == nil {
		return "n/a"
	}
	v := humanCount(*m.Value)
	if m.Basis == "derived" {
		return "~" + v
	}
	return v
}

func humanCount(v int64) string {
	return strconv.FormatInt(v, 10)
}

func fmtRatio(m explorationRatioPayload) string {
	if m.Basis == "unavailable" || m.Value == nil {
		return "n/a"
	}
	pct := strconv.FormatFloat(*m.Value*100, 'f', 1, 64) + "%"
	if m.Basis == "derived" {
		return "~" + pct
	}
	return pct
}

func printWorkflowExploration(w io.Writer, res workflowExplorationPayload) error {
	out := &strings.Builder{}
	linef(out, "workflow %s  (project %s)\n", res.RunID, res.ProjectID)
	if !res.Recorded {
		line(out, "\nNo exploration or usage recorded for this run.")
		line(out, "That is not zero exploration -- AO holds no transcript facts for it.")
	} else {
		for _, a := range res.Agents {
			label := a.Role
			if a.Cycle > 0 {
				label += " cycle " + strconv.FormatInt(a.Cycle, 10)
			}
			linef(out, "\nAgent Exploration -- %s  [%s %s, %s]\n", label, a.SubjectKind, a.SubjectID, a.Harness)
			writeAgentExploration(out, a)
		}
		line(out, "\nRun totals")
		writeAgentExploration(out, res.Totals)
	}
	writeRunQuality(out, res.Quality)
	_, err := io.WriteString(w, out.String())
	return err
}

func writeAgentExploration(out *strings.Builder, a agentExplorationPayload) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(a.Models) > 0 {
		linef(tw, "  Models\t%s\n", strings.Join(a.Models, ", "))
	}
	linef(tw, "  Files read\t%s\tUnique files\t%s\tRepeated\t%s\n", fmtMetric(a.FileReads), fmtMetric(a.UniqueFilesRead), fmtMetric(a.RepeatedReads))
	linef(tw, "  Searches\t%s\tListings\t%s\tExplore cmds\t%s\n", fmtMetric(a.Searches), fmtMetric(a.Listings), fmtMetric(a.ExploreCommands))
	linef(tw, "  Tool calls\t%s\tCommands\t%s\tEdits\t%s (shell ~%s)\n", fmtMetric(a.ToolCalls), fmtMetric(a.Commands), fmtMetric(a.Edits), strings.TrimPrefix(fmtMetric(a.ShellEdits), "~"))
	linef(tw, "  Files edited\t%s\tOps before 1st edit\t%s\tCalls before 1st edit\t%s\n", fmtMetric(a.UniqueFilesEdited), fmtMetric(a.OpsBeforeFirstEdit), fmtMetric(a.CallsBeforeFirstEdit))
	linef(tw, "  Model calls\t%s\tInput tokens\t%s\tCached\t%s\n", fmtMetric(a.ModelCalls), fmtMetric(a.InputTokens), fmtMetric(a.CachedInputTokens))
	linef(tw, "  Output tokens\t%s\t1st-call input\t%s\tActive span ms\t%s\n", fmtMetric(a.OutputTokens), fmtMetric(a.FirstCallInputTokens), fmtMetric(a.ActiveSpanMs))
	linef(tw, "  Harness tokens (1st call)\t%s\tUnattributed cmds\t%s\n", fmtMetric(a.HarnessTokens1st), fmtMetric(a.Unattributed))
	linef(tw, "  Repo bytes\t%s\tAO bytes\t%s\tHarness bytes\t%s\n", fmtMetric(a.RepoBytesObserved), fmtMetric(a.AOContextBytes), fmtMetric(a.HarnessContextBytes))
	linef(tw, "  Exploration share\t%s\tAO share\t%s\tHarness share\t%s\n", fmtRatio(a.ExplorationRatio), fmtRatio(a.AOContextRatio), fmtRatio(a.HarnessContextRatio))
	_ = tw.Flush()
	if len(a.PathScopes) > 0 {
		parts := make([]string, 0, len(a.PathScopes))
		for _, s := range a.PathScopes {
			parts = append(parts, s.Scope+"="+strconv.FormatInt(s.Count, 10))
		}
		linef(out, "  Path scopes  %s\n", strings.Join(parts, "  "))
	}
	if len(a.TopFiles) > 0 {
		line(out, "  Most read")
		for _, f := range a.TopFiles {
			linef(out, "    %3d  %s\n", f.Reads, f.Path)
		}
	}
}

func writeRunQuality(out *strings.Builder, q runQualityPayload) {
	line(out, "\nQuality")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	verify := "n/a"
	if q.VerifyPassed != nil {
		verify = strconv.FormatBool(*q.VerifyPassed)
	}
	verdict := q.FinalReviewVerdict
	if verdict == "" {
		verdict = "none"
	}
	linef(tw, "  Final state\t%s\tDuration ms\t%s\n", q.FinalState, fmtMetric(q.DurationMs))
	linef(tw, "  Verify passed\t%s\tChecks\t%s passed / %s failed\tVerify runs\t%s\n", verify, fmtMetric(q.ChecksPassed), fmtMetric(q.ChecksFailed), fmtMetric(q.VerifyRuns))
	linef(tw, "  Review verdict\t%s\tReview runs\t%s\tFix cycles\t%s\n", verdict, fmtMetric(q.ReviewRuns), fmtMetric(q.FixCycles))
	linef(tw, "  Attempts\t%s\tRetries\t%s\tFailovers\t%s\n", fmtMetric(q.Attempts), fmtMetric(q.Retries), fmtMetric(q.ProviderFailovers))
	_ = tw.Flush()
}
