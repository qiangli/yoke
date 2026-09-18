// Package kb is the host-scope shared knowledge base for agents — the
// collective memory of every agent tool working on this machine, across all
// repositories. It fills the ring between the per-repo stores (weave
// campaign memory, the graph contribution log) and the org/cloud tier: one
// wiki of small OKF-style markdown pages (frontmatter + distilled body)
// under ~/.bashy/kb, shared by claude/codex/opencode/… alike.
//
// The cooperative loop it exists for: an agent SEARCHES the kb before
// undertaking a task; if nothing relevant exists it CONTRIBUTES a candidate
// entry; after the task it runs a RETRO — validate or correct what it
// consulted (add / update / supersede / validate / noop — never
// blind-append).
//
// It is part of the AgentOS hub (consumed by bashy as `bashy kb`),
// standalone-first: no cloudbox, no network, no LLM — deterministic
// substring/tag retrieval with a small default K (precision over recall).
// The store is plain files, so agents without the CLI can grep it directly;
// index.md is the entry point.
package kb

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/scope"
)

// resolveKBDir picks the kb store directory using the shared scope resolver and
// reports a one-word scope label ("repo" | "user" | "dir"). Precedence: an
// explicit --dir wins; then $BASHY_KB_DIR forces the host store (tests, relocated
// homes) unless --repo/--base-dir asked otherwise; otherwise auto-detect (this
// repo's docs/kb/ inside a git repo, else the host store).
func resolveKBDir(dir *string, forceRepo, forceUser, forceAgent bool, baseDir string) (string, error) {
	if strings.TrimSpace(*dir) != "" {
		return "dir", nil
	}
	// The AGENT ring is explicit (--ring agent) and owner-only: BASHY_KB_DIR
	// (which forces the host store for tests/relocated homes) must not divert
	// it, so the env shortcut is skipped when --ring agent is asked for.
	if env := strings.TrimSpace(os.Getenv("BASHY_KB_DIR")); env != "" && baseDir == "" && !forceRepo && !forceAgent {
		*dir = env
		return "user", nil
	}
	sc, err := scope.Resolve(scope.Options{
		RepoSub:    RepoSub,
		HostDir:    func() (string, error) { return DefaultDir(), nil },
		AgentDir:   func() (string, error) { return AgentRingDir(), nil },
		ForceRepo:  forceRepo,
		ForceUser:  forceUser,
		ForceAgent: forceAgent,
		BaseDir:    baseDir,
	})
	if err != nil {
		return "", err
	}
	*dir = sc.Dir()
	return string(sc.Kind), nil
}

// NewKBCmd returns the `kb` cobra command tree — the host-agnostic entry
// point a front end mounts (e.g. `bashy kb`).
// openRing opens the store for the resolved ring. On the agent ring it scopes
// reads to the calling principal (ToolID) so another principal sees nothing;
// every other ring is unscoped.
func openRing(dir, ring string) *Store {
	if ring == string(scope.KindAgent) {
		return OpenAgentRing(dir, ToolID())
	}
	return Open(dir)
}

func NewKBCmd() *cobra.Command {
	var dir, baseDir, ring string
	var forceRepo, forceUser bool
	// resolved holds the ring the store was resolved to (repo|user|agent|dir),
	// shared with every subcommand so the agent ring's owner-only read scope is
	// applied uniformly.
	resolved := new(string)
	cmd := &cobra.Command{
		Use:   "kb",
		Short: "Knowledge base for agents — auto: repo docs/kb/ if in a git repo, else your host store",
		Long: `kb is agent memory as a wiki of small markdown pages (YAML frontmatter + a
distilled body). Like todo, the SCOPE is git-repo aware, so no flag is needed
for the common case:

  in a git repo   → THAT repo's docs/kb/ (committed, travels with the clone) —
                    knowledge TRUE OF THIS REPO; the structured replacement for
                    ad-hoc docs/*.md notes.
  not in a repo   → the host store (~/.bashy/kb, $BASHY_KB_DIR to override) —
                    cross-repo / this-machine knowledge, shared by every agent.

Overrides: --base-dir <root> reads ANOTHER project's store (<root>/docs/kb/) so
one agent can travel repos in a session; --user forces the host store even inside
a repo; --repo forces the repo store; --dir <path> points at any store directly.
'kb search --federate' additionally reads the current repo's graph/weave rings.

The loop: SEARCH before undertaking a task; if nothing relevant, ADD a
candidate entry; after the task, RETRO — validate or correct what you
consulted (update / supersede / validate), never blind-append.

Write distilled strategy, not transcripts. Capture failures as guardrails
("X looks right but Y"). The description field is the routing surface —
phrase it as "what + WHEN this applies" with trigger keywords.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Resolve the scope ONCE, before any subcommand runs, and populate `dir`
		// so every subcommand's Open(*dir) lands on the right store. A header on
		// stderr names WHICH store, so which kb you are on is never in doubt.
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			// --ring is the ring selector; it maps onto the existing force
			// flags. A ring is exactly one store — a write lands in one place.
			forceAgent := false
			switch strings.TrimSpace(ring) {
			case "":
				// no override — --repo/--user/auto-detect decide
			case "repo":
				forceRepo = true
			case "host":
				forceUser = true
			case "agent":
				forceAgent = true
			default:
				return fmt.Errorf("kb: unknown ring %q (repo|host|agent)", ring)
			}
			label, err := resolveKBDir(&dir, forceRepo, forceUser, forceAgent, baseDir)
			if err != nil {
				return err
			}
			*resolved = label
			fmt.Fprintf(c.ErrOrStderr(), "kb [%s] %s\n", label, dir)
			return nil
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "point at a kb store directory directly (bypasses scope detection)")
	cmd.PersistentFlags().StringVar(&ring, "ring", "", "select the store ring: repo | host | agent (default: repo in a git repo, else host; agent is the owner-only per-identity store)")
	cmd.PersistentFlags().BoolVar(&forceRepo, "repo", false, "force THIS repo's committed store (docs/kb/); error if not in a git repo")
	cmd.PersistentFlags().BoolVar(&forceUser, "user", false, "force the host store (~/.bashy/kb), even inside a repo")
	cmd.PersistentFlags().StringVar(&baseDir, "base-dir", "", "read ANOTHER project root's store (<root>/docs/kb/) — travel repos without cd")

	cmd.AddCommand(newSearchCmd(&dir, resolved))
	cmd.AddCommand(newShowCmd(&dir, resolved))
	cmd.AddCommand(newAddCmd(&dir, resolved))
	cmd.AddCommand(newNoteCmd(&dir, resolved))
	cmd.AddCommand(newObserveCmd(&dir, resolved))
	cmd.AddCommand(newUpdateCmd(&dir, resolved))
	cmd.AddCommand(newSupersedeCmd(&dir, resolved))
	cmd.AddCommand(newValidateCmd(&dir, resolved))
	cmd.AddCommand(newRetroCmd(&dir, resolved))
	cmd.AddCommand(newSourcesCmd(&dir))
	cmd.AddCommand(newTransferCmd(&dir))
	cmd.AddCommand(newListCmd(&dir, resolved))
	cmd.AddCommand(newIndexCmd(&dir, resolved))
	cmd.AddCommand(newLogCmd(&dir, resolved))
	cmd.AddCommand(newBacklinksCmd(&dir, resolved))
	cmd.AddCommand(newDoctorCmd(&dir, resolved))
	return cmd
}

// --- search --------------------------------------------------------------

func newSearchCmd(dir, ring *string) *cobra.Command {
	var (
		repo, goos     string
		tags           []string
		k              int
		all, jsonOut   bool
		full, federate bool
		brief, why     bool
		minCov         float64
		useWeight      float64
		retireAfter    time.Duration
		form, typ      string
	)
	cmd := &cobra.Command{
		Use:   "search <term>...",
		Short: "Find relevant pages (run this BEFORE starting a task)",
		Long: `Deterministic ranked search over the page index: substring terms scored
against title/description/tags/body, filtered by activation scope
(repo/os), weighted by the validation ladder (validated > candidate >
stale; superseded excluded). Output is token-lean and capped small on
purpose — open a page with 'kb show <slug>' only when its description
matches the task.

--federate additionally reads the CURRENT repo's rings read-only: the
graph contribution log (.agents/bashy/graph/contrib.jsonl) and the weave
campaign memory (~/.bashy/weave/...). No terms lists everything (use
--tags to filter).`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if form != "" && !ValidForm(form) {
				return fmt.Errorf("kb: invalid form %q (note|page|relation|code)", form)
			}
			if typ != "" && !ValidType(typ) {
				return fmt.Errorf("kb: invalid type %q (lesson|gotcha|runbook|decision|fact)", typ)
			}
			store := openRing(*dir, *ring)
			pages, err := store.List()
			if err != nil {
				return err
			}
			pages = filterType(filterForm(pages, form), typ)
			if repo == "" {
				if cwd, err := os.Getwd(); err == nil {
					if root := repoRootOf(cwd); root != "" {
						repo = filepath.Base(root)
					}
				}
			}
			// Tokenize the raw CLI args through the shared Terms() tokenizer
			// (as transfer.go does) so quoted task-shaped queries — "how do I
			// gate a merge" — match per word instead of as one 5-word term.
			terms := Terms(strings.Join(args, " "))
			q := Query{Terms: terms, Repo: repo, OS: goos, Tags: tags, K: k, All: all, MinCoverage: minCov}
			if retireAfter > 0 {
				q.RetireUnopenedAfter = retireAfter
				q.UseObservedFrom = store.ObservedFrom()
				if q.Use == nil {
					q.Use = store.UseHistory()
				}
			}
			if useWeight != 0 {
				// Opt-in: rank partly by what READERS have opened before.
				q.Use, q.UseWeight = store.UseHistory(), useWeight
			}
			hits := Search(pages, q)
			var rel []Relation
			if form == FormRelation {
				live, err := (RelationRing{Dir: store.Dir()}).Live()
				if err != nil {
					return err
				}
				rel = SearchRelations(live, terms, k)
			}
			var fed []FedHit
			if federate {
				if cwd, err := os.Getwd(); err == nil {
					fed = FederatedSearch(cwd, terms, k)
				}
			}
			out := c.OutOrStdout()

			// An empty answer explains itself: which terms no page uses, and
			// what vocabulary this corpus does speak. The caller reformulates
			// from that instead of guessing what the silence meant.
			empty := len(hits) == 0 && len(rel) == 0 && len(fed) == 0
			var rep *Report
			if empty || why {
				r := Diagnose(pages, q)
				rep = &r
			}

			if jsonOut {
				return writeSearchJSON(out, hits, rel, fed, rep, jsonResolution(brief, full), terms)
			}
			if empty {
				fmt.Fprint(out, rep.Text())
				fmt.Fprintln(out, "no matching kb pages — if this task teaches something durable, contribute one: bashy kb add")
				return nil
			}
			if rep != nil {
				fmt.Fprint(out, rep.Text())
			}
			res := ResLine
			if brief {
				res = ResCue
			}
			rd := Renderer{Resolution: res, Sep: "  "}
			for _, h := range hits {
				fmt.Fprint(out, rd.Page(h.Page))
				if full {
					if body := strings.TrimSpace(h.Page.Body); body != "" {
						fmt.Fprintln(out, indent(body, "    "))
					}
				}
			}
			for _, f := range fed {
				fmt.Fprintf(out, "%s  %s\n", f.Origin, f.Text)
			}
			for _, r := range rel {
				fmt.Fprintf(out, "relation  %s  (%s)\n", RelationText(r), r.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "filter to pages applicable to this repo basename (default: current repo)")
	cmd.Flags().StringVar(&goos, "os", runtime.GOOS, "filter to pages applicable to this OS")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "require at least one of these tags")
	cmd.Flags().IntVar(&k, "k", DefaultK, "max results")
	cmd.Flags().BoolVar(&all, "all", false, "include superseded pages")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "JSON output")
	cmd.Flags().BoolVar(&full, "full", false, "print page bodies, not just index lines")
	cmd.Flags().BoolVar(&brief, "brief", false, "cue lines only (slug + title) — ~3.7x leaner when you already know what you are looking for")
	cmd.Flags().BoolVar(&federate, "federate", false, "also search the current repo's contribution log + weave memory")
	cmd.Flags().DurationVar(&retireAfter, "retire-unopened-after", 0, "hide pages older than this that have never been opened (0 = never retire); a retrieval filter, nothing is deleted")
	cmd.Flags().Float64Var(&useWeight, "use-weight", 0, "weight the ACT-R base-level term (recency x frequency of opens); 0 = rank purely on the query")
	cmd.Flags().Float64Var(&minCov, "min-coverage", 0, "return NOTHING unless a page matches at least this fraction of the query terms (0 = always answer)")
	cmd.Flags().BoolVar(&why, "why", false, "also explain the search: which query terms the corpus carries, and where (always shown when nothing matches)")
	cmd.Flags().StringVar(&form, "form", "", "filter to one record form: note|page|relation|code (legacy pages read as page)")
	cmd.Flags().StringVar(&typ, "type", "", "filter to one page type: lesson|gotcha|runbook|decision|fact")
	return cmd
}

// filterType keeps only pages of the given type (exact match on the required
// frontmatter field). An empty type keeps everything.
func filterType(pages []*Page, typ string) []*Page {
	if typ == "" {
		return pages
	}
	out := pages[:0:0]
	for _, p := range pages {
		if p.Type == typ {
			out = append(out, p)
		}
	}
	return out
}

// filterForm keeps only pages of the given form (by effective form, so legacy
// records count as pages). An empty form keeps everything.
func filterForm(pages []*Page, form string) []*Page {
	if form == "" {
		return pages
	}
	out := pages[:0:0]
	for _, p := range pages {
		if p.EffForm() == form {
			out = append(out, p)
		}
	}
	return out
}

// searchHitJSON is one hit on the MACHINE path. Which fields are populated is
// governed by the Resolution ladder, exactly as on the text path — see
// jsonResolution. Everything below Slug/Title/Score/Why is omitempty precisely
// so a leaner resolution is leaner on the wire and not just in intent.
type searchHitJSON struct {
	Slug        string   `json:"slug"`
	Ref         string   `json:"ref"`           // kb:<slug> — the address, copy as-is
	ID          string   `json:"id,omitempty"`  // uuid — the identity (empty on a page not yet rewritten)
	Seq         int      `json:"seq,omitempty"` // running number within this ring — input only
	Type        string   `json:"type,omitempty"`
	Status      string   `json:"status,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Evidence    string   `json:"evidence,omitempty"`
	Body        string   `json:"body,omitempty"`
	Score       float64  `json:"score"`
	// Why explains the match: the fields that carried a query term, strongest
	// first, then coverage. Always present — a ranking a third party cannot
	// interrogate is one it cannot debug, and it cannot read our source.
	Why []string `json:"why,omitempty"`
}

type relationHitJSON struct {
	ID       string `json:"id"`
	Op       string `json:"op"`
	Target   string `json:"target,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	Relation string `json:"relation,omitempty"`
	Dst      string `json:"dst,omitempty"`
	DstID    string `json:"dst_id,omitempty"`
	Text     string `json:"text"`
}

// jsonResolution maps the output flags onto the resolution ladder for the
// machine path.
//
// It exists because --json ignored the ladder entirely: writeSearchJSON emitted
// the whole body at every resolution, so one live-store query measured 5,316
// bytes under --brief, the default AND --full alike — byte-identical, the flags
// inert — against 653 for the text default and 304 for text --brief. The
// surface an agent parses was 8.1x the surface a human reads, on the
// search-before-every-task call this store's own help text prescribes.
//
// --brief wins over --full when both are given: asking for the leanest and the
// fullest at once is a contradiction, and the lean answer is the safe one to
// resolve it to.
func jsonResolution(brief, full bool) Resolution {
	switch {
	case brief:
		return ResCue
	case full:
		return ResFull
	default:
		return ResLine
	}
}

type termReportJSON struct {
	Term     string   `json:"term"`
	Pages    int      `json:"pages"`
	CuePages int      `json:"cue_pages"`
	Near     []string `json:"near,omitempty"`
}

// searchReportJSON is the machine-readable half of an empty answer. An agent
// consuming --json gets the same explanation a human gets, so it can reformulate
// without parsing prose.
type searchReportJSON struct {
	Total    int              `json:"total"`
	Eligible int              `json:"eligible"`
	Terms    []termReportJSON `json:"terms"`
	Vocab    []string         `json:"vocab,omitempty"`
}

func writeSearchJSON(w io.Writer, hits []Hit, rel []Relation, fed []FedHit, rep *Report, res Resolution, terms []string) error {
	payload := struct {
		Pages     []searchHitJSON   `json:"pages"`
		Relations []relationHitJSON `json:"relations,omitempty"`
		Federated []FedHit          `json:"federated,omitempty"`
		Report    *searchReportJSON `json:"report,omitempty"`
	}{Pages: []searchHitJSON{}, Federated: fed}
	if rep != nil {
		r := &searchReportJSON{
			Total: rep.Total, Eligible: rep.Eligible,
			Terms: []termReportJSON{}, Vocab: rep.Vocab,
		}
		for _, t := range rep.Terms {
			r.Terms = append(r.Terms, termReportJSON{
				Term: t.Term, Pages: t.Pages, CuePages: t.CuePages, Near: t.Near,
			})
		}
		payload.Report = r
	}
	for _, h := range hits {
		p := h.Page
		// ResCue is the ADDRESS: enough to decide whether to open a page, not
		// enough to decide whether it applies. Same contract as render.go.
		hit := searchHitJSON{
			Slug: p.Slug, Ref: "kb:" + p.Slug, ID: p.ID, Seq: p.Seq, Title: p.Title, Score: h.Score,
			Why: MatchedFields(p, terms),
		}
		if res >= ResLine {
			hit.Type, hit.Status = p.Type, p.Status
			hit.Description, hit.Tags, hit.Evidence = p.Description, p.Tags, p.Evidence
		}
		if res == ResFull {
			// Capped, with the same "…" marker the text path uses, so a reader
			// can tell a short body from a clipped one. A caller that wants the
			// whole page asks for the whole page: kb show <slug>.
			if body := strings.TrimSpace(p.Body); body != "" {
				hit.Body = truncateRunes(strings.ReplaceAll(body, "\n", " "), DefaultBodyCap)
			}
		}
		payload.Pages = append(payload.Pages, hit)
	}
	for _, r := range rel {
		payload.Relations = append(payload.Relations, relationHitJSON{
			ID: r.ID, Op: r.Op, Target: r.Target, TargetID: r.TargetID,
			Relation: r.Relation, Dst: r.Dst, DstID: r.DstID, Text: RelationText(r),
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// --- show ----------------------------------------------------------------

func newShowCmd(dir, ring *string) *cobra.Command {
	var form string
	cmd := &cobra.Command{
		Use:   "show <slug|#seq|uuid>",
		Short: "Print one page (frontmatter + body) — by slug, running number or uuid (prefix)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if form != "" && !ValidForm(form) {
				return fmt.Errorf("kb: invalid form %q (note|page|relation|code)", form)
			}
			store := openRing(*dir, *ring)
			// The three handles of one entity (ref.Shape) all open the same
			// page. LoadByHandle applies the agent ring's owner scope the way
			// Load does: another principal's page reports as absent.
			p, err := store.LoadByHandle(strings.TrimPrefix(args[0], "kb:"))
			if err != nil {
				return err
			}
			if form != "" && p.EffForm() != form {
				return fmt.Errorf("kb: %s is form %s, not %s", p.Slug, p.EffForm(), form)
			}
			b, err := os.ReadFile(store.PagePath(p.Slug))
			if err != nil {
				return err
			}
			// An OPEN is the use signal — the reader's judgement, not the
			// ranker's own output. See pkg/kb/activation.go.
			store.RecordOpen(p.Slug)
			_, err = c.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&form, "form", "", "require the page to be this form: note|page")
	return cmd
}

// --- add -----------------------------------------------------------------

// pageFlags are the authoring flags shared by add and supersede.
type pageFlags struct {
	typ, title, desc     string
	tags, repos          []string
	goos, evidence, slug string
	body, bodyFile       string
	form                 string
}

func (f *pageFlags) register(cmd *cobra.Command, requireTitle bool) {
	cmd.Flags().StringVar(&f.form, "form", FormPage, "record form: note|page (note = description optional, body free)")
	cmd.Flags().StringVar(&f.typ, "type", TypeLesson, "page type: lesson|gotcha|runbook|decision|fact")
	cmd.Flags().StringVar(&f.title, "title", "", "page title")
	cmd.Flags().StringVar(&f.desc, "description", "", "what + WHEN this applies (the routing surface)")
	cmd.Flags().StringSliceVar(&f.tags, "tags", nil, "tags")
	cmd.Flags().StringSliceVar(&f.repos, "repos", nil, "scope: applies only to these repo basenames")
	cmd.Flags().StringVar(&f.goos, "os", "", "scope: applies only to this OS (GOOS value)")
	cmd.Flags().StringVar(&f.evidence, "evidence", "", "how this was verified (command, commit, issue)")
	cmd.Flags().StringVar(&f.slug, "slug", "", "override the auto slug")
	cmd.Flags().StringVar(&f.body, "body", "", "page body text")
	cmd.Flags().StringVarP(&f.bodyFile, "file", "f", "", "read the body from FILE ('-' = stdin)")
	if requireTitle {
		_ = cmd.MarkFlagRequired("title")
		// --description is required for a PAGE and optional for a NOTE (the
		// memo shape, page.go FormNote) — cobra cannot express "required
		// unless --form note", so buildPage enforces it per form.
	}
}

func (f *pageFlags) buildPage(c *cobra.Command) (*Page, error) {
	if !ValidType(f.typ) {
		return nil, fmt.Errorf("kb: invalid type %q (lesson|gotcha|runbook|decision|fact)", f.typ)
	}
	form := f.form
	if form == "" {
		form = FormPage
	}
	if !StorableForm(form) {
		// relation lives in the graph, code is a view — neither is a page.
		return nil, fmt.Errorf("kb: form %q cannot be written as a page (note|page); relation lives in the graph, code is a view", form)
	}
	if form == FormPage && strings.TrimSpace(f.desc) == "" {
		return nil, fmt.Errorf("kb: a page needs --description (what + WHEN it applies); a memo without one is --form note")
	}
	body := f.body
	if f.bodyFile != "" {
		var b []byte
		var err error
		if f.bodyFile == "-" {
			b, err = io.ReadAll(c.InOrStdin())
		} else {
			b, err = os.ReadFile(f.bodyFile)
		}
		if err != nil {
			return nil, err
		}
		body = string(b)
	}
	slug := f.slug
	if slug == "" {
		slug = Slugify(f.title)
	}
	// A slug is the page's readable handle; the other two shapes are taken.
	// A number would collide with the running number (`kb:22`), a hex run
	// with a uuid prefix — and a resolver that guessed would open the wrong
	// page silently. Refuse at creation; the slug is immutable after.
	if shape := ref.ShapeOf(slug); shape != ref.ShapeSlug {
		return nil, fmt.Errorf("kb: slug %q reads as a %s, not a slug — a running number and a hex uuid prefix are the page's OTHER two handles; pick a name with a letter in it (--slug)", slug, shape)
	}
	p := &Page{
		Slug:        slug,
		Form:        form,
		Type:        f.typ,
		Title:       f.title,
		Description: strings.TrimSpace(f.desc),
		Tags:        f.tags,
		Evidence:    f.evidence,
		Status:      StatusCandidate,
		Source:      &Source{Tool: ToolID(), Host: HostID(), Episode: EpisodeID()},
		Body:        strings.TrimSpace(body),
	}
	if len(f.repos) > 0 || f.goos != "" {
		p.Scope = &Scope{Repos: f.repos, OS: f.goos}
	}
	return p, nil
}

func newAddCmd(dir, ring *string) *cobra.Command {
	var flags pageFlags
	var force bool
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Contribute a new page (reconciles against existing pages first)",
		Long: `Add a candidate page. The add RECONCILES first: if an existing live page
looks like the same knowledge, add refuses and points at it — update or
supersede that page instead (--force overrides). Distill strategy, not
transcript; failures are as valuable as successes.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			store := openRing(*dir, *ring)
			p, err := flags.buildPage(c)
			if err != nil {
				return err
			}
			pages, err := store.List()
			if err != nil {
				return err
			}
			if !force {
				if dup := NearDuplicate(pages, p.Title, p.Description); dup != nil {
					return fmt.Errorf("kb: looks like a duplicate of %q (%s) — use 'kb update %s' or 'kb supersede %s' (or --force)",
						dup.Title, dup.Slug, dup.Slug, dup.Slug)
				}
			}
			if err := store.Write(p, "add"); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added %s (candidate) — validate after it proves out: bashy kb validate %s --evidence \"...\"\n", p.Slug, p.Slug)
			return nil
		},
	}
	flags.register(cmd, true)
	cmd.Flags().BoolVar(&force, "force", false, "skip the duplicate check")
	return cmd
}

// --- update --------------------------------------------------------------

func newUpdateCmd(dir, ring *string) *cobra.Command {
	var (
		title, desc, status, evidence string
		tags                          []string
		body, bodyFile                string
	)
	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Refresh an existing page (the entry was right — extend or correct in place)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			store := openRing(*dir, *ring)
			p, err := store.Load(args[0])
			if err != nil {
				return err
			}
			changed := false
			set := func(dst *string, v string) {
				if v != "" {
					*dst = v
					changed = true
				}
			}
			set(&p.Title, title)
			set(&p.Description, strings.TrimSpace(desc))
			set(&p.Evidence, evidence)
			// supersededHere records a status flip INTO superseded, so the
			// announcement fires from this route too — `update --status
			// superseded` invalidates a fact exactly as `supersede` does, just
			// without a replacement to point at.
			supersededHere := false
			if status != "" {
				switch status {
				case StatusCandidate, StatusValidated, StatusStale, StatusSuperseded:
					supersededHere = status == StatusSuperseded && p.Status != StatusSuperseded
					p.Status = status
					changed = true
				default:
					return fmt.Errorf("kb: invalid status %q (candidate|validated|stale|superseded)", status)
				}
			}
			if len(tags) > 0 {
				p.Tags = tags
				changed = true
			}
			if bodyFile != "" {
				var b []byte
				var rerr error
				if bodyFile == "-" {
					b, rerr = io.ReadAll(c.InOrStdin())
				} else {
					b, rerr = os.ReadFile(bodyFile)
				}
				if rerr != nil {
					return rerr
				}
				body = string(b)
			}
			if body != "" {
				p.Body = strings.TrimSpace(body)
				changed = true
			}
			if !changed {
				return fmt.Errorf("kb: nothing to update — pass at least one of --title/--description/--status/--evidence/--tags/--body/-f")
			}
			if err := store.Write(p, "update"); err != nil {
				return err
			}
			if supersededHere {
				// Same invalidation, different route — no replacement to name.
				publishSuperseded(p, nil)
			}
			fmt.Fprintf(c.OutOrStdout(), "updated %s\n", p.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title (slug is kept)")
	cmd.Flags().StringVar(&desc, "description", "", "new description")
	cmd.Flags().StringVar(&status, "status", "", "new status: candidate|validated|stale|superseded")
	cmd.Flags().StringVar(&evidence, "evidence", "", "new evidence")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "replace tags")
	cmd.Flags().StringVar(&body, "body", "", "replace the body text")
	cmd.Flags().StringVarP(&bodyFile, "file", "f", "", "replace the body from FILE ('-' = stdin)")
	return cmd
}

// --- supersede -----------------------------------------------------------

func newSupersedeCmd(dir, ring *string) *cobra.Command {
	var flags pageFlags
	cmd := &cobra.Command{
		Use:   "supersede <slug>",
		Short: "Replace a wrong page with a corrected one (the correction stays linked, nothing is deleted)",
		Long: `Write a new page and flip the old one to status: superseded, with
supersedes/superseded_by links both ways. Never delete knowledge — an
invalidated lesson plus its correction is itself knowledge.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			store := openRing(*dir, *ring)
			old, err := store.Load(args[0])
			if err != nil {
				return err
			}
			p, err := flags.buildPage(c)
			if err != nil {
				return err
			}
			if p.Slug == old.Slug {
				return fmt.Errorf("kb: new page needs its own identity — pass a different --title or --slug")
			}
			p.Supersedes = old.Slug
			if err := store.Write(p, "supersede"); err != nil {
				return err
			}
			old.Status = StatusSuperseded
			old.SupersededBy = p.Slug
			if err := store.Write(old, "supersede"); err != nil {
				return err
			}
			// Announce it: a superseded page is the one kb operation an agent
			// cannot afford to discover on its next pull, because it may already
			// have read and believed the page being invalidated. Published after
			// both writes land, so the notification never points at a correction
			// that is not on disk yet.
			publishSuperseded(old, p)
			fmt.Fprintf(c.OutOrStdout(), "superseded %s -> %s\n", old.Slug, p.Slug)
			return nil
		},
	}
	flags.register(cmd, true)
	return cmd
}

// --- retro ---------------------------------------------------------------

func newRetroCmd(dir, ring *string) *cobra.Command {
	var k int
	cmd := &cobra.Command{
		Use:   "retro [<term>...]",
		Short: "Post-task write-back: review related pages, then ADD / UPDATE / SUPERSEDE / VALIDATE / NOOP",
		Long: `Run AFTER completing a task. Shows the pages related to what you just did
and the one decision to make about the knowledge — never blind-append.
Deterministic (no LLM): the judgment is yours, retro structures it.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			store := openRing(*dir, *ring)
			pages, err := store.List()
			if err != nil {
				return err
			}
			repo := ""
			if cwd, err := os.Getwd(); err == nil {
				if root := repoRootOf(cwd); root != "" {
					repo = filepath.Base(root)
				}
			}
			hits := Search(pages, Query{Terms: Terms(strings.Join(args, " ")), Repo: repo, OS: runtime.GOOS, K: k})
			out := c.OutOrStdout()
			fmt.Fprintln(out, "kb retro — post-task knowledge write-back")
			if len(args) > 0 {
				fmt.Fprintf(out, "related pages (query: %s):\n", strings.Join(args, " "))
			} else {
				fmt.Fprintln(out, "related pages:")
			}
			if len(hits) == 0 {
				fmt.Fprintln(out, "  (none)")
			}
			for _, h := range hits {
				p := h.Page
				fmt.Fprint(out, Renderer{Resolution: ResLine, Bullet: "  ", Sep: "  "}.Page(p))
			}
			fmt.Fprint(out, `decide ONE:
  ADD        bashy kb add --type lesson --title "<distilled insight>" --description "<what + WHEN it applies>"   # nothing relevant existed and the task taught something durable
  UPDATE     bashy kb update <slug> --evidence "..."       # a page was right — extend/refresh it
  SUPERSEDE  bashy kb supersede <slug> --title "..." ...   # a page was wrong — replace it, correction stays linked
  VALIDATE   bashy kb validate <slug> --evidence "..."     # a candidate page was consulted and proved correct
  NOOP       nothing durable learned — do nothing
write distilled strategy, not transcript; capture failures as guardrails.
`)
			return nil
		},
	}
	cmd.Flags().IntVar(&k, "k", DefaultK, "max related pages shown")
	return cmd
}

// --- list / index / log --------------------------------------------------

func newListCmd(dir, ring *string) *cobra.Command {
	var jsonOut bool
	var form, typ string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every page, one line each",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if form != "" && !ValidForm(form) {
				return fmt.Errorf("kb: invalid form %q (note|page|relation|code)", form)
			}
			if typ != "" && !ValidType(typ) {
				return fmt.Errorf("kb: invalid type %q (lesson|gotcha|runbook|decision|fact)", typ)
			}
			store := openRing(*dir, *ring)
			pages, err := store.List()
			if err != nil {
				return err
			}
			pages = filterType(filterForm(pages, form), typ)
			if jsonOut {
				// list has no query, so there is nothing to explain (no why).
				// ResLine matches this verb's own contract — "one line each",
				// which is what the text path below renders. It previously
				// emitted every page's full body, so `list --json` on a real
				// store was the single largest payload kb could produce.
				return writeSearchJSON(c.OutOrStdout(), toHits(pages), nil, nil, nil, ResLine, nil)
			}
			rd := LineRenderer()
			rd.Ref = true // kb:<slug> — copy a row straight into prose
			for _, p := range pages {
				fmt.Fprint(c.OutOrStdout(), rd.Page(p))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "JSON output")
	cmd.Flags().StringVar(&form, "form", "", "filter to one record form: note|page (legacy pages read as page)")
	cmd.Flags().StringVar(&typ, "type", "", "filter to one page type: lesson|gotcha|runbook|decision|fact")
	return cmd
}

func toHits(pages []*Page) []Hit {
	out := make([]Hit, 0, len(pages))
	for _, p := range pages {
		out = append(out, Hit{Page: p})
	}
	return out
}

func newIndexCmd(dir, ring *string) *cobra.Command {
	return &cobra.Command{
		Use:   "index",
		Short: "Regenerate index.md (the always-load entry point)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			store := openRing(*dir, *ring)
			if err := store.RebuildIndex(); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "wrote %s\n", filepath.Join(store.Dir(), "index.md"))
			return nil
		},
	}
}

func newLogCmd(dir, ring *string) *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show recent journal entries (who wrote what, when)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			store := openRing(*dir, *ring)
			lines, err := store.JournalTail(n)
			if err != nil {
				return err
			}
			for _, l := range lines {
				fmt.Fprintln(c.OutOrStdout(), l)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "n", "n", 20, "number of entries")
	return cmd
}

// --- backlinks -----------------------------------------------------------

// graphNodes collects the link-graph nodes visible from the resolved store:
// every kb page, plus the repo's todo/issue records when the store is a repo
// ring (so todo→kb citations resolve). todoKnown reports whether the todo
// namespace was enumerable — the caller uses it so an un-enumerable todo link
// is treated as external, not dangling.
func graphNodes(store *Store) (nodes []LinkNode, todoKnown bool, err error) {
	pages, err := store.List()
	if err != nil {
		return nil, false, err
	}
	nodes = KBNodes(pages)
	if td := siblingTodoDir(store.Dir()); td != "" {
		todoKnown = true
		tn, err := TodoNodesFromDir(td)
		if err != nil {
			return nil, false, err
		}
		nodes = append(nodes, tn...)
	}
	return nodes, todoKnown, nil
}

type backlinkJSON struct {
	Ref   string `json:"ref"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

func newBacklinksCmd(dir, ring *string) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "backlinks <slug>",
		Short: "Show every record (kb page or todo) whose body links to this page",
		Long: `Resolve the link graph at READ TIME: list every kb page and todo/issue whose
body cites <slug> (via [[slug]], [[kb:slug]], or a pages/<slug>.md link). Reads
only — nothing is recorded, no body is rewritten.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			store := openRing(*dir, *ring)
			nodes, _, err := graphNodes(store)
			if err != nil {
				return err
			}
			target := LinkNode{Kind: LinkKB, ID: args[0]}
			back := Backlinks(target, nodes)
			out := c.OutOrStdout()
			if jsonOut {
				rows := make([]backlinkJSON, 0, len(back))
				for _, n := range back {
					rows = append(rows, backlinkJSON{Ref: n.Ref(), Kind: string(n.Kind), Title: n.Title})
				}
				payload := struct {
					Slug      string         `json:"slug"`
					Backlinks []backlinkJSON `json:"backlinks"`
				}{Slug: args[0], Backlinks: rows}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}
			if len(back) == 0 {
				fmt.Fprintf(out, "no records link to kb:%s\n", args[0])
				return nil
			}
			fmt.Fprintf(out, "%d record(s) link to kb:%s\n", len(back), args[0])
			for _, n := range back {
				fmt.Fprintf(out, "  %s\t%s\n", n.Ref(), n.Title)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "JSON output")
	return cmd
}

// --- doctor ----------------------------------------------------------------

func newDoctorCmd(dir, ring *string) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Flag link-graph and hygiene problems (never fixes — repair with update/supersede)",
		Long: `Report, and only report: dangling links, links whose <kind>: is not in
the ref vocabulary, orphan pages (no inbound, no outbound, not validated),
near-duplicate pairs, and records missing a form or description. doctor FLAGS, it is never 'kb fix' — nothing here rewrites a body,
so a reported page is left byte-identical on disk. Scope it with --ring.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			store := openRing(*dir, *ring)
			pages, err := store.List()
			if err != nil {
				return err
			}
			var todoNodes []LinkNode
			todoKnown := false
			if td := siblingTodoDir(store.Dir()); td != "" {
				todoKnown = true
				if todoNodes, err = TodoNodesFromDir(td); err != nil {
					return err
				}
			}
			rep := Doctor(pages, store, todoNodes, todoKnown)
			relations, err := (RelationRing{Dir: store.Dir()}).Live()
			if err != nil {
				return err
			}
			rep.OpenRelations = DoctorRelations(relations)
			rep.Ring = *ring
			out := c.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			if rep.Clean() {
				fmt.Fprintln(out, "kb doctor: no issues found")
				return nil
			}
			if len(rep.Dangling) > 0 {
				fmt.Fprintf(out, "dangling links (%d):\n", len(rep.Dangling))
				for _, d := range rep.Dangling {
					fmt.Fprintf(out, "  %s -> %s  %s\n", d.From, d.Target, d.Raw)
				}
			}
			if len(rep.UnknownKind) > 0 {
				fmt.Fprintf(out, "unknown-kind links (%d; kinds: %s):\n", len(rep.UnknownKind), strings.Join(ref.KindNames(), " "))
				for _, d := range rep.UnknownKind {
					fmt.Fprintf(out, "  %s -> [[%s]]  %s\n", d.From, d.Target, d.Raw)
				}
			}
			if len(rep.Orphans) > 0 {
				fmt.Fprintf(out, "orphan pages (%d):\n", len(rep.Orphans))
				for _, s := range rep.Orphans {
					fmt.Fprintf(out, "  %s\n", s)
				}
			}
			if len(rep.NearDuplicates) > 0 {
				fmt.Fprintf(out, "near-duplicate pairs (%d):\n", len(rep.NearDuplicates))
				for _, p := range rep.NearDuplicates {
					fmt.Fprintf(out, "  %s <-> %s\n", p.A, p.B)
				}
			}
			if len(rep.MissingForm) > 0 {
				fmt.Fprintf(out, "missing form (%d):\n", len(rep.MissingForm))
				for _, s := range rep.MissingForm {
					fmt.Fprintf(out, "  %s\n", s)
				}
			}
			if len(rep.MissingDescription) > 0 {
				fmt.Fprintf(out, "missing description (%d):\n", len(rep.MissingDescription))
				for _, s := range rep.MissingDescription {
					fmt.Fprintf(out, "  %s\n", s)
				}
			}
			if len(rep.MissingID) > 0 {
				fmt.Fprintf(out, "missing id/seq (%d) — resolve by slug only until rewritten (`kb update <slug>`):\n", len(rep.MissingID))
				for _, s := range rep.MissingID {
					fmt.Fprintf(out, "  %s\n", s)
				}
			}
			if len(rep.DuplicateSeq) > 0 {
				fmt.Fprintf(out, "duplicate seq (%d) — kb:<seq> is ambiguous for these; slug and uuid still resolve:\n", len(rep.DuplicateSeq))
				for _, d := range rep.DuplicateSeq {
					fmt.Fprintf(out, "  #%d  %s\n", d.Seq, strings.Join(d.Slugs, "  "))
				}
			}
			if len(rep.OpenRelations) > 0 {
				fmt.Fprintf(out, "open-vocabulary relations (%d):\n", len(rep.OpenRelations))
				for _, r := range rep.OpenRelations {
					fmt.Fprintf(out, "  %s -%s-> %s  [%s]\n", r.Target, r.Relation, r.Dst, r.ID)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "JSON output")
	return cmd
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
