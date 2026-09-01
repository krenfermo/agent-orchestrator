package cli

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// memory_knowledge.go — the operator's view of shared task knowledge (P2-C §17).
//
// P2-A's `memory inspect` answers "what does AO remember about this codebase".
// These commands answer the four different questions P2-C creates, and they are
// different because their subject is different: inspect is about the
// REPOSITORY, and these are about what TASKS have learned and whether it still
// holds.
//
//	ao memory knowledge   what tasks have taught this project
//	ao memory decisions   which choices still govern, and what they replaced
//	ao memory risks       what is still open, and who closed what is not
//	ao memory task        everything one task produced, in every status
//	ao memory context     what one execution was actually told
//
// They all read through the daemon, and the daemon answers them through the
// same service call the pack builder uses. That is the point: an operator told
// a decision is active is looking at the judgement a Worker's pack made, not at
// an independent guess that happens to agree most of the time.
//
// There is deliberately no UI for any of this yet. P2-C's bar is that the
// behaviour is correct and inspectable; a dashboard built over numbers nobody
// has checked is a dashboard that hides them.

type memoryKnowledgeEnvelope struct {
	RepoID  string `json:"repoId"`
	Entries []struct {
		ID            string  `json:"id"`
		Type          string  `json:"type"`
		Scope         string  `json:"scope"`
		Key           string  `json:"key"`
		Summary       string  `json:"summary"`
		Status        string  `json:"status"`
		Kind          string  `json:"kind"`
		Share         string  `json:"share"`
		Subject       string  `json:"subject"`
		SourceTask    string  `json:"sourceTask"`
		SupersededBy  string  `json:"supersededBy"`
		Supersedes    string  `json:"supersedes"`
		ResolvedBy    string  `json:"resolvedBy"`
		ConflictsWith string  `json:"conflictsWith"`
		State         string  `json:"state"`
		StateReason   string  `json:"stateReason"`
		Confidence    float64 `json:"confidence"`
		SourceCommit  string  `json:"sourceCommit"`
		UpdatedAt     string  `json:"updatedAt"`
	} `json:"entries"`
	Total int `json:"total"`
}

type memoryManifestsEnvelope struct {
	Entries []struct {
		ID              string   `json:"id"`
		TaskRef         string   `json:"taskRef"`
		WorkflowRunID   string   `json:"workflowRunId"`
		Role            string   `json:"role"`
		PackDigest      string   `json:"packDigest"`
		PolicyVersion   int      `json:"policyVersion"`
		Generation      int64    `json:"generation"`
		IndexedCommit   string   `json:"indexedCommit"`
		ItemIDs         []string `json:"itemIds"`
		ItemCount       int      `json:"itemCount"`
		SelectedBytes   int      `json:"selectedBytes"`
		EstimatedTokens int      `json:"estimatedTokens"`
		Missing         []string `json:"missing"`
		Items           []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Summary string `json:"summary"`
		} `json:"items"`
	} `json:"entries"`
	Total int `json:"total"`
}

// knowledgeQuery builds the shared query every knowledge command uses.
func knowledgeQuery(projectID, repoPath, itemType, status, task string, limit int) string {
	q := url.Values{}
	for key, val := range map[string]string{
		"repoPath": repoPath, "type": itemType, "status": status, "task": task,
	} {
		if strings.TrimSpace(val) != "" {
			q.Set(key, val)
		}
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	path := "projects/" + url.PathEscape(projectID) + "/memory/knowledge"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

// printKnowledge renders knowledge entries.
//
// The lifecycle links are printed on their own indented lines rather than
// squeezed into the first: "superseded by X" is the answer to a question an
// operator only asks about one entry at a time, and putting it in the column
// layout would cost every OTHER line the width.
func printKnowledge(cmd *cobra.Command, res memoryKnowledgeEnvelope, empty string) error {
	out := cmd.OutOrStdout()
	if len(res.Entries) == 0 {
		_, _ = fmt.Fprintln(out, empty)
		return nil
	}
	for _, e := range res.Entries {
		marker := " "
		switch {
		case e.Status == "conflicting":
			marker = "?"
		case e.Status != "active":
			marker = "-"
		case e.State != "valid":
			marker = "!"
		}
		label := e.Type
		if e.Kind != "" && e.Kind != "risk" {
			label = e.Kind
		}
		_, _ = fmt.Fprintf(out, "%s %-14s %-11s %-9s %s\n", marker, label, e.Status, e.Share, e.Summary)
		if e.SourceTask != "" {
			_, _ = fmt.Fprintf(out, "    from task %s%s\n", e.SourceTask, commitSuffix(e.SourceCommit))
		}
		if e.SupersededBy != "" {
			_, _ = fmt.Fprintf(out, "    superseded by %s\n", e.SupersededBy)
		}
		if e.Supersedes != "" {
			_, _ = fmt.Fprintf(out, "    supersedes %s\n", e.Supersedes)
		}
		if e.ResolvedBy != "" {
			_, _ = fmt.Fprintf(out, "    resolved by task %s\n", e.ResolvedBy)
		}
		if e.ConflictsWith != "" {
			_, _ = fmt.Fprintf(out, "    conflicts with %s — AO could not order these, so neither is served as current\n",
				e.ConflictsWith)
		}
		if e.StateReason != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", e.StateReason)
		}
	}
	_, _ = fmt.Fprintf(out, "\n%d entries\n", res.Total)
	return nil
}

func commitSuffix(commit string) string {
	if strings.TrimSpace(commit) == "" {
		return ""
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return " at " + commit
}

func newMemoryKnowledgeCommand(ctx *commandContext) *cobra.Command {
	var repoPath, itemType, status, task string
	var limit int
	cmd := &cobra.Command{
		Use:   "knowledge <project-id>",
		Short: "List what tasks have taught this project",
		Long: "Shared task knowledge is what one task learned that a later task can reuse: its outcome, the decisions\n" +
			"it made, and the risks it left open.\n\n" +
			"Only ACTIVE knowledge is listed by default, because that is what a task would actually receive. Nothing\n" +
			"is ever deleted, so --status superseded, resolved or obsolete reconstructs what the project used to\n" +
			"believe and when it stopped.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res memoryKnowledgeEnvelope
			if err := ctx.getJSON(cmd.Context(),
				knowledgeQuery(args[0], repoPath, itemType, status, task, limit), &res); err != nil {
				return err
			}
			return printKnowledge(cmd, res, "No shared task knowledge.")
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to read (defaults to every repository of the project)")
	cmd.Flags().StringVar(&itemType, "type", "", "Only this type: task_result, decision, known_risk")
	cmd.Flags().StringVar(&status, "status", "", "Only this status: active, superseded, resolved, obsolete, conflicting")
	cmd.Flags().StringVar(&task, "task", "", "Only knowledge produced by this task")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum entries to list (default 200)")
	return cmd
}

func newMemoryDecisionsCommand(ctx *commandContext) *cobra.Command {
	var repoPath, status string
	var limit int
	cmd := &cobra.Command{
		Use:   "decisions <project-id>",
		Short: "List the decisions that still govern this project",
		Long: "A decision is a choice later work must respect. Re-deciding a topic does not overwrite the previous\n" +
			"answer: the old decision is marked superseded and names its replacement, so current context carries one\n" +
			"answer while the history stays reconstructible.\n\n" +
			"Use --status superseded to walk back through what the project used to believe.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res memoryKnowledgeEnvelope
			if err := ctx.getJSON(cmd.Context(),
				knowledgeQuery(args[0], repoPath, "decision", status, "", limit), &res); err != nil {
				return err
			}
			return printKnowledge(cmd, res, "No decisions recorded.")
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to read (defaults to every repository of the project)")
	cmd.Flags().StringVar(&status, "status", "", "Only this status: active (default), superseded, obsolete, conflicting")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum decisions to list (default 200)")
	return cmd
}

func newMemoryRisksCommand(ctx *commandContext) *cobra.Command {
	var repoPath, status string
	var limit int
	cmd := &cobra.Command{
		Use:   "risks <project-id>",
		Short: "List the risks and follow-ups still open on this project",
		Long: "Open risks are carried into the context of work that touches the same area. A later task that fixes one\n" +
			"resolves it, and a resolved risk stops being carried while keeping the task ref that closed it.\n\n" +
			"Use --status resolved to see what has been closed and by whom.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res memoryKnowledgeEnvelope
			if err := ctx.getJSON(cmd.Context(),
				knowledgeQuery(args[0], repoPath, "known_risk", status, "", limit), &res); err != nil {
				return err
			}
			return printKnowledge(cmd, res, "No open risks or follow-ups.")
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to read (defaults to every repository of the project)")
	cmd.Flags().StringVar(&status, "status", "", "Only this status: active (default), resolved, obsolete")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum risks to list (default 200)")
	return cmd
}

func newMemoryTaskCommand(ctx *commandContext) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "task <project-id> <task-id>",
		Short: "Show everything one task taught this project",
		Long: "Every status is shown, not just the current one. A decision this task made that a later task has since\n" +
			"replaced is still something this task produced, and hiding it would make the answer wrong rather than\n" +
			"tidy.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res memoryKnowledgeEnvelope
			if err := ctx.getJSON(cmd.Context(),
				knowledgeQuery(args[0], "", "", "", args[1], limit), &res); err != nil {
				return err
			}
			return printKnowledge(cmd, res,
				"This task produced no durable knowledge, or its unintegrated knowledge has been discarded.")
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum entries to list (default 200)")
	return cmd
}

func newMemoryContextCommand(ctx *commandContext) *cobra.Command {
	var task, run string
	var expand bool
	cmd := &cobra.Command{
		Use:   "context <project-id>",
		Short: "Show what an execution was actually told",
		Long: "A context manifest records the IDENTITIES of the memory facts one dispatch received — never the prompt,\n" +
			"and never the facts' text, which may have been superseded since.\n\n" +
			"It is what makes \"the Worker was working from a stale decision\" checkable rather than suspected, and\n" +
			"what lets a Reviewer be shown exactly what the Worker knew.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(task) == "" && strings.TrimSpace(run) == "" {
				return usageError{fmt.Errorf("name the execution with --task or --run")}
			}
			q := url.Values{}
			if strings.TrimSpace(task) != "" {
				q.Set("task", task)
			}
			if strings.TrimSpace(run) != "" {
				q.Set("run", run)
			}
			if expand {
				q.Set("expand", "true")
			}
			path := "projects/" + url.PathEscape(args[0]) + "/memory/manifests?" + q.Encode()
			var res memoryManifestsEnvelope
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Entries) == 0 {
				_, _ = fmt.Fprintln(out, "No context manifest was recorded for that execution.")
				return nil
			}
			for _, e := range res.Entries {
				_, _ = fmt.Fprintf(out, "%-9s %d facts, %d bytes (~%d tokens), policy v%d, generation %d%s\n",
					e.Role, e.ItemCount, e.SelectedBytes, e.EstimatedTokens,
					e.PolicyVersion, e.Generation, commitSuffix(e.IndexedCommit))
				_, _ = fmt.Fprintf(out, "    digest %s\n", e.PackDigest)
				for _, it := range e.Items {
					_, _ = fmt.Fprintf(out, "    - %-14s %-11s %s\n", it.Type, it.Status, it.Summary)
				}
				if len(e.Missing) > 0 {
					_, _ = fmt.Fprintf(out,
						"    %d fact(s) this execution was given no longer exist: %s\n",
						len(e.Missing), strings.Join(e.Missing, ", "))
				}
			}
			_, _ = fmt.Fprintf(out, "\n%d manifests\n", res.Total)
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&task, "task", "", "The task whose frozen context to show")
	cmd.Flags().StringVar(&run, "run", "", "The workflow run whose executions to show")
	cmd.Flags().BoolVar(&expand, "expand", false, "Resolve the manifest's item ids back into the facts they name")
	return cmd
}
