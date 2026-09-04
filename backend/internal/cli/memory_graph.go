package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// memory_graph.go — the code graph's operator commands.
//
//	ao memory graph status <project>   which backend, which generation, how big, and does it drift
//	ao memory graph sync <project>     bring it to the checkout's current commit
//	ao memory graph query <project>    ask it what a dispatch would be told
//
// Three verbs and no more, for the reason section 33 of the brief gives: the
// point is diagnostics, not a graph browser. Anything that would need paging,
// traversal or a layout belongs in a tool nobody has asked for yet.
//
// Like the rest of `ao memory`, the CLI classifies nothing itself. Health,
// drift and the choice between an incremental and a full pass are all decided
// by the daemon and printed here, so an operator's view and the daemon's can
// never disagree.

type memoryGraphStatusEnvelope struct {
	Repositories []struct {
		RepoID        string `json:"repoId"`
		RepoPath      string `json:"repoPath"`
		Backend       string `json:"backend"`
		Generation    int64  `json:"generation"`
		Phase         string `json:"phase"`
		IndexedCommit string `json:"indexedCommit"`
		RepoIdentity  string `json:"repoIdentity"`
		Files         int64  `json:"files"`
		Symbols       int64  `json:"symbols"`
		Edges         int64  `json:"edges"`
		LastSyncKind  string `json:"lastSyncKind"`
		FilesParsed   int64  `json:"filesParsed"`
		FilesReused   int64  `json:"filesReused"`
		FilesRemoved  int64  `json:"filesRemoved"`
		LastMillis    int64  `json:"lastMillis"`
		LastError     string `json:"lastError"`
		Architecture  string `json:"architecture"`
		UpdatedAt     string `json:"updatedAt"`
		Healthy       bool   `json:"healthy"`
		Drift         string `json:"drift"`
	} `json:"repositories"`
}

type memoryGraphSyncEnvelope struct {
	RepoID         string `json:"repoId"`
	RepoPath       string `json:"repoPath"`
	Kind           string `json:"kind"`
	Generation     int64  `json:"generation"`
	IndexedCommit  string `json:"indexedCommit"`
	FilesScanned   int    `json:"filesScanned"`
	FilesParsed    int    `json:"filesParsed"`
	FilesReused    int    `json:"filesReused"`
	FilesRemoved   int    `json:"filesRemoved"`
	SymbolsAdded   int    `json:"symbolsAdded"`
	SymbolsRemoved int    `json:"symbolsRemoved"`
	EdgesAdded     int    `json:"edgesAdded"`
	EdgesRemoved   int    `json:"edgesRemoved"`
	Files          int    `json:"files"`
	Symbols        int    `json:"symbols"`
	Edges          int    `json:"edges"`
	Millis         int64  `json:"millis"`
	Truncated      bool   `json:"truncated"`
	Reason         string `json:"reason"`
}

type memoryGraphSymbol struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Path      string  `json:"path"`
	Line      int     `json:"line"`
	Signature string  `json:"signature"`
	Summary   string  `json:"summary"`
	Exported  bool    `json:"exported"`
	Score     float64 `json:"score"`
	Reason    string  `json:"reason"`
}

type memoryGraphEdge struct {
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
	Line int    `json:"line"`
}

type memoryGraphAnswerEnvelope struct {
	RepoID            string              `json:"repoId"`
	Generation        int64               `json:"generation"`
	IndexedCommit     string              `json:"indexedCommit"`
	Symbols           []memoryGraphSymbol `json:"symbols"`
	Callers           []memoryGraphEdge   `json:"callers"`
	Callees           []memoryGraphEdge   `json:"callees"`
	Tests             []memoryGraphSymbol `json:"tests"`
	Endpoints         []memoryGraphSymbol `json:"endpoints"`
	Tables            []string            `json:"tables"`
	Files             []string            `json:"files"`
	ConsideredSymbols int                 `json:"consideredSymbols"`
	ConsideredEdges   int                 `json:"consideredEdges"`
	Truncated         bool                `json:"truncated"`
	Reason            string              `json:"reason"`
}

func newMemoryGraphCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Inspect and refresh the structural code graph behind project memory",
		Long: "The code graph is the structural half of project memory: the symbols a repository declares and the\n" +
			"relations between them, so a task can be told where the authorization path lives without being sent\n" +
			"the repository.\n\n" +
			"It is a CACHE like the rest of memory. Every fact carries the file digest and the commit it was\n" +
			"observed at, and a graph whose provenance no longer holds is reported as drifted rather than served\n" +
			"as current.\n\n" +
			"One registered project has exactly ONE canonical graph, derived from the repository root. A task's\n" +
			"isolated worktree never gets its own.",
	}
	cmd.AddCommand(newMemoryGraphStatusCommand(ctx))
	cmd.AddCommand(newMemoryGraphSyncCommand(ctx))
	cmd.AddCommand(newMemoryGraphQueryCommand(ctx))
	return cmd
}

func newMemoryGraphStatusCommand(ctx *commandContext) *cobra.Command {
	var showArchitecture bool
	cmd := &cobra.Command{
		Use:   "status <project-id>",
		Short: "Show the code graph's backend, generation, size and drift",
		Long: "Prints one line per repository: which backend is serving the graph, which generation and commit it is\n" +
			"at, how many files, symbols and relations it holds, and what the last sync had to do.\n\n" +
			"The backend is reported by its real name. The in-tree implementation is \"local\"; it is never printed\n" +
			"under a third-party name, because AO does not ship a third-party graph.\n\n" +
			"A drifted graph is one whose commit or repository identity no longer matches the checkout. Its rows are\n" +
			"intact, but AO cannot prove they describe what is on disk, so it is reported unhealthy until a sync.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res memoryGraphStatusEnvelope
			if err := ctx.getJSON(cmd.Context(), "projects/"+url.PathEscape(args[0])+"/memory/graph", &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Repositories) == 0 {
				_, _ = fmt.Fprintln(out, "No repository of this project has a code graph yet.")
				_, _ = fmt.Fprintln(out, "Run `ao memory graph sync <project>` to build one.")
				return nil
			}
			for i, repo := range res.Repositories {
				if i > 0 {
					_, _ = fmt.Fprintln(out)
				}
				_, _ = fmt.Fprintf(out, "repository:  %s\n", orDash(repo.RepoPath))
				_, _ = fmt.Fprintf(out, "backend:     %s\n", orDash(repo.Backend))
				_, _ = fmt.Fprintf(out, "state:       %s, generation %d at %s\n",
					graphHealthWord(repo.Healthy, repo.Phase), repo.Generation, orDash(repo.IndexedCommit))
				_, _ = fmt.Fprintf(out, "size:        %d files, %d symbols, %d relations\n",
					repo.Files, repo.Symbols, repo.Edges)
				_, _ = fmt.Fprintf(out, "last sync:   %s, %d parsed / %d reused / %d removed, %dms\n",
					orDash(repo.LastSyncKind), repo.FilesParsed, repo.FilesReused, repo.FilesRemoved, repo.LastMillis)
				if repo.Drift != "" {
					_, _ = fmt.Fprintf(out, "drift:       %s\n", repo.Drift)
				}
				if repo.LastError != "" {
					_, _ = fmt.Fprintf(out, "last error:  %s\n", repo.LastError)
				}
				if showArchitecture && repo.Architecture != "" {
					_, _ = fmt.Fprintf(out, "\n%s", repo.Architecture)
				}
			}
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().BoolVar(&showArchitecture, "architecture", false,
		"Also print the bounded structural summary the Planner receives")
	return cmd
}

func newMemoryGraphSyncCommand(ctx *commandContext) *cobra.Command {
	var repoPath string
	var full bool
	cmd := &cobra.Command{
		Use:   "sync <project-id>",
		Short: "Bring a repository's code graph up to its current commit",
		Long: "Chooses between an incremental update and a full build exactly as a dispatch would, so running it by\n" +
			"hand exercises the production path rather than a second one that could drift from it.\n\n" +
			"An incremental sync touches only the paths the commit changed, and of those only the ones whose\n" +
			"content actually moved. The numbers printed are the evidence: parsed is what was read and analysed,\n" +
			"reused is what was left alone.\n\n" +
			"--full forces a rebuild. It is the repair for a graph there is reason to distrust; it stages the new\n" +
			"generation and publishes it only when complete, so readers keep being served the previous one\n" +
			"throughout.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := url.Values{}
			if trimmed := strings.TrimSpace(repoPath); trimmed != "" {
				query.Set("repoPath", trimmed)
			}
			if full {
				query.Set("full", "true")
			}
			path := "projects/" + url.PathEscape(args[0]) + "/memory/graph/sync"
			if encoded := query.Encode(); encoded != "" {
				path += "?" + encoded
			}
			var res memoryGraphSyncEnvelope
			if err := ctx.postJSON(cmd.Context(), path, nil, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "repository:  %s\n", orDash(res.RepoPath))
			_, _ = fmt.Fprintf(out, "sync:        %s, generation %d at %s (%dms)\n",
				orDash(res.Kind), res.Generation, orDash(res.IndexedCommit), res.Millis)
			_, _ = fmt.Fprintf(out, "files:       %d scanned, %d parsed, %d reused, %d removed\n",
				res.FilesScanned, res.FilesParsed, res.FilesReused, res.FilesRemoved)
			_, _ = fmt.Fprintf(out, "symbols:     +%d / -%d    relations: +%d / -%d\n",
				res.SymbolsAdded, res.SymbolsRemoved, res.EdgesAdded, res.EdgesRemoved)
			_, _ = fmt.Fprintf(out, "graph:       %d files, %d symbols, %d relations\n",
				res.Files, res.Symbols, res.Edges)
			if res.Truncated {
				_, _ = fmt.Fprintln(out, "note:        a pass bound was reached; the graph covers part of the repository")
			}
			if res.Reason != "" {
				_, _ = fmt.Fprintf(out, "note:        %s\n", res.Reason)
			}
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to sync (defaults to the project's own root)")
	cmd.Flags().BoolVar(&full, "full", false, "Force a full rebuild instead of applying the diff since the indexed commit")
	return cmd
}

func newMemoryGraphQueryCommand(ctx *commandContext) *cobra.Command {
	var repoPath, path string
	var limit int
	cmd := &cobra.Command{
		Use:   "query <project-id> [terms...]",
		Short: "Ask the code graph what a dispatch would be told",
		Long: "Given a symbol, a file, or free text from an objective, prints the bounded neighbourhood a context pack\n" +
			"would carry: the matching declarations with their signatures and summaries, what reaches them, the\n" +
			"tests proven to cover them, the routes that arrive at them, and the tables they touch.\n\n" +
			"It is the same retrieval a dispatch runs, at the same bounds, so what is printed here is what an agent\n" +
			"would receive — not an approximation of it.\n\n" +
			"Examples:\n" +
			"  ao memory graph query my-project export permissions supervisor\n" +
			"  ao memory graph query my-project --symbol Records.MayExport\n" +
			"  ao memory graph query my-project --path internal/service/records.go",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			symbol, _ := cmd.Flags().GetString("symbol")
			terms := strings.Join(args[1:], " ")
			if strings.TrimSpace(symbol) == "" && strings.TrimSpace(path) == "" && strings.TrimSpace(terms) == "" {
				return usageError{fmt.Errorf("a graph query needs search terms, --symbol, or --path")}
			}
			query := url.Values{}
			for key, value := range map[string]string{
				"repoPath": repoPath, "symbol": symbol, "path": path, "terms": terms,
			} {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					query.Set(key, trimmed)
				}
			}
			if limit > 0 {
				query.Set("limit", strconv.Itoa(limit))
			}
			var res memoryGraphAnswerEnvelope
			if err := ctx.getJSON(cmd.Context(),
				"projects/"+url.PathEscape(args[0])+"/memory/graph/query?"+query.Encode(), &res); err != nil {
				return err
			}
			printGraphAnswer(cmd, res)
			return nil
		},
	}
	normalizeDashedFlags(cmd)
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root to query (defaults to the project's own root)")
	cmd.Flags().String("symbol", "", "A declaration to start from, by name or by full symbol id")
	cmd.Flags().StringVar(&path, "path", "", "A repo-relative file to anchor on")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum symbols to return (0 uses the retrieval default)")
	return cmd
}

func printGraphAnswer(cmd *cobra.Command, res memoryGraphAnswerEnvelope) {
	out := cmd.OutOrStdout()
	if len(res.Symbols) == 0 {
		if res.Reason != "" {
			_, _ = fmt.Fprintln(out, res.Reason)
			return
		}
		_, _ = fmt.Fprintln(out, "The code graph held nothing matching that question.")
		return
	}
	_, _ = fmt.Fprintf(out, "generation %d at %s\n\n", res.Generation, orDash(res.IndexedCommit))
	for _, sym := range res.Symbols {
		_, _ = fmt.Fprintf(out, "%s:%d\n  %s\n", sym.Path, sym.Line, orDash(sym.Summary))
		if sym.Reason != "" {
			_, _ = fmt.Fprintf(out, "  selected because: %s\n", sym.Reason)
		}
	}
	if len(res.Tests) > 0 {
		_, _ = fmt.Fprintln(out, "\ncovered by:")
		for _, test := range res.Tests {
			_, _ = fmt.Fprintf(out, "  %s:%d %s\n", test.Path, test.Line, test.Name)
		}
	}
	if len(res.Endpoints) > 0 {
		_, _ = fmt.Fprintln(out, "\nreached from HTTP:")
		for _, ep := range res.Endpoints {
			_, _ = fmt.Fprintf(out, "  %s (%s)\n", ep.Name, ep.Path)
		}
	}
	if len(res.Callers) > 0 {
		_, _ = fmt.Fprintln(out, "\nreached by:")
		for _, edge := range res.Callers {
			_, _ = fmt.Fprintf(out, "  %s %s %s\n", edge.From, edge.Kind, edge.To)
		}
	}
	if len(res.Tables) > 0 {
		_, _ = fmt.Fprintf(out, "\ntables touched: %s\n", strings.Join(res.Tables, ", "))
	}
	_, _ = fmt.Fprintf(out, "\n%d symbols selected from %d considered", len(res.Symbols), res.ConsideredSymbols)
	if res.Truncated {
		_, _ = fmt.Fprint(out, " (bounded; the graph holds more)")
	}
	_, _ = fmt.Fprintln(out)
}

// graphHealthWord renders the pair an operator needs in one word: is this graph
// servable, and is a build running.
func graphHealthWord(healthy bool, phase string) string {
	switch {
	case phase == "building":
		return "building (the previous generation is still being served)"
	case healthy:
		return "healthy"
	case phase == "failed":
		return "failed"
	default:
		return "not servable"
	}
}
