package cli

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// memory.go — P2-A's operator commands (§13).
//
//	ao memory status <project>              what AO remembers, and whether it can vouch for it
//	ao memory inspect <project>             the individual facts, stale ones included
//	ao memory rebuild <project>             re-derive it
//	ao memory invalidate <project>          retire what can no longer be proven
//
// The deliberate shape here mirrors `ao capacity status`: the CLI prints what
// the daemon decided and classifies nothing itself, so an operator's preview
// and the daemon's own view can never disagree. There is no `ao memory index`
// separate from `rebuild` — one verb, one code path.
//
// There is also no command that prints a context pack's contents. A pack is
// assembled for a specific dispatch, with that dispatch's changed paths and
// budget; printing one out of context would show something no agent was ever
// given, which is worse than showing nothing.

type memoryStatusEnvelope struct {
	Repositories []struct {
		RepoID        string         `json:"repoId"`
		RepoPath      string         `json:"repoPath"`
		Phase         string         `json:"phase"`
		Generation    int64          `json:"generation"`
		IndexedCommit string         `json:"indexedCommit"`
		Branch        string         `json:"branch"`
		ResumeCursor  string         `json:"resumeCursor"`
		LastError     string         `json:"lastError"`
		Healthy       bool           `json:"healthy"`
		Items         int            `json:"items"`
		Valid         int            `json:"valid"`
		Stale         int            `json:"stale"`
		Invalidated   int            `json:"invalidated"`
		Rebuilding    int            `json:"rebuilding"`
		TaskLocal     int            `json:"taskLocal"`
		Relations     int            `json:"relations"`
		FilesIndexed  int            `json:"filesIndexed"`
		FilesSkipped  int            `json:"filesSkipped"`
		LastIndexedAt *string        `json:"lastIndexedAt"`
		LastUpdatedAt *string        `json:"lastUpdatedAt"`
		ByType        map[string]int `json:"byType"`
	} `json:"repositories"`
}

type memoryItemsEnvelope struct {
	RepoID string `json:"repoId"`
	Items  []struct {
		Type         string   `json:"type"`
		Scope        string   `json:"scope"`
		Key          string   `json:"key"`
		Origin       string   `json:"origin"`
		OriginRef    string   `json:"originRef"`
		Summary      string   `json:"summary"`
		State        string   `json:"state"`
		StateReason  string   `json:"stateReason"`
		Confidence   float64  `json:"confidence"`
		Generation   int64    `json:"generation"`
		SourceCommit string   `json:"sourceCommit"`
		SourcePaths  []string `json:"sourcePaths"`
		ContentBytes int      `json:"contentBytes"`
		UpdatedAt    string   `json:"updatedAt"`
		// P2-D: the second axis. A fact can be perfectly valid on the drift
		// axis and still be withheld, so an inspect that printed only `state`
		// would show "valid" beside a fact no agent is receiving.
		Authority       string `json:"authority"`
		AuthorityReason string `json:"authorityReason"`
		Servable        bool   `json:"servable"`
		ProvenanceKind  string `json:"provenanceKind"`
	} `json:"items"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

type memoryRebuildEnvelope struct {
	RepoID           string `json:"repoId"`
	Generation       int64  `json:"generation"`
	Skipped          bool   `json:"skipped"`
	SkipReason       string `json:"skipReason"`
	FilesIndexed     int    `json:"filesIndexed"`
	FilesSkipped     int    `json:"filesSkipped"`
	ItemsWritten     int    `json:"itemsWritten"`
	ItemsReconfirmed int    `json:"itemsReconfirmed"`
	ItemsRetired     int64  `json:"itemsRetired"`
	IndexedCommit    string `json:"indexedCommit"`
	Truncated        bool   `json:"truncated"`
	TruncatedReason  string `json:"truncatedReason"`
}

type memoryInvalidateEnvelope struct {
	RepoID           string `json:"repoId"`
	ItemsInvalidated int64  `json:"itemsInvalidated"`
	DriftChecked     int    `json:"driftChecked"`
	DriftFound       int    `json:"driftFound"`
}

func newMemoryCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect and repair AO's durable project memory",
		Long: "Project memory is a bounded, provenance-carrying summary AO keeps of a project so the Planner, Worker,\n" +
			"Reviewer and Repair agents do not re-derive the same facts on every task.\n\n" +
			"It is a CACHE, never a source of truth. Where memory and the working tree disagree, the working tree is\n" +
			"right and the memory is out of date — which is why every fact carries the commit and the file digests it\n" +
			"was derived from, and why anything AO cannot currently prove is withheld rather than served.",
	}
	cmd.AddCommand(newMemoryStatusCommand(ctx))
	cmd.AddCommand(newMemoryInspectCommand(ctx))
	cmd.AddCommand(newMemoryRebuildCommand(ctx))
	cmd.AddCommand(newMemoryInvalidateCommand(ctx))
	cmd.AddCommand(newMemoryValidateCommand(ctx))
	cmd.AddCommand(newMemoryProvenanceCommand(ctx))
	cmd.AddCommand(newMemoryPruneCommand(ctx))
	cmd.AddCommand(newMemoryReportCommand(ctx))
	cmd.AddCommand(newMemoryKnowledgeCommand(ctx))
	cmd.AddCommand(newMemoryDecisionsCommand(ctx))
	cmd.AddCommand(newMemoryRisksCommand(ctx))
	cmd.AddCommand(newMemoryTaskCommand(ctx))
	cmd.AddCommand(newMemoryContextCommand(ctx))
	return cmd
}

type memoryReportEnvelope struct {
	Mode          string `json:"mode"`
	CacheEnabled  bool   `json:"cacheEnabled"`
	SyncTimeout   string `json:"syncTimeout"`
	RepoID        string `json:"repoId"`
	RepoPath      string `json:"repoPath"`
	Warm          bool   `json:"warm"`
	Generation    int64  `json:"generation"`
	IndexedCommit string `json:"indexedCommit"`
	SyncKind      string `json:"syncKind"`
	SyncReason    string `json:"syncReason"`
	SyncFilesRead int    `json:"syncFilesRead"`
	SyncMillis    int64  `json:"syncMillis"`
	Roles         []struct {
		Role                string `json:"role"`
		BudgetBytes         int    `json:"budgetBytes"`
		BudgetItems         int    `json:"budgetItems"`
		BudgetDocuments     int    `json:"budgetDocuments"`
		PackItems           int    `json:"packItems"`
		PackBytes           int    `json:"packBytes"`
		EstimatedPackTokens int    `json:"estimatedPackTokens"`
		Candidates          int    `json:"candidates"`
		RejectedByBudget    int    `json:"rejectedByBudget"`
		ReducedToSummary    int    `json:"reducedToSummary"`
		StaleExcluded       int    `json:"staleExcluded"`
		FallbackReason      string `json:"fallbackReason"`
	} `json:"roles"`
	CacheHits   int64 `json:"cacheHits"`
	CacheMisses int64 `json:"cacheMisses"`
}

// newMemoryReportCommand is P2-B's answer to "is this actually helping".
//
// It deliberately prints what a dispatch would receive rather than a summary of
// the store: the daemon assembles each role's pack through the same provisioner
// the wrappers use, so an operator and an agent are looking at one number.
func newMemoryReportCommand(ctx *commandContext) *cobra.Command {
	var repoPath string
	cmd := &cobra.Command{
		Use:   "report <project-id>",
		Short: "Show whether a project is warm and what memory costs each role",
		Long: "Runs the ordinary lifecycle freshness check and then assembles each role's context pack exactly as a\n" +
			"dispatch would, so the numbers printed are the ones agents actually receive rather than an estimate.\n\n" +
			"A warm project reports sync=none and 0 files read: memory was already at the repository's current\n" +
			"commit, which is the whole point of the optimisation. A cold or moved project reports the incremental\n" +
			"or full pass it had to run instead.\n\n" +
			"Token figures are ESTIMATES at four bytes per token. AO does not have the provider's tokenizer, and a\n" +
			"number presented as exact would be wrong in a way nobody could audit.\n\n" +
			"Everything here is AO-ASSEMBLED context only. AO does not observe what a coding harness reads inside\n" +
			"the worktree, so nothing printed is a count of agent-side reads avoided.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "projects/" + url.PathEscape(args[0]) + "/memory/report"
			if strings.TrimSpace(repoPath) != "" {
				path += "?" + url.Values{"repoPath": {repoPath}}.Encode()
			}
			var res memoryReportEnvelope
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "mode:        %s (cache %s, sync timeout %s)\n",
				res.Mode, enabledWord(res.CacheEnabled), orDash(res.SyncTimeout))
			if res.Mode == "off" {
				_, _ = fmt.Fprintln(out, "\nProject memory is switched off; nothing is attached to any dispatch.")
				_, _ = fmt.Fprintln(out, "Set AO_MEMORY_MODE=assisted to have AO attach a bounded memory pack.")
				return nil
			}
			_, _ = fmt.Fprintf(out, "repository:  %s\n", orDash(res.RepoPath))
			_, _ = fmt.Fprintf(out, "state:       %s at %s (generation %d)\n",
				warmWord(res.Warm), orDash(res.IndexedCommit), res.Generation)
			_, _ = fmt.Fprintf(out, "last sync:   %s, %d files read, %dms\n",
				orDash(res.SyncKind), res.SyncFilesRead, res.SyncMillis)
			if res.SyncReason != "" {
				_, _ = fmt.Fprintf(out, "             %s\n", res.SyncReason)
			}
			_, _ = fmt.Fprintf(out, "pack cache:  %d hits, %d misses\n\n", res.CacheHits, res.CacheMisses)

			_, _ = fmt.Fprintf(out, "%-9s %-24s %-24s %s\n", "role", "selected", "budget", "excluded by budget")
			for _, r := range res.Roles {
				selected := fmt.Sprintf("%d items / %dB / ~%dt", r.PackItems, r.PackBytes, r.EstimatedPackTokens)
				budget := fmt.Sprintf("%d items / %dB / %d docs", r.BudgetItems, r.BudgetBytes, r.BudgetDocuments)
				excluded := fmt.Sprintf("%d of %d", r.RejectedByBudget, r.Candidates)
				if r.ReducedToSummary > 0 {
					excluded += fmt.Sprintf(" (+%d to summary)", r.ReducedToSummary)
				}
				_, _ = fmt.Fprintf(out, "%-9s %-24s %-24s %s\n", r.Role, selected, budget, excluded)
				if r.FallbackReason != "" {
					_, _ = fmt.Fprintf(out, "          fallback: %s\n", r.FallbackReason)
				}
				if r.StaleExcluded > 0 {
					_, _ = fmt.Fprintf(out, "          %d facts withheld because AO can no longer vouch for them\n", r.StaleExcluded)
				}
			}
			_, _ = fmt.Fprintln(out, "\nToken figures are estimates, and cover AO-assembled context only.")
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to report on (defaults to the project's own root)")
	return cmd
}

func enabledWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func warmWord(warm bool) string {
	if warm {
		return "warm"
	}
	return "cold or moved"
}

func newMemoryStatusCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "status <project-id>",
		Short: "Show what AO remembers about a project and whether it can vouch for it",
		Long: "Reports, per repository: the memory generation, the commit it was derived from, the per-state fact\n" +
			"census, and whether an indexing pass is running right now.\n\n" +
			"A repository that does not appear has never been indexed. A repository whose phase is not 'idle' has a\n" +
			"pass in flight — or one that died, which the next pass resumes from its recorded cursor.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res memoryStatusEnvelope
			if err := ctx.getJSON(cmd.Context(), "projects/"+url.PathEscape(args[0])+"/memory", &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Repositories) == 0 {
				_, _ = fmt.Fprintln(out, "No repository of this project has been indexed yet.")
				_, _ = fmt.Fprintln(out, "Run `ao memory rebuild "+args[0]+"` to build it.")
				return nil
			}
			for i, r := range res.Repositories {
				if i > 0 {
					_, _ = fmt.Fprintln(out)
				}
				_, _ = fmt.Fprintf(out, "%s\n", r.RepoPath)
				_, _ = fmt.Fprintf(out, "  generation:  %d (%s)\n", r.Generation, healthWord(r.Healthy))
				_, _ = fmt.Fprintf(out, "  indexed at:  %s%s\n", orDash(r.IndexedCommit), branchSuffix(r.Branch))
				_, _ = fmt.Fprintf(out, "  phase:       %s%s\n", r.Phase, cursorSuffix(r.Phase, r.ResumeCursor))
				_, _ = fmt.Fprintf(out, "  facts:       %d total — %d valid, %d stale, %d invalidated, %d rebuilding\n",
					r.Items, r.Valid, r.Stale, r.Invalidated, r.Rebuilding)
				if r.TaskLocal > 0 {
					_, _ = fmt.Fprintf(out, "               %d task-local (one task's unintegrated view; not canonical)\n", r.TaskLocal)
				}
				_, _ = fmt.Fprintf(out, "  relations:   %d\n", r.Relations)
				_, _ = fmt.Fprintf(out, "  last pass:   %d files indexed, %d unchanged and skipped\n", r.FilesIndexed, r.FilesSkipped)
				_, _ = fmt.Fprintf(out, "  last index:  %s\n", orDash(deref(r.LastIndexedAt)))
				_, _ = fmt.Fprintf(out, "  last change: %s\n", orDash(deref(r.LastUpdatedAt)))
				if r.LastError != "" {
					_, _ = fmt.Fprintf(out, "  last error:  %s\n", r.LastError)
					_, _ = fmt.Fprintln(out, "               this memory is NOT being served to agents until a pass succeeds")
				}
			}
			return nil
		},
	}
}

func newMemoryInspectCommand(ctx *commandContext) *cobra.Command {
	var repoPath, state, itemType, pathPrefix string
	var limit int
	cmd := &cobra.Command{
		Use:   "inspect <project-id>",
		Short: "List the individual facts in a project's memory",
		Long: "Unlike a context pack, an inspect shows the facts AO can no longer vouch for as well as the ones it\n" +
			"can — seeing what went stale, and why, is the point.\n\n" +
			"Bodies are not printed: this answers what AO remembers and whether it still holds, not what each fact\n" +
			"says in full.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			for key, val := range map[string]string{
				"repoPath": repoPath, "state": state, "type": itemType, "path": pathPrefix,
			} {
				if strings.TrimSpace(val) != "" {
					q.Set(key, val)
				}
			}
			if limit > 0 {
				q.Set("limit", fmt.Sprint(limit))
			}
			path := "projects/" + url.PathEscape(args[0]) + "/memory/items"
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			var res memoryItemsEnvelope
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Items) == 0 {
				_, _ = fmt.Fprintln(out, "No matching facts.")
				return nil
			}
			withheld := 0
			for _, it := range res.Items {
				// The marker is driven by SERVABLE rather than by state alone.
				// A fact whose files are untouched but whose licence AO can no
				// longer prove is not being handed to anyone, and printing it
				// unmarked beside "valid" would be the one misleading thing an
				// inspect could do.
				marker := " "
				if !it.Servable {
					marker = "!"
					withheld++
				}
				_, _ = fmt.Fprintf(out, "%s %-18s %-10s %-8s conf %.2f gen %d  %s\n",
					marker, it.Type, it.Scope, it.State, it.Confidence, it.Generation, it.Summary)
				if it.StateReason != "" {
					_, _ = fmt.Fprintf(out, "    reason: %s\n", it.StateReason)
				}
				if it.AuthorityReason != "" {
					_, _ = fmt.Fprintf(out, "    authority: %s — %s\n", it.Authority, it.AuthorityReason)
				}
				if it.Origin == "task_local" {
					_, _ = fmt.Fprintf(out, "    task-local to %s (not part of the project's canonical memory)\n", it.OriginRef)
				}
			}
			_, _ = fmt.Fprintf(out, "\n%d facts", res.Total)
			if withheld > 0 {
				_, _ = fmt.Fprintf(out, ", %d of them withheld (marked !); `ao memory provenance` explains one", withheld)
			}
			if res.Truncated {
				_, _ = fmt.Fprintf(out, " (showing the first %d; raise --limit for more)", len(res.Items))
			}
			_, _ = fmt.Fprintln(out)
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to inspect (defaults to the project's own root)")
	cmd.Flags().StringVar(&state, "state", "", "Only facts in this state: valid, stale, invalidated, rebuilding")
	cmd.Flags().StringVar(&itemType, "type", "", "Only facts of this type (module, convention, architecture, ...)")
	cmd.Flags().StringVar(&pathPrefix, "path", "", "Only facts about this repo-relative path prefix")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum facts to list (default 200)")
	return cmd
}

func newMemoryRebuildCommand(ctx *commandContext) *cobra.Command {
	var repoPath string
	var purge bool
	cmd := &cobra.Command{
		Use:   "rebuild <project-id>",
		Short: "Re-derive a repository's project memory",
		Long: "Runs a full bounded pass at the repository's current checkout state. The pass is restart-safe: if it\n" +
			"dies, the next one resumes from the path it had reached rather than starting over.\n\n" +
			"By default the existing facts are kept and re-derived over, so identities and creation times survive.\n" +
			"--purge deletes them first, which is the escape hatch for memory that is wrong in a way a re-derivation\n" +
			"cannot fix. --purge also discards recorded task outcomes for that repository.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"repoPath": repoPath, "purge": purge}
			var res memoryRebuildEnvelope
			if err := ctx.postJSON(cmd.Context(), "projects/"+url.PathEscape(args[0])+"/memory/rebuild", body, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.Skipped {
				_, _ = fmt.Fprintf(out, "Skipped: %s\n", res.SkipReason)
				return nil
			}
			_, _ = fmt.Fprintf(out, "generation %d at %s\n", res.Generation, orDash(res.IndexedCommit))
			_, _ = fmt.Fprintf(out, "  %d files indexed, %d unchanged and skipped\n", res.FilesIndexed, res.FilesSkipped)
			_, _ = fmt.Fprintf(out, "  %d facts written, %d reconfirmed unchanged, %d retired\n",
				res.ItemsWritten, res.ItemsReconfirmed, res.ItemsRetired)
			if res.Truncated {
				_, _ = fmt.Fprintf(out, "  bounded: %s — this memory covers less than the whole repository\n", res.TruncatedReason)
			}
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to rebuild (defaults to the project's own root)")
	cmd.Flags().BoolVar(&purge, "purge", false, "Delete the existing facts before re-deriving")
	return cmd
}

func newMemoryInvalidateCommand(ctx *commandContext) *cobra.Command {
	var repoPath, reason string
	var paths []string
	cmd := &cobra.Command{
		Use:   "invalidate <project-id>",
		Short: "Retire memory that can no longer be proven current",
		Long: "With --path, retires exactly what those paths proved.\n\n" +
			"With no --path, runs drift detection instead: AO recomputes the digest of every fact's sources and\n" +
			"demotes the ones whose sources moved. That is the honest repair for \"something changed and I cannot\n" +
			"tell you what\" — far better than invalidating everything, which would force a full re-derivation of\n" +
			"facts that are still perfectly good.\n\n" +
			"Nothing is deleted. A retired fact stays readable, because knowing that it went stale (and why) is\n" +
			"information, and re-deriving from it is cheaper than from nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"repoPath": repoPath, "paths": paths, "reason": reason}
			var res memoryInvalidateEnvelope
			if err := ctx.postJSON(cmd.Context(), "projects/"+url.PathEscape(args[0])+"/memory/invalidate", body, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(paths) == 0 {
				_, _ = fmt.Fprintf(out, "checked %d facts against their sources; %d had drifted\n",
					res.DriftChecked, res.DriftFound)
			}
			_, _ = fmt.Fprintf(out, "%d facts are no longer served as authoritative\n", res.ItemsInvalidated)
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to the project's own root)")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "Repo-relative path whose facts to retire (repeatable)")
	cmd.Flags().StringVar(&reason, "reason", "", "Why, recorded on every fact this retires")
	return cmd
}

// normalizeDashedFlags accepts snake_case as well as kebab-case flag names,
// matching the convention the capacity and runtime commands already set.
func normalizeDashedFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
	})
}

func healthWord(healthy bool) string {
	if healthy {
		return "usable"
	}
	return "not vouched for"
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func branchSuffix(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return ""
	}
	return " on " + branch
}

// cursorSuffix explains a non-idle phase, because "scanning" on its own reads
// as "in progress" when it may mean "a pass died here and the next one will
// pick up from this path".
func cursorSuffix(phase, cursor string) string {
	if phase == "idle" || strings.TrimSpace(cursor) == "" {
		return ""
	}
	return " (resume point: " + cursor + ")"
}
