package recall

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/craft"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/redact"
	"github.com/qiangli/yoke/pkg/scope"
	"github.com/qiangli/yoke/pkg/skills"
)

// ExitBrokenRing is returned when a ring EXISTED but could not be read.
//
// The exit-code contract is the part third parties depend on most, and it has one
// rule that is easy to get wrong: an EMPTY ANSWER IS EXIT 0. A host that knows
// nothing about a topic is a fact, not a failure. Only a ring that was present
// and unreadable is exit 1 — because a caller that cannot distinguish "nothing is
// known" from "something broke" will proceed on a false negative, which is the
// specific harm this package exists to avoid.
const ExitBrokenRing = 1

// NewRecallCmd builds the `recall` verb.
func NewRecallCmd() *cobra.Command {
	var (
		asJSON     bool
		k          int
		budget     int
		resolution string
		rings      []string
		memory     string
		since      time.Duration
		minCover   float64
		explain    bool
		storeDir   string
		lineage    bool
		asOf       string
		hops       int
	)

	cmd := &cobra.Command{
		Use:   "recall <query>...",
		Short: "what is known about a topic, across every memory ring",
		Long: `recall is the third-party READ surface over this host's memory.

It answers "what is known about X" for any tool, not just bashy: ranked hits from
each memory ring, every one carrying its ring label, a re-openable source pointer,
its validation status, and why it matched.

Three things about its behaviour are contract, not implementation:

  EMPTY IS SUCCESS. A host that knows nothing exits 0 with no hits. Only a ring
  that exists and cannot be read exits 1, so a caller can always tell "nothing
  known" from "something broke".

  --k IS PER RING. Scores from different rings are not comparable — a kb page's
  BM25 score is computed over prose lessons, a capability's over contracts — so
  each ring contributes at most k hits ranked within itself and rings are emitted
  in precedence order. Fusing them was measured and rejected.

  FACTS ARE NOT A RING. craft facts are entity-bound and host-local with no export
  path, and that absence is the enforcement. recall's output is designed to be
  piped into software we do not control, so it can never read them — not behind a
  flag, not with --all. A fold derived from facts is the supported path.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// With --memory the flag only narrows the expansion when it was
			// explicitly passed; otherwise the memory row supplies the rings.
			var selected []string
			if cmd.Flags().Changed("ring") {
				selected = cleanList(rings)
			}
			at, err := asOfFlag(asOf)
			if err != nil {
				return err
			}
			if hops < 0 {
				return fmt.Errorf("kb recall: --hops %d: want >= 0", hops)
			}
			q, err := ApplyMemory(Query{
				AsOf: at,
				Text: strings.Join(args, " "),
				K:    k, Budget: budget, Rings: selected, Hops: hops,
				Since: since, MinCoverage: minCover,
				Resolution: parseResolution(resolution),
				Memory:     strings.TrimSpace(memory),
				Lineage:    readerLineage("", lineage),
				OS:         hostOS(),
				Repo:       repoName(),
			})
			if err != nil {
				return err
			}
			res := Recall(q, openRings(storeDir)...)

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return err
				}
			} else {
				render(out, res, explain)
			}
			// Errors are reported in the envelope AND in the exit code: a
			// machine reads the code, a human reads the lines.
			if len(res.Errors) > 0 {
				for _, e := range res.Errors {
					fmt.Fprintf(cmd.ErrOrStderr(), "recall: ring %s: %s\n", e.Ring, e.Err)
				}
				os.Exit(ExitBrokenRing)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVar(&asJSON, "json", false, "emit the recall-v1 envelope")
	f.IntVar(&k, "k", 3, "max hits PER RING (not a global cap — see --help)")
	f.IntVar(&budget, "budget", 2000, "hard ceiling on rendered output, in tokens (0 = none)")
	f.StringVar(&resolution, "resolution", "line", "how much of each record to render: cue|line|full")
	f.StringSliceVar(&rings, "ring", nil, "restrict to these rings (repeatable); default all")
	f.StringVar(&memory, "memory", "", "memory type: working|episodic|semantic|procedural|reflective|profile|code")
	f.DurationVar(&since, "since", 0, "ignore records older than this")
	f.Float64Var(&minCover, "min-coverage", 0, "abstain unless a hit covers this fraction of the query")
	f.BoolVar(&explain, "explain", false, "print the ranking inputs, not just why")
	f.StringVar(&storeDir, "store", "", "override the store root (default ~/.bashy)")
	f.BoolVar(&lineage, "lineage", false, "agent ring: also read descendant episodes' notes")
	f.StringVar(&asOf, "as-of", "", "resolve page validity at this instant (RFC3339 or YYYY-MM-DD; default now)")
	f.IntVar(&hops, "hops", 0, "multi-hop graph expansion depth from ranked hits (0 = direct hits only)")
	return cmd
}

// asOfFlag parses an optional --as-of; empty means now (zero AsOf).
func asOfFlag(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	return ParseAsOf(s)
}

// NewContextCmd builds the budgeted `kb context` stage verb. Extra readers are
// injected by the mounting application (for example cmds/graph's CodeRing), so
// recall never imports codegraph, gfy, or tree-sitter.
func NewContextCmd(extra ...Reader) *cobra.Command {
	var (
		forText  string
		files    []string
		episode  string
		rings    []string
		forms    []string
		memory   string
		budget   int
		minCover float64
		k        int
		asJSON   bool
		lineage  bool
		asOf     string
		hops     int
	)
	cmd := &cobra.Command{
		Use:           "context",
		Short:         "assemble one budgeted context across knowledge rings",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(forText) == "" {
				return fmt.Errorf("kb context: --for is required")
			}
			readers := append(openContextRings(), extra...)
			// With --memory the flags only narrow the expansion when
			// explicitly passed; otherwise the memory row supplies them.
			// An empty intersection is the normal empty answer, never an
			// error, so the resolved selection validates as-is.
			var selectedRings, selectedForms []string
			if strings.TrimSpace(memory) != "" {
				if cmd.Flags().Changed("rings") {
					selectedRings = cleanList(rings)
				}
				if cmd.Flags().Changed("forms") {
					selectedForms = cleanList(forms)
				}
			} else {
				selectedRings, selectedForms = cleanList(rings), cleanList(forms)
			}
			at, err := asOfFlag(asOf)
			if err != nil {
				return fmt.Errorf("kb context: %w", err)
			}
			if hops < 0 {
				return fmt.Errorf("kb context: --hops %d: want >= 0", hops)
			}
			q, err := ApplyMemory(Query{
				AsOf: at,
				Text: strings.TrimSpace(forText), Files: cleanList(files), Episode: strings.TrimSpace(episode),
				Rings: selectedRings, Forms: selectedForms, Budget: budget, MinCoverage: minCover, K: k, Hops: hops,
				Memory:  strings.TrimSpace(memory),
				Lineage: readerLineage(episode, lineage),
				OS:      hostOS(), Repo: repoName(),
			})
			if err != nil {
				return err
			}
			// A row that constrains no rings (code) leaves Rings empty to
			// mean every reader; validate its forms against all of them.
			validationRings := q.Rings
			if len(validationRings) == 0 && strings.TrimSpace(memory) != "" && !cmd.Flags().Changed("rings") {
				for _, rd := range readers {
					if !slices.Contains(validationRings, rd.Ring()) {
						validationRings = append(validationRings, rd.Ring())
					}
				}
			}
			if err := validateContextSelection(validationRings, q.Forms, readers); err != nil {
				return err
			}
			res := Context(q, readers...)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return err
				}
			} else {
				renderContext(cmd.OutOrStdout(), res)
			}
			for _, ring := range res.Rings {
				if !ring.OK {
					return fmt.Errorf("kb context: ring %s: %s", ring.Name, ring.Error)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&forText, "for", "", "task text to assemble context for (required)")
	f.StringSliceVar(&files, "files", nil, "task files exposed as cues to injected readers")
	f.StringVar(&episode, "episode", "", "include checkpoint-tagged agent notes from this episode")
	f.StringSliceVar(&rings, "rings", []string{RingRepo, RingHost}, "rings to read: agent,repo,host,capability")
	f.StringSliceVar(&forms, "forms", []string{kb.FormNote, kb.FormPage}, "forms to read: note,page,relation,code")
	f.StringVar(&memory, "memory", "", "memory type: working|episodic|semantic|procedural|reflective|profile|code (sets default rings/forms)")
	f.IntVar(&budget, "budget", PreambleBudget, "hard ceiling on assembled output, in estimated tokens")
	f.Float64Var(&minCover, "min-coverage", 0, "abstain unless a hit covers this fraction of the query")
	f.IntVar(&k, "k", 3, "max hits per ring")
	f.BoolVar(&asJSON, "json", false, "emit the frozen kb-context-v1 envelope")
	f.BoolVar(&lineage, "lineage", false, "agent ring: also read descendant episodes' notes")
	f.StringVar(&asOf, "as-of", "", "resolve page validity at this instant (RFC3339 or YYYY-MM-DD; default now)")
	f.IntVar(&hops, "hops", 0, "multi-hop graph expansion depth from ranked hits (0 = direct hits only)")
	return cmd
}

// readerLineage places the reader in the episode chain: --episode when given,
// else this process's own episode. The parent edge (BASHY_PARENT_EPISODE) is
// known only for the process's own episode.
func readerLineage(episode string, descendants bool) Lineage {
	self := strings.TrimSpace(episode)
	ambient := kb.EpisodeID()
	if self == "" {
		self = ambient
	}
	l := Lineage{Self: self, Descendants: descendants}
	if self == ambient {
		l.Parent = kb.ParentEpisodeID()
	}
	return l
}

func openContextRings() []Reader {
	repoRoot, ok := scope.FindGitRoot()
	if !ok {
		repoRoot, _ = os.Getwd()
	}
	repoDir := filepath.Join(repoRoot, kb.RepoSub)
	hostDir := kb.DefaultDir()
	agentDir := kb.AgentRingDir()
	if agentScope, err := scope.Resolve(scope.Options{
		AgentDir:   func() (string, error) { return kb.AgentRingDir(), nil },
		ForceAgent: true,
	}); err == nil && agentScope.Kind == scope.KindAgent {
		agentDir = agentScope.Dir()
	}
	skillsDir := skills.DefaultStoreDir()
	return []Reader{
		AgentRing{Store: kb.OpenAgentRing(agentDir, kb.ToolID()), Path: agentDir, Required: true},
		RelationRing{RingName: RingAgent, Path: agentDir, Owner: kb.ToolID()},
		RepoRing{Store: kb.Open(repoDir), Path: repoDir},
		RelationRing{RingName: RingRepo, Path: repoDir},
		HostRing{Store: kb.Open(hostDir), Path: hostDir},
		RelationRing{RingName: RingHost, Path: hostDir},
		CapabilityRing{Folds: craft.OpenFolds(skillsDir, redact.New()), Path: skillsDir},
	}
}

func validateContextSelection(rings, forms []string, readers []Reader) error {
	selected := cleanList(rings)
	availableRings := map[string]bool{}
	for _, rd := range readers {
		availableRings[rd.Ring()] = true
	}
	for _, ring := range selected {
		if !availableRings[ring] {
			return fmt.Errorf("kb context: unknown ring %q (agent|repo|host|capability)", ring)
		}
	}
	for _, form := range cleanList(forms) {
		available := false
		for _, rd := range readers {
			if slices.Contains(selected, rd.Ring()) && slices.Contains(rd.Forms(), form) {
				available = true
				break
			}
		}
		if !available {
			return fmt.Errorf("kb: invalid form %q (note|page|relation|code)", form)
		}
	}
	return nil
}

func cleanList(in []string) []string {
	var out []string
	for _, value := range in {
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" && !slices.Contains(out, item) {
				out = append(out, item)
			}
		}
	}
	return out
}

// openRings resolves recall's readable rings. A MISSING store is not an error
// on this historical surface; context has the stricter requested-ring contract.
func openRings(root string) []Reader {
	var rs []Reader
	kbDir := kb.DefaultDir()
	craftDir := skills.DefaultStoreDir()
	if root != "" {
		kbDir = filepath.Join(root, "kb")
		craftDir = filepath.Join(root, "skills")
	}
	if isDir(kbDir) {
		rs = append(rs, HostRing{Store: kb.Open(kbDir)})
	}
	if isDir(craftDir) {
		rs = append(rs, CapabilityRing{Folds: craft.OpenFolds(craftDir, redact.New())})
	}
	return rs
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func parseResolution(s string) kb.Resolution {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cue":
		return kb.ResCue
	case "full":
		return kb.ResFull
	default:
		return kb.ResLine
	}
}

func hostOS() string { return osGOOS }

func repoName() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for d := wd; ; {
		if isDir(filepath.Join(d, ".git")) {
			return filepath.Base(d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// render writes the human view. Deliberately citation-shaped: every line carries
// its ring and id so a reader can go open the source. There is no --summarise and
// there must not be one — that is the flag that turns a citation tool into a
// confabulation surface.
func render(w io.Writer, res Result, explain bool) {
	if res.Abstained {
		fmt.Fprintf(w, "nothing is known about %q above the coverage threshold\n", res.Query)
		return
	}
	if len(res.Hits) == 0 {
		fmt.Fprintf(w, "nothing is known about %q\n", res.Query)
		return
	}
	ring := ""
	for _, h := range res.Hits {
		if h.Ring != ring {
			ring = h.Ring
			fmt.Fprintf(w, "\n%s\n", ring)
		}
		fmt.Fprintf(w, "  %-44s %s\n", truncate(h.Cue, 44), h.ID)
		if h.Gist != "" {
			fmt.Fprintf(w, "  %-44s %s\n", "", truncate(h.Gist, 60))
		}
		if explain {
			fmt.Fprintf(w, "  %-44s score %.2f  why: %s\n", "", h.Score, strings.Join(h.Why, ", "))
			for _, s := range h.Source {
				fmt.Fprintf(w, "  %-44s source %s\n", "", s.URI)
			}
		}
		if h.Compose != "" {
			fmt.Fprintf(w, "  %-44s → %s\n", "", h.Compose)
		}
	}
	if res.Budget.Truncated {
		fmt.Fprintf(w, "\n(truncated at %d/%d tokens — raise --budget for more)\n",
			res.Budget.Spent, res.Budget.Tokens)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
