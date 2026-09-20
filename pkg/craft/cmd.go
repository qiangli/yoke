package craft

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/skills"
)

// config is assembled from Options by NewCraftCmd.
type config struct {
	// skillOpts are the SAME options the skills CLI is built with, so both see
	// one catalog rather than two views that can disagree about what exists.
	skillOpts []skills.Option

	// storeDir is the ring-1 skills store craft reads evidence from. It is
	// deliberately the SAME directory pkg/skills writes to: craft is a layer
	// over the catalog, not a parallel store. A second store would be an
	// eighth disjoint outcome log in a tree that already has seven.
	storeDir string
}

// Option configures the craft command tree.
type Option func(*config)

// WithStoreDir overrides the skills store craft reads
// (default ~/.config/bashy/skills).
func WithStoreDir(dir string) Option { return func(c *config) { c.storeDir = dir } }

// WithSkillOptions passes the host's skill-catalog options through, so craft
// indexes exactly the skills `bashy skill list` shows.
func WithSkillOptions(opts ...skills.Option) Option {
	return func(c *config) { c.skillOpts = append(c.skillOpts, opts...) }
}

// index builds the queryable view over the applicable catalog.
func (c *config) index() *Index {
	cat, ps := skills.NewCatalog(c.skillOpts...)
	return NewIndex(LoadImplementations(cat, ps))
}

// coordinate is THIS host's space-time coordinate, used wherever a caller does
// not name one.
//
// It reads the same helper `skills probe` prints, rather than computing a
// second one: a fold written at a coordinate the reader never computes is
// invisible, and nothing about it looks broken — the write succeeds, the store
// grows, and every read returns nothing.
//
// Defaulting at all is what makes the fold half of this usable day to day.
// Requiring a 65-character hash on the command line for "what I just learned
// about this machine" is a cost paid at exactly the moment somebody has one
// thing to write down and no patience for a second command.
func (c *config) coordinate() string { return skills.HostCoordinate(c.skillOpts...) }

// defaultStoreDir is the catalog's own store — craft accumulates INTO the
// skills store, so there is exactly one ladder, and it is the catalog's.
func defaultStoreDir() string { return skills.DefaultStoreDir() }

// NewCraftCmd builds the `craft` command tree — the living skill graph over
// the catalog pkg/skills manages.
//
// The split is deliberate. `bashy skill` is the CATALOG: an Agent
// Skills-compatible store you list, show, add, verify, run, and export.
// `bashy craft` is what the catalog ACCUMULATES INTO: evidence gathered across
// runs, coordinates, and implementations. One is the shelf; the other is what
// the practitioner has learned from working it.
func NewCraftCmd(opts ...Option) *cobra.Command {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.storeDir == "" {
		cfg.storeDir = defaultStoreDir()
	}

	root := &cobra.Command{
		Use:   "craft",
		Short: "the living skill graph: what this host has learned from running skills",
		Long: "craft is the accumulated body of practical skill on this host.\n\n" +
			"Where `bashy skill` manages the catalog — the skills you have — craft is\n" +
			"what running them has taught: which contract held, at which space-time\n" +
			"coordinate, under which executor, and how often.\n\n" +
			"Evidence is keyed two ways. By NAME, which identifies one skill. And by\n" +
			"CAPABILITY — a content address over a skill's contract and effect cap — so\n" +
			"two differently-named skills that make the same guarantee pool their\n" +
			"evidence instead of competing as strangers.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(cmd *cobra.Command, args []string) error { return runHistory(cmd, cfg, "", "", false, false) },
	}

	var histJSON, histAll bool
	var histCapability string
	history := &cobra.Command{
		Use:   "history [name]",
		Short: "stored run receipts, per skill or pooled per capability",
		Long: "history reads back the attestation ledger every `skills run` writes.\n" +
			"Each row is one recorded run at one space-time coordinate: whether the\n" +
			"contract held, which executor tier ran it, and when.\n\n" +
			"With no argument it summarises every skill with evidence. With a name it\n" +
			"summarises that skill. With --capability it pools evidence across every\n" +
			"implementation that makes the same guarantee, which is the question worth\n" +
			"asking when two skills are interchangeable.\n\n" +
			"This reports; it decides nothing. Retirement and election need an evidence\n" +
			"floor a single host does not reach alone, and acting on a handful of runs\n" +
			"would discard a good skill on noise.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			return runHistory(cmd, cfg, name, histCapability, histAll, histJSON)
		},
	}
	history.Flags().BoolVar(&histJSON, "json", false, "machine-readable summary")
	history.Flags().BoolVar(&histAll, "all", false, "list individual runs, not just the summary")
	history.Flags().StringVar(&histCapability, "capability", "", "pool evidence across implementations of this capability key")

	var studyLicense, studyOrigin, studyRef string
	var studyAgent, studyLang string
	var studyBand int
	var studyJSON bool
	study := &cobra.Command{
		Use:   "study <dir>",
		Short: "absorb a directory of external skills — digest, do not memorize",
		Long: "study reads a directory of skill folders and resolves each against what is\n" +
			"already known, BY CAPABILITY rather than by name:\n\n" +
			"  novel        a guarantee nothing here makes yet   -> the catalog grows\n" +
			"  alternative  same guarantee, another implementation\n" +
			"  duplicate    same guarantee, same implementation\n" +
			"  quarantined  no contract, so no capability is knowable — held, not merged\n" +
			"  refused      license, or a face that will not parse — nothing is stored\n\n" +
			"Only `novel` grows the catalog, and the ratio of growth to input is the\n" +
			"DIGESTION RATIO. At 1:1 nothing was digested: each external skill became\n" +
			"its own entry, which is how a catalog turns into a pile that makes an\n" +
			"agent measurably worse rather than better.\n\n" +
			"The license gate is fail-closed. Absence of a license is not permission —\n" +
			"it is all-rights-reserved by default — and licence is read PER SKILL, never\n" +
			"inherited from a repository.\n\n" +
			"Prose with no contract is QUARANTINED, not guessed at: inventing a contract\n" +
			"would file a skill under a promise nobody verified.\n\n" +
			"To decompose prose into a typed contract, pass --normalise-agent with the\n" +
			"model's --band. This is the one step that genuinely needs a model, and it is\n" +
			"the work worth paying for once: the contract it produces is checked for free\n" +
			"by every later run. It requires band >= 3 and REFUSES below that — an\n" +
			"under-banded decomposition is worse than none, because a plausible-but-wrong\n" +
			"contract is a promise nobody verified that every later reader inherits.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStudy(cmd, cfg, args[0], Source{
				Origin:  studyOrigin,
				Ref:     studyRef,
				License: studyLicense,
			}, studyJSON, studyAgent, studyBand, studyLang)
		},
	}
	study.Flags().StringVar(&studyAgent, "normalise-agent", "", "headless agent CLI that decomposes prose into a typed contract; the prompt is appended as the last argument (e.g. \"claude -p\")")
	study.Flags().IntVar(&studyBand, "band", 0, "capability band of the normalise-agent's model (must be >= 3; an unverified band is not a passing band)")
	study.Flags().StringVar(&studyLang, "lang", "en", "source natural language of the prose (the canonical form is language-neutral)")
	study.Flags().StringVar(&studyLicense, "license", "", "SPDX id these skills are under (required; absence is not permission)")
	study.Flags().StringVar(&studyOrigin, "origin", "", "upstream repo or URL, recorded as provenance")
	study.Flags().StringVar(&studyRef, "ref", "", "upstream commit sha, recorded as provenance")
	study.Flags().BoolVar(&studyJSON, "json", false, "machine-readable report")

	var findJSON bool
	var findLimit int
	find := &cobra.Command{
		Use:   "find <query>",
		Short: "ask for a capability in your own words, not by skill name",
		Long: "find ranks CAPABILITIES against a query — what a skill guarantees, not what\n" +
			"it is called.\n\n" +
			"Two implementations of one guarantee return as ONE result with an\n" +
			"alternative, never as two rows competing for selection. That competition is\n" +
			"the failure mode this exists to remove: semantically-overlapping entries\n" +
			"displacing each other is what makes a large catalog worse than a small one.\n\n" +
			"Matching runs on a stdlib floor with no model and no network: field-weighted\n" +
			"scoring plus graph expansion over the typed neighbourhood. Because a\n" +
			"capability is described by what it GUARANTEES, contract predicates are\n" +
			"searchable text — which is how a query finds a skill whose prose never uses\n" +
			"the query's words.\n\n" +
			"Every match reports WHY it scored. A ranking nobody can interrogate is one\n" +
			"nobody can debug when it starts returning the wrong thing. A query that\n" +
			"matches nothing returns nothing, rather than the least-bad row.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFind(cmd, cfg, strings.Join(args, " "), findLimit, findJSON)
		},
	}
	find.Flags().BoolVar(&findJSON, "json", false, "machine-readable matches")
	find.Flags().IntVar(&findLimit, "limit", 5, "maximum matches")

	var composeBand int
	var composeJSON bool
	var composeFor, composeCoord string
	compose := &cobra.Command{
		Use:   "compose <query>",
		Short: "render the best-matching skill on demand, cut at a band",
		Long: "compose resolves a query and renders the elected implementation.\n\n" +
			"There is no stored file: the artifact is assembled now, and anything on disk\n" +
			"is a cache.\n\n" +
			"BAND is a cut point, not a variant — one artifact, one identity, rendered at\n" +
			"a different depth:\n\n" +
			"  0  pure script, runnable with NO model\n" +
			"  1  script plus preconditions and known failures\n" +
			"  2  imperative steps with the bound commands inline\n" +
			"  3  contract and effect cap — the WHAT, not the HOW\n" +
			"  4  intent and contract — maximum latitude\n\n" +
			"The default is the artifact's FLOOR, not the model's ceiling. A premium model\n" +
			"handed a deterministic script is cheaper, faster, and reproducible; high band\n" +
			"is the fallback for what genuinely cannot be pinned down.\n\n" +
			"A band below the floor is REFUSED, never synthesized. A model writing a\n" +
			"script at render time would put a model back on the read path, and the result\n" +
			"would stop being reproducible.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompose(cmd, cfg, strings.Join(args, " "), composeBand, composeJSON, composeFor, composeCoord)
		},
	}
	compose.Flags().IntVar(&composeBand, "band", -1, "cut point 0-4 (default: the artifact's floor)")
	compose.Flags().BoolVar(&composeJSON, "json", false, "machine-readable composition")
	compose.Flags().StringVar(&composeFor, "for", "", "scope to what this host has learned about an entity (host:name, service:name)")
	compose.Flags().StringVar(&composeCoord, "coordinate", "", "apply the folds that hold at this space-time coordinate")

	var learnSource string
	var forget bool
	learn := &cobra.Command{
		Use:   "learn <entity> <key> [value]",
		Short: "record what this host learned about one thing (host-local, never shared)",
		Long: "learn records a PARTICULAR fact — the login on that box, the port that\n" +
			"service answers on. Facts bind to an ENTITY and are true nowhere else, which\n" +
			"is what separates them from a fold (an OS-specific workaround, keyed on a\n" +
			"coordinate, and freely shareable).\n\n" +
			"Facts NEVER leave this host. Not scrubbed-then-shared — not shared. A fact is\n" +
			"by definition a statement about someone's machine, and values are stored raw\n" +
			"because a redacted login is useless. The boundary therefore has to hold at\n" +
			"the store, not at the reader.\n\n" +
			"Recording the same key again SUPERSEDES: the old value is closed off, never\n" +
			"rewritten, so you can still ask what this host believed last Tuesday.\n\n" +
			"With --forget the fact is invalidated WITHOUT asserting a replacement.\n" +
			"\"This is now wrong\" and \"this is now X\" are different claims, and a run that\n" +
			"failed has learned only the first — being made to invent a replacement is how\n" +
			"a guess gets into the store.\n\n" +
			"KEYS SPELLED AS ROLES TRAVEL; other keys are remembered but never offered.\n" +
			"`user` is the login a command will be handed; `remote_user` is a note to\n" +
			"yourself. Both are stored, only the first can be suggested — so the command\n" +
			"says which one you wrote.",
		Example: "  bashy craft learn host:workshop user svc-build\n" +
			"  bashy craft learn host:workshop port 2222 --source remote-shell\n" +
			"  bashy craft learn host:workshop user --forget",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			var value string
			if len(args) == 3 {
				value = args[2]
			}
			return runLearn(cmd, cfg, args[0], args[1], value, learnSource, forget)
		},
	}
	learn.Flags().StringVar(&learnSource, "source", "", "what learned it (a skill, a run) — provenance for a fact that turns out wrong")
	learn.Flags().BoolVar(&forget, "forget", false, "invalidate without asserting a replacement")

	var importSSHConfig string
	var importDryRun bool
	importCmd := &cobra.Command{
		Use:   "import",
		Short: "read facts from config this host already DECLARES (ssh config)",
		Long: "Every other path into the store learns by watching: a command runs, it\n" +
			"succeeds, and what it used is recorded. That is patient and it works, but it\n" +
			"only ever knows about hosts somebody has already reached from here.\n\n" +
			"An ssh config is the opposite — a curated map of hosts to the port, login and\n" +
			"key each one needs, written deliberately. So it fills the store at once, and\n" +
			"it covers machines this host has never contacted.\n\n" +
			"Host ALIASES are the interesting part. `Host dev-box` with `HostName 10.0.0.41`\n" +
			"means \"dev-box\" is a name that exists here and nowhere else — exactly the local\n" +
			"jargon worth being able to answer for, arriving with its meaning attached.\n\n" +
			"Wildcard blocks are skipped (they declare defaults for everything rather than\n" +
			"facts about anything) and Include is not followed. What lands are facts, so it\n" +
			"stays host-local and is never shared.",
		Example: "  bashy craft import --dry-run\n" +
			"  bashy craft import\n" +
			"  bashy craft import --ssh-config /path/to/config",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImport(cmd, cfg, importSSHConfig, importDryRun)
		},
	}
	importCmd.Flags().StringVar(&importSSHConfig, "ssh-config", "", "ssh config to read (default: the caller's own)")
	importCmd.Flags().BoolVar(&importDryRun, "dry-run", false, "show what would be recorded without writing")

	factsCmd := &cobra.Command{
		Use:   "facts [entity]",
		Short: "what this host has learned about things (host-local)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ent string
			if len(args) == 1 {
				ent = args[0]
			}
			return runFacts(cmd, cfg, ent)
		},
	}

	var foldCoord, foldCapability, foldEvidence, foldSource string
	var foldRetire bool
	fold := &cobra.Command{
		Use:   "fold <note>",
		Short: "record what generally holds at this coordinate (shareable, identity-free)",
		Long: "fold records a GENERALISABLE thing — \"mDNS is unreliable here, resolve by IP\"\n" +
			"— keyed on a space-time COORDINATE rather than on a machine. That is what\n" +
			"makes it worth sharing: two hosts with the same OS and toolchain share a\n" +
			"coordinate, so one machine's discovery is useful on another.\n\n" +
			"The classification is CHECKED, not trusted. A note naming a hostname, a user,\n" +
			"an address or a home path is a FACT wearing a fold's clothes — it is true on\n" +
			"one machine, not at a coordinate — and it is REFUSED with a pointer to\n" +
			"`craft learn`. That is what keeps the shareable store free of identity\n" +
			"without anyone having to remember which half they are writing.\n\n" +
			"Record the EVIDENCE too. A fold asserted without it is an opinion, and\n" +
			"opinions should not outlive the session that formed them.",
		Example: "  bashy craft fold \"mDNS is unreliable here; resolve the address first\" \\\n" +
			"      --evidence \"three consecutive lookup timeouts\"\n" +
			"  bashy craft folds          # what is folded here, and where else\n" +
			"  bashy craft fold \"…\" --coordinate <key>   # file it at another coordinate",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFold(cmd, cfg, args[0], foldCoord, foldCapability, foldEvidence, foldSource, foldRetire)
		},
	}
	fold.Flags().StringVar(&foldCoord, "coordinate", "", "the space-time context key this holds at (default: this host's)")
	fold.Flags().StringVar(&foldCapability, "capability", "", "scope to one capability; omit for an environment truth")
	fold.Flags().StringVar(&foldEvidence, "evidence", "", "what happened that taught this")
	fold.Flags().StringVar(&foldSource, "source", "", "what learned it")
	fold.Flags().BoolVar(&foldRetire, "retire", false, "mark this fold as no longer holding")

	foldsCmd := &cobra.Command{
		Use:   "folds [coordinate]",
		Short: "what generally holds, per coordinate",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var coord string
			if len(args) == 1 {
				coord = args[0]
			}
			return runFolds(cmd, cfg, coord)
		},
	}

	var promoteMin int
	var promoteCoord, promoteAccept string
	promoteCmd := &cobra.Command{
		Use:   "promote",
		Short: "facts that have repeated often enough to look general",
		Long: "promote finds facts that hold identically across several entities. When the\n" +
			"same thing is true of the third host in a row, it has probably stopped being\n" +
			"particular: said of one service it is a fact; said of every service here, it\n" +
			"is how this place works.\n\n" +
			"It PROPOSES and never decides. Nothing is recorded without --accept.\n\n" +
			"A proposal does not bypass the fold admission gate, and the interlock is the\n" +
			"point: `remote_user = svc-build` on three hosts IS a real regularity, and the\n" +
			"note stating it names a username — so it is refused and stays local. Being\n" +
			"widespread on your machines does not make something shareable; it makes it a\n" +
			"widespread local fact. Blocked candidates are still listed, with the reason.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPromote(cmd, cfg, promoteMin, promoteCoord, promoteAccept)
		},
	}
	promoteCmd.Flags().IntVar(&promoteMin, "min", DefaultPromotionMin, "entities a fact must hold for before it is proposed")
	promoteCmd.Flags().StringVar(&promoteCoord, "coordinate", "", "coordinate to record the promoted fold at (default: this host's)")
	promoteCmd.Flags().StringVar(&promoteAccept, "accept", "", "promote the candidate with this key")

	root.AddCommand(history, study, find, compose, learn, importCmd, factsCmd, fold, foldsCmd, promoteCmd)
	return root
}

// parseEntity reads the `kind:name` form. A bare word is a host, because that
// is what an operator means nine times in ten and demanding a prefix for the
// common case is friction with no payoff.
func parseEntity(s string) (Entity, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Entity{}, fmt.Errorf("craft: no entity given (try host:workshop)")
	}
	kind, name, ok := strings.Cut(s, ":")
	if !ok {
		return Entity{Kind: EntityHost, Name: s}, nil
	}
	e := Entity{Kind: EntityKind(strings.ToLower(kind)), Name: name}
	switch e.Kind {
	case EntityHost, EntityService, EntityAccount, EntityEndpoint:
	default:
		return Entity{}, fmt.Errorf("craft: unknown entity kind %q (host, service, account, endpoint)", kind)
	}
	if !e.Valid() {
		return Entity{}, fmt.Errorf("craft: entity %q has no name", s)
	}
	return e, nil
}

func runImport(cmd *cobra.Command, cfg *config, sshConfig string, dryRun bool) error {
	path := strings.TrimSpace(sshConfig)
	if path == "" {
		path = DefaultSSHConfigPath()
	}
	if path == "" {
		return fmt.Errorf("craft: no ssh config to read and no home directory to find one in")
	}

	entries := ParseSSHConfig(path)
	facts := SSHConfigFacts(entries, "ssh-config")
	if len(facts) == 0 {
		// Not an error. An operator with no ssh config, or one holding only
		// wildcard defaults, has simply declared nothing — and reporting that as
		// a failure would make a clean machine look broken.
		fmt.Fprintf(cmd.ErrOrStderr(), "craft: nothing declared in %s\n", path)
		return nil
	}

	out := cmd.OutOrStdout()
	store := OpenFacts(cfg.storeDir)
	for _, f := range facts {
		if dryRun {
			fmt.Fprintf(out, "%s\t%s\t%s\n", f.Entity.ID(), f.Key, f.Value)
			continue
		}
		if err := store.Record(f); err != nil {
			return err
		}
	}

	verb := "recorded"
	if dryRun {
		verb = "would record"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "craft: %s %d facts about %d host(s) from %s (host-local; never shared)\n",
		verb, len(facts), len(entries), path)
	return nil
}

func runLearn(cmd *cobra.Command, cfg *config, entity, key, value, source string, forget bool) error {
	e, err := parseEntity(entity)
	if err != nil {
		return err
	}
	store := OpenFacts(cfg.storeDir)
	if forget {
		// Say what actually happened. Invalidating something never believed is
		// a no-op rather than an error — but reporting it as "forgot" is a
		// statement that a claim was withdrawn, and a caller correcting a
		// mistyped key would read that as done and move on.
		if !believes(store, e, key) {
			fmt.Fprintf(cmd.ErrOrStderr(), "craft: nothing believed about %s for %s — nothing to forget\n", key, e.ID())
			return nil
		}
		if err := store.Invalidate(e, key, time.Now().UTC()); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "craft: forgot %s about %s\n", key, e.ID())
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("craft: no value given for %q (use --forget to invalidate instead)", key)
	}
	if err := store.Record(Fact{Entity: e, Key: key, Value: value, Source: source}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "craft: learned %s about %s (host-local; never shared)\n", key, e.ID())
	if !IsTransferableRole(key) {
		// Stored, and honestly labelled. A key outside the role vocabulary is
		// recorded and shown by `craft facts`, but the suggestion path keys on
		// ROLES, so nothing will ever offer this one to a command.
		names := make([]string, 0, len(TransferableRoles()))
		for _, r := range TransferableRoles() {
			names = append(names, string(r))
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"craft: %q is not a role, so it is remembered but never offered to a command (roles: %s)\n",
			key, strings.Join(names, ", "))
	}
	return nil
}

// believes reports whether the store currently holds a value for (entity, key).
func believes(store *FactStore, e Entity, key string) bool {
	for _, f := range store.For(e) {
		if f.Key == key {
			return true
		}
	}
	return false
}

func runPromote(cmd *cobra.Command, cfg *config, min int, coord, accept string) error {
	facts := OpenFacts(cfg.storeDir)
	folds := OpenFolds(cfg.storeDir, HostScrubber(cfg.storeDir))
	cands := facts.PromotionCandidates(min, folds)

	out := cmd.OutOrStdout()
	if len(cands) == 0 {
		fmt.Fprintf(out, "nothing has repeated across %d entities yet\n", min)
		return nil
	}

	if accept != "" {
		if strings.TrimSpace(coord) == "" {
			coord = cfg.coordinate()
		}
		for _, c := range cands {
			if c.Key != accept {
				continue
			}
			if err := Promote(c, coord, folds); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "craft: promoted %q to a fold at %s\n", c.Note, skills.ShortID(coord))
			return nil
		}
		return fmt.Errorf("craft: no candidate with key %q (run without --accept to list)", accept)
	}

	for _, c := range cands {
		mark := "  "
		if !c.Promotable() {
			mark = "! "
		}
		fmt.Fprintf(out, "%s%s\n", mark, c.Note)
		fmt.Fprintf(out, "    seen on %d: ", len(c.Entities))
		names := make([]string, 0, len(c.Entities))
		for _, e := range c.Entities {
			names = append(names, e.ID())
		}
		fmt.Fprintln(out, strings.Join(names, ", "))
		if !c.Promotable() {
			// Reported, never hidden: "this repeats but cannot travel" is
			// itself worth knowing.
			fmt.Fprintf(out, "    BLOCKED: names host identity, so it stays a fact\n")
			continue
		}
		fmt.Fprintf(out, "    accept: bashy craft promote --accept %s --coordinate <key>\n", c.Key)
	}
	return nil
}

func runFold(cmd *cobra.Command, cfg *config, note, coord, capability, evidence, source string, retire bool) error {
	if strings.TrimSpace(coord) == "" {
		coord = cfg.coordinate()
	}
	if strings.TrimSpace(coord) == "" {
		return fmt.Errorf("craft: no coordinate given and this host's could not be read — a fold that holds " +
			"nowhere in particular holds nowhere (`bashy skill probe` prints this host's)")
	}
	store := OpenFolds(cfg.storeDir, HostScrubber(cfg.storeDir))
	if retire {
		if err := store.Retire(capability, coord, note, time.Now().UTC()); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "craft: retired that fold at %s\n", skills.ShortID(coord))
		return nil
	}
	if err := store.Record(Fold{
		Capability: capability, Coordinate: coord,
		Note: note, Evidence: evidence, Source: source,
	}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "craft: folded at %s (generalisable; shareable)\n", skills.ShortID(coord))
	return nil
}

func runFolds(cmd *cobra.Command, cfg *config, coord string) error {
	store := OpenFolds(cfg.storeDir, HostScrubber(cfg.storeDir))
	out := cmd.OutOrStdout()
	if coord == "" {
		coords := store.Coordinates()
		if len(coords) == 0 {
			fmt.Fprintln(out, "nothing folded yet")
			fmt.Fprintln(out, "folds accrue from `craft fold`, and unlike facts they CAN be shared")
			return nil
		}
		// Which of these is HERE is the one thing the list cannot be read
		// without: a coordinate is a hash, and a reader comparing it by eye to
		// the one in another command's output is a reader about to get it
		// wrong.
		here := cfg.coordinate()
		for _, c := range coords {
			mark := ""
			if c == here {
				mark = "  (here)"
			}
			fmt.Fprintf(out, "%-20s %d fold(s)%s\n", skills.ShortID(c), len(store.For("", c)), mark)
		}
		return nil
	}
	folds := store.For("", coord)
	if len(folds) == 0 {
		fmt.Fprintf(out, "nothing folded at %s\n", skills.ShortID(coord))
		return nil
	}
	for _, f := range folds {
		fmt.Fprintf(out, "%s\n", f.Note)
		if f.Evidence != "" {
			fmt.Fprintf(out, "    evidence: %s\n", f.Evidence)
		}
	}
	return nil
}

func runFacts(cmd *cobra.Command, cfg *config, entity string) error {
	store := OpenFacts(cfg.storeDir)
	out := cmd.OutOrStdout()

	if entity == "" {
		ents := store.Entities()
		if len(ents) == 0 {
			fmt.Fprintln(out, "nothing learned yet")
			fmt.Fprintln(out, "facts accrue from `craft learn`, and never leave this host")
			return nil
		}
		for _, e := range ents {
			fmt.Fprintf(out, "%-28s %d fact(s)\n", e.ID(), len(store.For(e)))
		}
		return nil
	}

	e, err := parseEntity(entity)
	if err != nil {
		return err
	}
	facts := store.For(e)
	if len(facts) == 0 {
		fmt.Fprintf(out, "nothing learned about %s\n", e.ID())
		return nil
	}
	for _, f := range facts {
		fmt.Fprintf(out, "%-20s %s", f.Key, f.Value)
		if f.Source != "" {
			fmt.Fprintf(out, "   (from %s)", f.Source)
		}
		fmt.Fprintln(out)
	}
	return nil
}

func runFind(cmd *cobra.Command, cfg *config, query string, limit int, asJSON bool) error {
	ix := cfg.index()
	matches := ix.Resolve(Query{Text: query, Limit: limit})
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"schema": "bashy-craft-find-v1", "query": query,
			"indexed": ix.Len(), "matches": matches,
		})
	}
	out := cmd.OutOrStdout()
	if len(matches) == 0 {
		// Nothing, rather than the least-bad row: a confident wrong answer
		// propagates, and the agent acts on it with nothing reporting the error.
		fmt.Fprintf(out, "no capability matches %q\n", query)
		if ix.Len() == 0 {
			// A different statement of fact, and the difference matters: an
			// empty index means the question was never really asked. Reported
			// as "no match" it reads as "this host cannot do that", which is a
			// conclusion drawn from an absence of evidence.
			fmt.Fprintf(out, "nothing in the catalog carries a contract yet, so there is nothing to search\n")
			return nil
		}
		fmt.Fprintf(out, "%d capabilit%s indexed; only skills carrying a contract are searchable, and a match must "+
			"account for more than half your question\n", ix.Len(), plural(ix.Len(), "y is", "ies are"))
		return nil
	}
	for _, m := range matches {
		fmt.Fprintf(out, "%-28s %s  score %.0f", craftTruncate(m.Name, 28), skills.ShortID(m.Key), m.Score)
		if m.Alternatives > 0 {
			fmt.Fprintf(out, "  (+%d alternative)", m.Alternatives)
		}
		fmt.Fprintln(out)
		if len(m.Why) > 0 {
			// The coverage is printed beside the signals because it is the half
			// that says how much of the QUESTION was answered — a high score on
			// one word out of five is exactly the shape of a wrong answer.
			fmt.Fprintf(out, "%-28s   matched on: %s (%d/%d terms)\n", "",
				strings.Join(m.Why, ", "), m.Covered, m.Terms)
		}
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func runCompose(cmd *cobra.Command, cfg *config, query string, band int, asJSON bool, forEntity, coordinate string) error {
	matches := cfg.index().Resolve(Query{Text: query, Limit: 1})
	if len(matches) == 0 {
		return fmt.Errorf("craft: no capability matches %q — nothing to compose", query)
	}
	// Take the revision BEFORE reading any store, so the stamp records the
	// state the composition was read against rather than the state it ended at.
	// The window is small and nothing here writes, but on a host several agents
	// share it is not zero, and a rev taken afterwards would name a store the
	// composition never saw.
	opts := ComposeOptions{Band: band, GraphVersion: Revision(cfg.storeDir).String()}
	if strings.TrimSpace(forEntity) != "" {
		e, err := parseEntity(forEntity)
		if err != nil {
			return err
		}
		opts.Entity = e
		opts.Facts = OpenFacts(cfg.storeDir).For(e)
	}
	// Compose HERE unless told otherwise. A rendering that silently omitted the
	// workarounds this host already knows about would be the same lie in the
	// other direction: correct-looking, and missing the part a fresh agent would
	// otherwise rediscover by failing.
	if strings.TrimSpace(coordinate) == "" {
		coordinate = cfg.coordinate()
	}
	if strings.TrimSpace(coordinate) != "" {
		opts.Coordinate = coordinate
		opts.Folds = OpenFolds(cfg.storeDir, HostScrubber(cfg.storeDir)).For(matches[0].Key, coordinate)
	}
	c, err := Compose(matches[0].Primary, opts)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	}
	out := cmd.OutOrStdout()
	fmt.Fprint(out, c.Body)
	// Provenance on stderr so stdout stays the artifact — a caller piping this
	// into a file or an agent's context must get the skill, not a header.
	fmt.Fprintf(cmd.ErrOrStderr(),
		"\ncraft: %s band=%d floor=%d bands=%v determinism=%.2f coordinate=%s folds=%d facts=%d graph=%s stamp=%s\n",
		c.Name, c.Band, c.Floor, c.Bands, c.DeterminismRatio, skills.ShortID(c.Coordinate), c.Folds, c.Facts,
		orUnknown(c.GraphVersion), c.Stamp)
	return nil
}

func runStudy(cmd *cobra.Command, cfg *config, dir string, src Source, asJSON bool, agent string, band int, lang string) error {
	// Build the normaliser BEFORE reading anything. An under-banded or
	// misconfigured agent must fail here, loudly and with nothing absorbed —
	// not halfway through a run with part of a catalog already quarantined
	// under a reason that blames the content rather than the configuration.
	var norm Normaliser
	if strings.TrimSpace(agent) != "" {
		n, err := NewNormaliser(ExecCompleter(agent), NormaliserOptions{Band: band, Lang: lang})
		if err != nil {
			return err
		}
		norm = n
	}

	cands, err := LoadDir(dir, src)
	if err != nil {
		return fmt.Errorf("craft: reading %s: %w", dir, err)
	}
	if len(cands) == 0 {
		return fmt.Errorf("craft: no skill folders found under %s (a candidate is a directory holding a SKILL.md)", dir)
	}

	// Absorption resolves against the capabilities already held. Reading the
	// ledger is a stand-in until the catalog itself is capability-indexed:
	// evidence is where capabilities are currently observable.
	known := map[string][]string{}
	if l, err := ReadLedger(cfg.storeDir); err == nil {
		for _, o := range l.Observations {
			if o.Capability != "" {
				known[o.Capability] = appendDistinct(known[o.Capability], o.Identity)
			}
		}
	}

	rep := Study(cands, known, norm)

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	out := cmd.OutOrStdout()
	for _, o := range rep.Outcomes {
		line := fmt.Sprintf("%-12s %-28s", o.Disposition, craftTruncate(o.Name, 28))
		if o.Capability != "" {
			line += " " + skills.ShortID(o.Capability)
		}
		fmt.Fprintln(out, line)
		if o.Reason != "" {
			fmt.Fprintf(out, "%-12s   %s\n", "", o.Reason)
		}
	}
	fmt.Fprintf(out, "\n%d absorbed, %d capabilities added — digestion ratio %.2f\n",
		rep.Absorbed, rep.Grew, rep.DigestionRatio())
	if rep.DigestionRatio() >= 1.0 && rep.Absorbed > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"craft: ratio is 1.00 — every skill became its own capability, so nothing was digested\n")
	}
	return nil
}

// ExitCode maps an Execute error to the repo exit convention.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// craftTruncate shortens a display cell to n runes, rune-aware so a multi-byte
// name is not cut mid-character.
func craftTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// historyRow is one summarised subject in the ledger — a skill, or a capability.
type historyRow struct {
	Subject     string   `json:"subject"`
	Kind        string   `json:"kind"` // "skill" | "capability"
	Runs        int      `json:"runs"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	Yielded     int      `json:"yielded,omitempty"`
	Rate        float64  `json:"contribution"`
	Coordinates []string `json:"coordinates,omitempty"`
	Tiers       []string `json:"tiers,omitempty"`
	First       string   `json:"first,omitempty"`
	Last        string   `json:"last,omitempty"`
}

type historyReport struct {
	Schema string        `json:"schema"`
	Rows   []historyRow  `json:"rows"`
	Runs   []Observation `json:"observations,omitempty"`
	// Malformed is surfaced, never swallowed: evidence that failed to decode
	// is missing evidence, and a summary that hid it would present an absence
	// as a clean result.
	Malformed int      `json:"malformed,omitempty"`
	Files     []string `json:"files,omitempty"`
}

const historySchema = "bashy-craft-history-v1"

func runHistory(cmd *cobra.Command, cfg *config, name, capability string, all, asJSON bool) error {
	l, err := ReadLedger(cfg.storeDir)
	if err != nil {
		return fmt.Errorf("craft: reading the attestation ledger: %w", err)
	}

	rep := historyReport{Schema: historySchema, Malformed: l.Malformed, Files: l.Files}
	switch {
	case capability != "":
		rep.Rows = []historyRow{rowOf(capability, "capability", l.ForCapability(capability))}
		if all {
			rep.Runs = filterObs(l.Observations, func(o Observation) bool { return o.Capability == capability })
		}
	case name != "":
		rep.Rows = []historyRow{rowOf(name, "skill", l.ForSkill(name))}
		if all {
			rep.Runs = filterObs(l.Observations, func(o Observation) bool { return o.Name == name })
		}
	default:
		for _, n := range l.Names() {
			rep.Rows = append(rep.Rows, rowOf(n, "skill", l.ForSkill(n)))
		}
		if all {
			rep.Runs = l.Observations
		}
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	writeHistoryText(cmd, rep, name, capability)
	return nil
}

func rowOf(subject, kind string, s Stats) historyRow {
	r := historyRow{
		Subject:     subject,
		Kind:        kind,
		Runs:        s.Runs,
		Passed:      s.Passed,
		Failed:      s.Failed,
		Yielded:     s.Yielded,
		Rate:        s.Contribution(),
		Coordinates: s.Coordinates,
		Tiers:       s.Tiers,
	}
	if !s.First.IsZero() {
		r.First = s.First.UTC().Format(time.RFC3339)
	}
	if !s.Last.IsZero() {
		r.Last = s.Last.UTC().Format(time.RFC3339)
	}
	return r
}

func filterObs(in []Observation, keep func(Observation) bool) []Observation {
	var out []Observation
	for _, o := range in {
		if keep(o) {
			out = append(out, o)
		}
	}
	return out
}

func writeHistoryText(cmd *cobra.Command, rep historyReport, name, capability string) {
	out := cmd.OutOrStdout()

	total := 0
	for _, r := range rep.Rows {
		total += r.Runs
	}
	if total == 0 {
		// An empty ledger is a legitimate state, not a failure — and saying
		// so plainly beats printing an empty table that reads like a bug.
		subject := "any skill"
		if capability != "" {
			subject = "capability " + skills.ShortID(capability)
		} else if name != "" {
			subject = name
		}
		fmt.Fprintf(out, "no recorded runs for %s\n", subject)
		fmt.Fprintf(out, "evidence accrues from `skills run`; only contracted skills attest\n")
		return
	}

	fmt.Fprintf(out, "%-28s %5s %5s %5s %7s  %s\n", "SKILL", "RUNS", "PASS", "FAIL", "RATE", "COORDINATES")
	for _, r := range rep.Rows {
		subject := r.Subject
		if r.Kind == "capability" {
			subject = skills.ShortID(subject)
		}
		fmt.Fprintf(out, "%-28s %5d %5d %5d %+7.2f  %d\n",
			craftTruncate(subject, 28), r.Runs, r.Passed, r.Failed, r.Rate, len(r.Coordinates))
	}

	if len(rep.Runs) > 0 {
		fmt.Fprintln(out)
		for _, o := range rep.Runs {
			state := "FAIL"
			switch {
			case o.Yielded():
				state = "yield"
			case o.Valid:
				state = "pass"
			}
			fmt.Fprintf(out, "%s  %-5s  %-24s %s  %s\n",
				o.At.UTC().Format(time.RFC3339), state, craftTruncate(o.Name, 24), skills.ShortID(o.ContextKey), o.Tier)
		}
	}

	if rep.Malformed > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"skills: %d ledger line(s) could not be decoded and are NOT counted above\n", rep.Malformed)
	}
}
