// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package lexicon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ref"
)

// Synopses is set by the embedding shell (bashy) so the lexicon can carry a verb's
// one-liner. The atlas holds classification, not prose; this keeps the package
// usable by any project rather than hard-wiring bashy's help text.
var Synopses = map[string]string{}

// NewLexiconCmd builds `bashy lexicon`.
func NewLexiconCmd(opts ...fleet.Option) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lexicon",
		Short: "what do this project's words mean HERE?",
		Long: `lexicon is the project's jargon, projected from the registries that already
define it — never a hand-written glossary.

The problem it solves: a user says "handoff this to codex". Neither word means what
the dictionary says. "handoff" is a bashy verb; "codex" is an agent binding ON THIS
HOST (a CLI tool plus a bound model), and the same word denotes a different binding
on another machine.

It is 20% glossary and 80% PRECEDENCE + LOOKUP:

  precedence   one sentence, in the always-on tier every tool reads:
               "in this workspace these words are never their English senses"
  lookup       a name is RESOLVED, never memorised

It STORES NOTHING. Verbs are projected from the Command Atlas, agent bindings from
the fleet registry. Only two things are hand-written, because a machine cannot infer
them: what the team actually SAYS (alt labels), and the precedence rule (scope
notes). Everything that can go stale is generated.

In written artifacts a term may be marked [[handoff]] — that is how the term set is
TAUGHT and how mentions become machine-detectable. In conversation the word is used
plainly, like any jargon. The marker is optional emphasis, never required syntax.`,
		Example: `  bashy lexicon                          # the vocabulary of this project
  bashy lexicon resolve codex --json     # what does that word mean HERE?
  bashy lexicon emit --write AGENTS.md   # seed every tool's always-on tier
  bashy lexicon scan docs/               # find [[terms]] that resolve to NOTHING`,
	}
	cmd.AddCommand(newListCmd(opts), newResolveCmd(opts), newEmitCmd(opts), newScanCmd(opts),
		NewDefineCmd(opts...), NewStudyCmd(opts...))
	return cmd
}

// RecordDiscovery is set by the embedding shell to route identity-bearing
// findings into a host-local fact store.
//
// A function var rather than an import because the two belong to different
// layers and must not be coupled: a glossary that could write to a fact store
// would eventually be asked to read from one, and identity would start flowing
// the wrong way. nil means collection reports its findings and stores none —
// which is the right default for a package whose job is vocabulary.
var RecordDiscovery func(Discovery) error

// NewStudyCmd builds the `study` verb: prepopulate the glossary by asking the
// machine what exists.
func NewStudyCmd(opts ...fleet.Option) *cobra.Command {
	var asJSON, dryRun bool
	cmd := &cobra.Command{
		Use:   "study",
		Short: "prepopulate the glossary by asking this machine what exists",
		Long: `study collects the names this host actually carries — network interfaces,
mounted volumes — and adds them to the vocabulary.

It is the counterpart to enumeration, one layer down: Enumerate reads what the
SHELL knows (env keys, PATH, the working directory), while study asks the OS.
Interface names like en0, utun3, docker0 or tailscale0 are dense local jargon,
and an agent that meets one in a log has nowhere to look it up.

Collection finds two KINDS of thing and separates them at the source:

  NAMES      an interface or volume name is VOCABULARY — utun3 means the same
             kind of thing on any machine that has one
  ADDRESSES  an address is IDENTITY — it says WHICH machine this is, so it goes
             to the host-local fact store, never the glossary

That split is why study can be run freely: nothing identity-bearing reaches the
shareable side, and every address it does record makes the fold admission gate
stricter, because it is one more thing the scrubber can recognise.

Numbers are deliberately not collected. CPU load and bytes free are telemetry,
not vocabulary; ` + "`bashy resources`" + ` already reports them.`,
		// `bashy lexicon study`, NOT `bashy define study` — the latter asks what
		// the WORD "study" means, and always will. `define` takes an arbitrary
		// token and therefore has no subcommands; an example that spells it the
		// other way teaches the one mistake this command tree is shaped to
		// prevent.
		Example: `  bashy lexicon study            # collect, record, and report
  bashy lexicon study --dry-run  # show what WOULD be collected`,
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, found := Discover(CollectOptions{})

			out := cmd.OutOrStdout()
			if asJSON {
				b, _ := json.MarshalIndent(map[string]any{
					"schema":      "bashy-lexicon-study-v1",
					"terms":       inv,
					"identities":  len(found),
					"dry_run":     dryRun,
					"recorded_to": "fact store (host-local)",
				}, "", "  ")
				fmt.Fprintln(out, string(b))
				return nil
			}

			for _, n := range inv.Interfaces {
				fmt.Fprintf(out, "%-20s interface\n", n)
			}
			for _, n := range inv.Mounts {
				fmt.Fprintf(out, "%-20s mount\n", n)
			}
			fmt.Fprintf(out, "\n%d term(s) collected\n", len(inv.Interfaces)+len(inv.Mounts))

			// The addresses are COUNTED here, never printed: this output is
			// routinely piped, pasted, and read by an agent, and the whole point
			// of separating them was to keep them off that path.
			switch {
			case dryRun:
				fmt.Fprintf(out, "%d identity value(s) found; --dry-run, nothing recorded\n", len(found))
			case RecordDiscovery == nil:
				fmt.Fprintf(out, "%d identity value(s) found; no fact store wired, nothing recorded\n", len(found))
			default:
				n := 0
				for _, d := range found {
					if err := RecordDiscovery(d); err == nil {
						n++
					}
				}
				fmt.Fprintf(out, "%d identity value(s) recorded to the host-local fact store\n", n)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable report")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report without recording")
	return cmd
}

// NewDefineCmd builds the `define` verb. Exported so a host can mount it at top
// level as well as under `lexicon` — it is the question agents ask most, and
// burying it two words deep costs more than the namespace is worth.
//
// IT MUST NEVER GAIN A SUBCOMMAND. Its argument is an arbitrary user token, so
// every subcommand name would permanently remove a word from the definable
// vocabulary: mount `study` here and `bashy define study` stops meaning "what is
// the word study". Worse, the hole is invisible — nothing fails until somebody
// asks about that exact word, and then the answer is a help screen. Actions
// belong under `lexicon`, whose arguments are a closed set.
//
// TestDefineCmd_HasNoSubcommands pins this.
func NewDefineCmd(opts ...fleet.Option) *cobra.Command {
	var asJSON bool
	var kindFilter []string
	var listKinds bool
	cmd := &cobra.Command{
		Use:   "define <term>",
		Short: "what is this word on THIS system? (verb, agent, env var, command, path, address — or unknown)",
		Long: `define answers "what is this word, here?" for any token.

Three kinds of answer, and the third is the one that matters:

  KNOWN    the term is in a projected registry — a bashy verb, an agent binding,
           a skill, an environment variable, a local command, a path segment.
  SHAPED   no registry knows it, but its FORM is recognisable: an address, a
           UUID, a git sha — or something shaped like a CREDENTIAL.
  UNKNOWN  genuinely not known. Said plainly.

Saying "I don't know" is a feature. A resolver that guesses is worse than no
resolver, because a confident wrong definition propagates: the agent acts on it
and nothing reports the error.

A term that looks like a credential is classified but NEVER echoed back, never
stored, and never looked up. "That is an API key" is a useful answer; repeating
the key into a terminal, a log, or an agent transcript is how it ends up
somewhere permanent.

A REF is answered by its store. Anything bashy can name has one canonical
address, <kind>:<id> (kb:deploy-runbook, todo:a5f5c1d2e3b4, sprint:168,
run:coreutils-7, agent:codex …; urn:dhnt:<kind>:<id> is the same ref spelled
for text that leaves bashy). define parses it, asks the store that owns the
kind, and prints the record's title, status, where it lives and the command
that opens it. Only the vocabulary counts: codex:gpt5.6-sol is a tool:model
binding and stays a word. A ref that names nothing is an error, and it says
which of three things happened — the store has no such id, the kind is not in
the vocabulary, or this build never wired a resolver for the kind (a hook
left nil is not an empty store). --list-kinds prints the vocabulary.`,
		Example: `  bashy define handoff        # a bashy verb
  bashy define codex          # an agent binding ON THIS HOST
  bashy define WEAVE_AGENT    # an environment variable this fleet sets
  bashy define outpost        # a local command, outside the standard userland
  bashy define sk-proj-...    # classified as a credential, and not echoed
  bashy define kb:deploy-runbook       # a ref: the kb page, from the kb store
  bashy define urn:dhnt:sprint:168     # the same grammar, fully qualified`,
		// MaximumNArgs, not ExactArgs: `--list-kinds` takes no term, and under
		// ExactArgs it could not be reached at all — `bashy define --list-kinds`
		// failed with "accepts 1 arg(s), received 0". That is the flag the
		// unknown-term advice points a caller at, so the one path out of "I
		// don't know" ended in a usage error.
		//
		// Still NO subcommands, and that is unrelated to arity: the argument is
		// an arbitrary token either way.
		Args: cobra.MaximumNArgs(1),
		// A ref that names nothing is an ANSWER (exit 1 with the reason), not a
		// usage mistake: printing the flag table under it buries the one line
		// the caller needs, and the embedding shell already prints the error
		// once. Usage still shows for a real arity error via the explicit
		// message below.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 && !listKinds {
				return fmt.Errorf("define needs exactly one term — the word to look up " +
					"(`--list-kinds` lists the namespaces this host can answer for instead)")
			}
			if listKinds {
				out := cmd.OutOrStdout()
				fmt.Fprintln(out, strings.Join(buildFull(opts).Kinds(), "\n"))
				// The ref vocabulary is a second group: those kinds are answered
				// by their stores through RefResolvers, not by the projections.
				fmt.Fprintln(out)
				fmt.Fprintln(out, "refs:")
				for _, k := range ref.KindNames() {
					fmt.Fprintf(out, "  %s\n", k)
				}
				return nil
			}
			// A ref is answered by its store, before any projection is built:
			// the token's shape decides, and only a non-ref reaches the term
			// path (see defineRef for the outcomes it owns).
			if err := defineRef(cmd.OutOrStdout(), args[0], asJSON); !errors.Is(err, errNotARef) {
				return err
			}
			s := buildFull(opts)
			kinds := make([]Kind, 0, len(kindFilter))
			for _, k := range kindFilter {
				kinds = append(kinds, Kind(strings.TrimSpace(k)))
			}
			d := s.DefineKinds(args[0], kinds)
			if asJSON {
				b, _ := json.MarshalIndent(d, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			out := cmd.OutOrStdout()
			switch {
			case d.Sensitive:
				// The term is deliberately absent from this output.
				fmt.Fprintf(out, "‹not shown›  %s\n", d.Classification)
				fmt.Fprintf(out, "  %s\n", d.Advice)
			case d.Found:
				// EVERY reading, not just the first: one string is often several
				// things at once, and which one the caller meant is not ours to
				// decide for them.
				if len(d.Concepts) > 1 {
					fmt.Fprintf(out, "%s is %d things here:\n\n", d.Term, len(d.Concepts))
				}
				for i, c := range d.Concepts {
					if i > 0 {
						fmt.Fprintln(out)
					}
					writeConcept(out, c)
				}
			case d.Classification != "":
				fmt.Fprintf(out, "%s  — %s\n", d.Term, d.Classification)
				fmt.Fprintf(out, "  %s\n", d.Advice)
			default:
				fmt.Fprintf(out, "%s  — unknown here\n", d.Term)
				fmt.Fprintf(out, "  %s\n", d.Advice)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable answer")
	cmd.Flags().StringSliceVar(&kindFilter, "kind", nil, "only these namespaces (repeatable); --list-kinds shows what is available")
	cmd.Flags().BoolVar(&listKinds, "list-kinds", false, "print the namespaces this host can answer for")
	return cmd
}

func writeConcept(out io.Writer, c *Concept) {
	fmt.Fprintf(out, "%s  (%s)\n", c.PrefLabel, c.Kind)
	if c.Definition != "" {
		fmt.Fprintf(out, "  %s\n", c.Definition)
	}
	if c.Host != "" {
		fmt.Fprintf(out, "  host: %s\n", c.Host)
	}
	if len(c.AltLabels) > 0 {
		fmt.Fprintf(out, "  also: %s\n", strings.Join(c.AltLabels, ", "))
	}
	if c.Location != "" {
		// The `which` half of the answer. An agent told only "it is a command"
		// still cannot act; it needs to know WHICH binary, and an alias changes
		// what a name runs entirely.
		label := "at:  "
		if c.Kind == KindAlias {
			label = "runs:"
		}
		fmt.Fprintf(out, "  %s %s\n", label, c.Location)
	}
	if c.Use != "" {
		fmt.Fprintf(out, "  use:  %s\n", c.Use)
	}
	if a := c.Action; a != nil {
		// What running it amounts to, on one line: the contract that binds it,
		// how much freedom the run takes, what it may do, and what runs it.
		line := fmt.Sprintf("%s contract=%s latitude=%s authority=%s", a.Kind, a.Contract, a.Latitude, a.Authority)
		if len(a.EffectsDeclared) > 0 {
			line += " effects=" + strings.Join(a.EffectsDeclared, ",")
		}
		fmt.Fprintf(out, "  runs: %s via %s (%s)\n", line, a.Executor, a.Envelope)
	}
	if c.ScopeNote != "" {
		fmt.Fprintf(out, "  note: %s\n", c.ScopeNote)
	}
	if c.Source != "" {
		fmt.Fprintf(out, "  from: %s\n", c.Source)
	}
}

// KnownCommands is set by the embedding shell: the standard command set to
// subtract when enumerating this host's local commands. Same reasoning as
// Synopses — the atlas knows this, and passing it in keeps the package usable
// by any project rather than hard-wiring bashy.
var KnownCommands []string

// SkillSource is the seam to the skill catalog, injected by the embedding
// shell — the same shape as SeatSource, and for the same reason: the ring is
// mounted by the shell (its embedded FS is bashy's, not this package's), and
// pkg/lexicon must not import pkg/skills (see SkillRow). nil means the host
// cannot answer for skills, so `define <skill>` says "unknown here" rather
// than guessing. WIRING IT IS THE LOAD-BEARING STEP.
var SkillSource func() []SkillRow

func build(opts []fleet.Option) *Store {
	host, _ := os.Hostname()
	return Build(fleet.New(opts...), Synopses, host, Overlay{})
}

// buildFull adds the host's system inventory to the registry projections.
//
// Separate from build because it costs a PATH scan: a caller that only wants
// verb resolution should not pay for one. Lookup paths use it, because the
// whole point of the inventory is that `bashy define` can answer for terms no
// registry declared.
func buildFull(opts []fleet.Option) *Store {
	s := build(opts)

	// The standard userland. NOT jargon — and that is why it belongs: a verb
	// called `define` that answers "unknown" for `ls` is wrong on its own terms.
	s.AddStandardTools(atlas.ToolNames(), Overlay{})

	roots := []string{}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	// The standard set is subtracted from the LOCAL command enumeration, so a
	// name is either standard or peculiar to this host, never both. Sourced from
	// the atlas directly rather than from the embedding shell: the atlas is
	// ratcheted (its coverage tests fail the build when a tool has no entry), so
	// it cannot fall behind, and nothing has to remember to wire it.
	known := append(atlas.ToolNames(), atlas.VerbNames()...)
	known = append(known, KnownCommands...)
	s.AddSystem(EnumerateHost(roots, known), Overlay{})

	// Names the OS knows but the shell does not: interfaces, mounted volumes.
	// Only the NAMES — Discover's identity half is dropped on the floor here,
	// because a glossary is exactly where it must not go.
	collected, _ := Discover(CollectOptions{})
	s.AddCollected(collected, Overlay{})

	// The host's own name and its role seats. Both are ADDRESSES, and both were
	// missing: `define <this machine>` answered "unknown here", and `define
	// steward` returned the verb rather than the seat somebody was holding.
	s.AddReachable(Overlay{})

	// The skill catalog — a capability, run by name, with its action facet.
	if SkillSource != nil {
		s.AddSkills(SkillSource(), Overlay{})
	}
	return s
}

func newListCmd(opts []fleet.Option) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "every term that resolves in this project",
		RunE: func(cmd *cobra.Command, args []string) error {
			s := build(opts)
			if asJSON {
				b, _ := json.MarshalIndent(s.Concepts, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			for _, c := range s.Concepts {
				fmt.Fprintf(cmd.OutOrStdout(), "%-22s %-14s %s\n", c.PrefLabel, c.Kind, oneLine(c.Definition))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the concepts")
	return cmd
}

func newResolveCmd(opts []fleet.Option) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "resolve <term>",
		Short: "what does this word mean HERE?",
		Long: `resolve answers the question that makes jargon work: what does this word denote
in THIS workspace, on THIS host?

A name is resolved by a lookup, never memorised. That one rule is what lets "codex"
mean a live binding here and something else on another machine, without anyone
maintaining a glossary.

It accepts the bare word, the [[marked]] form, and a namespaced form ([[agent:codex]]).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := build(opts)
			c, ok := s.Resolve(args[0])
			if !ok {
				// An unknown term is an ERROR, not an empty answer. Silence would
				// invite the agent to fall back on the English word — the exact
				// failure this whole feature exists to prevent.
				return fmt.Errorf("%q is not a term in this project's lexicon.\n"+
					"It may simply be an ordinary English word — but if you expected it to name "+
					"something here, `bashy lexicon list` shows what does", args[0])
			}
			if asJSON {
				b, _ := json.MarshalIndent(c, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s  (%s)\n", c.PrefLabel, c.Kind)
			if c.Definition != "" {
				fmt.Fprintf(out, "  %s\n", c.Definition)
			}
			if c.Host != "" {
				fmt.Fprintf(out, "  host:  %s\n", c.Host)
			}
			if len(c.AltLabels) > 0 {
				fmt.Fprintf(out, "  also: %s\n", strings.Join(c.AltLabels, ", "))
			}
			if c.Use != "" {
				fmt.Fprintf(out, "  use:  %s\n", c.Use)
			}
			if c.ScopeNote != "" {
				fmt.Fprintf(out, "  note: %s\n", c.ScopeNote)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the concept")
	return cmd
}

func newEmitCmd(opts []fleet.Option) *cobra.Command {
	var write string
	cmd := &cobra.Command{
		Use:   "emit",
		Short: "render the managed lexicon block for AGENTS.md / CLAUDE.md",
		Long: `emit renders the always-on tier: the precedence rule, a SELECTION of the
highest-value terms, and the resolver command.

A selection, not a dump — deliberately. Term/tool selection accuracy DEGRADES past
roughly 15-20 items in active rotation, and near-synonyms are the top failure mode.
More vocabulary in context does not mean better resolution. The long tail is reached
by lookup, which is why the resolver line is the most important one in the block.

With --write it splices the block into a file, replacing any previous one.
Idempotent, so it is safe to wire into a hook or a gate.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s := build(opts)
			cwd, _ := os.Getwd()
			block := s.EmitAgentsMD(filepath.Base(cwd))
			if write == "" {
				fmt.Fprint(cmd.OutOrStdout(), block)
				return nil
			}
			if err := WriteInto(write, block); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "lexicon: managed block written into %s\n", write)
			return nil
		},
	}
	cmd.Flags().StringVar(&write, "write", "", "splice the block into this file (e.g. AGENTS.md)")
	return cmd
}

func newScanCmd(opts []fleet.Option) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan [path...]",
		Short: "find [[terms]] in artifacts that resolve to NOTHING",
		Long: `scan walks the project's artifacts, collects every [[marked]] term, and reports
the ones that resolve to nothing.

This is what makes the lexicon FALSIFIABLE, and it is the property a prose glossary
can never have: a prose glossary rots silently, a linked one cannot. A [[term]] that
resolves to nothing is a broken link, and broken links are findable.

It also means the term set is derived from HOW THE TEAM ACTUALLY WRITES, rather than
from a list someone has to remember to maintain.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{"."}
			}
			s := build(opts)
			broken := map[string][]string{}
			for _, root := range args {
				_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						if d != nil && d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
							return filepath.SkipDir
						}
						return nil
					}
					if !strings.HasSuffix(p, ".md") {
						return nil
					}
					b, err := os.ReadFile(p)
					if err != nil {
						return nil
					}
					if u := s.Unresolved(string(b)); len(u) > 0 {
						broken[p] = u
					}
					return nil
				})
			}
			if len(broken) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "lexicon: every [[term]] resolves")
				return nil
			}
			for p, terms := range broken {
				for _, t := range terms {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: [[%s]] resolves to nothing\n", p, t)
				}
			}
			return fmt.Errorf("%d file(s) contain terms that resolve to nothing", len(broken))
		},
	}
	return cmd
}
