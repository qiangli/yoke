package fleet

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// --- agents add / set --------------------------------------------------

// agentFlags are the fields `agents add` and `agents set` can write. A nil
// pointer means "not given", so `set` can distinguish clearing a field from
// leaving it alone.
type agentFlags struct {
	tool, model, display, description, nick string
	aliases, addAlias, rmAlias              []string
	force, ephemeral                        bool
	paths                                   pathFlags
}

func (f *agentFlags) bind(c *cobra.Command, forSet bool) {
	c.Flags().StringVar(&f.tool, "tool", "", "the agentic CLI half of the binding")
	c.Flags().StringVar(&f.model, "model", "", "the inference-backend half of the binding")
	c.Flags().StringVar(&f.display, "display", "", "human-facing label")
	c.Flags().StringVar(&f.description, "description", "", "what this agent is for")
	c.Flags().StringVar(&f.nick, "nick", "", "the agent's human name; empty means one is assigned from the binding")
	c.Flags().BoolVar(&f.force, "force", false, "take a name that already belongs to another entry")
	c.Flags().BoolVar(&f.ephemeral, "ephemeral", false, "mark this as a one-task agent")
	f.paths.bind(c)
	if forSet {
		c.Flags().StringArrayVar(&f.addAlias, "add-alias", nil, "add a nickname (repeatable)")
		c.Flags().StringArrayVar(&f.rmAlias, "rm-alias", nil, "drop a nickname (repeatable)")
		return
	}
	c.Flags().StringArrayVar(&f.aliases, "alias", nil, "an additional nickname (repeatable)")
}

func newAgentsAdd(opts []Option) *cobra.Command {
	var f agentFlags
	c := &cobra.Command{
		Use:   "add (<nickname> --tool T --model M | <file>|-)",
		Short: "Mint an agent: a named tool:model binding",
		Long: "Mint an agent: a named tool:model binding.\n\n" +
			"Give a nickname with --tool and --model to build the binding from flags,\n" +
			"or give a path (or - for stdin) to import an agent asset document.\n\n" +
			"A nickname is an alias for a binding, not an identity of its own:\n" +
			"`007` and `smarty` may both name claude:fable5, and both resolve to the\n" +
			"same capability-matrix row.",
		Example: "  bashy agent add 007 --tool codex --model deepseek-v4 --alias smarty\n" +
			"  bashy agent add ./conductor.yaml",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			arg := args[0]

			if f.tool == "" && f.model == "" && len(f.paths.set) == 0 && len(f.paths.unset) == 0 && !cmd.Flags().Changed("ephemeral") && looksLikePath(arg) {
				return importAgent(cmd, cat, arg, f.force)
			}
			if len(f.paths.set) == 0 && (f.tool == "" || f.model == "") {
				return fmt.Errorf("fleet: minting %q needs both --tool and --model (an agent always names both)", arg)
			}
			a := Agent{
				Name: arg, Aliases: f.aliases, Tool: f.tool, Model: f.model,
				Display: f.display, Description: f.description, Nick: f.nick, Ephemeral: f.ephemeral,
			}
			if err := applyPathFlags(cmd, KindAgent, &a, f.paths); err != nil {
				return err
			}
			claims := append(append([]string{}, a.Aliases...), a.Nick)
			if err := cat.claimName(KindAgent, a.Name, claims, f.force); err != nil {
				return err
			}
			if err := cat.SaveAgent(a); err != nil {
				return err
			}
			return reportAgentSaved(cmd, cat, a)
		},
	}
	f.bind(c, false)
	return c
}

func importAgent(cmd *cobra.Command, cat *Catalog, path string, force bool) error {
	data, err := readSource(path, cmd.InOrStdin())
	if err != nil {
		return err
	}
	file, err := ParseAgentFile(baseName(path), data, nil)
	if err != nil {
		return err
	}
	for _, a := range file.Agents {
		if err := cat.claimName(KindAgent, a.Name, a.Aliases, force); err != nil {
			return err
		}
		if err := cat.SaveAgent(a); err != nil {
			return err
		}
		if err := reportAgentSaved(cmd, cat, a); err != nil {
			return err
		}
	}
	return nil
}

// reportAgentSaved echoes what was written and whether it can actually run.
// A minted agent whose halves do not resolve is a warning, never an error:
// binding ahead of installing the tool is a legitimate order of work.
func reportAgentSaved(cmd *cobra.Command, cat *Catalog, a Agent) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s → %s\n", a.Name, a.MatrixKey())
	if len(a.Aliases) > 0 {
		fmt.Fprintf(out, "aliases: %s\n", strings.Join(a.Aliases, " "))
	}
	for _, w := range cat.crossKindWarnings(KindAgent, a.Name, a.Aliases) {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
	chk := cat.VerifyAgent(a.Name, Probes(nil))
	if !chk.OK {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", chk.Reason)
	}
	return nil
}

// crossKindWarnings reports names that already mean something else.
//
// Names are unique WITHIN a kind, not across kinds, so `bashy agent add
// claude` is legal. But it makes `whois claude` ambiguous, and the places that
// resolve a name — `bashy chat --agent`, `weave start -- <name>` — try the
// agent first, so the new nickname silently shadows the tool. Say so.
func (c *Catalog) crossKindWarnings(kind, canonical string, aliases []string) []string {
	var out []string
	for _, n := range names(canonical, aliases) {
		if kind != KindTool {
			if t, ok := c.Tool(n); ok {
				out = append(out, fmt.Sprintf("%q also names a tool%s; `whois %s` is now ambiguous, and a launcher resolving %q will prefer this %s", n, alsoKnownAs(t.Name, n), n, n, kind))
			}
		}
		if kind != KindModel {
			if m, ok := c.Model(n); ok {
				out = append(out, fmt.Sprintf("%q also names a model%s; `whois %s` is now ambiguous", n, alsoKnownAs(m.Name, n), n))
			}
		}
	}
	return out
}

// alsoKnownAs names the entry a colliding name actually reaches, when that
// is not the name itself. A collision with an alias is every bit as
// ambiguous to whois as a collision with a canonical name — `opus` reaching
// model `opus4.8` shadows just as hard — so the warning has to fire either
// way, and say which entry it landed on.
func alsoKnownAs(canonical, name string) string {
	if canonical == name {
		return ""
	}
	return " (" + canonical + ")"
}

func newAgentsSet(opts []Option) *cobra.Command {
	var f agentFlags
	c := &cobra.Command{
		Use:   "set <name>",
		Short: "Modify an agent's binding or nicknames",
		Long: "Modify an agent's binding or nicknames.\n\n" +
			"An agent from the embedded baseline, a shared dir, or an org overlay is\n" +
			"copied into the host-local store on first modification: the edit shadows\n" +
			"the original rather than mutating a catalog this host does not own.",
		Example:       "  bashy agent set 007 --model opus --add-alias bond",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			a, ok := cat.Agent(args[0])
			if !ok {
				return fmt.Errorf("fleet: no agent %q", args[0])
			}
			from := a.Ring

			if cmd.Flags().Changed("tool") {
				a.Tool = f.tool
			}
			if cmd.Flags().Changed("model") {
				a.Model = f.model
			}
			if cmd.Flags().Changed("display") {
				a.Display = f.display
			}
			if cmd.Flags().Changed("description") {
				a.Description = f.description
			}
			if cmd.Flags().Changed("nick") {
				a.Nick = f.nick
			}
			if cmd.Flags().Changed("ephemeral") {
				a.Ephemeral = f.ephemeral
			}
			a.Aliases = mergeAliases(a.Aliases, f.addAlias, f.rmAlias)
			if err := applyPathFlags(cmd, KindAgent, &a, f.paths); err != nil {
				return err
			}

			// The claim covers the explicit nickname too — an operator naming
			// an agent `codex` would otherwise shadow the tool of that name.
			// Derived names are absent here by construction: they are never
			// persisted, so there is nothing of ours to re-claim.
			claims := append(append([]string{}, a.Aliases...), a.Nick)
			if err := cat.claimName(KindAgent, a.Name, claims, f.force); err != nil {
				return err
			}
			if err := cat.SaveAgent(a); err != nil {
				return err
			}
			if from != ringLocal() {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: copied %s from the %s ring into the local store\n", a.Name, from)
			}
			return reportAgentSaved(cmd, cat, a)
		},
	}
	f.bind(c, true)
	return c
}

// --- tools / models add + set -------------------------------------------

func newToolsAdd(opts []Option) *cobra.Command {
	var force bool
	var hidden bool
	var name string
	var paths pathFlags
	c := &cobra.Command{
		Use:           "add (<name> --set path=value | <file>|-)",
		Short:         "Add or import a tool definition into the local store",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			var t Tool
			if looksLikePath(args[0]) && len(paths.set) == 0 && len(paths.unset) == 0 && !cmd.Flags().Changed("hidden") {
				data, err := readSource(args[0], cmd.InOrStdin())
				if err != nil {
					return err
				}
				fallback := name
				if fallback == "" {
					fallback = baseName(args[0])
				}
				t, err = ParseTool(fallback, data, nil)
				if err != nil {
					return err
				}
				if name != "" {
					t.Name = name
				}
			} else {
				t = Tool{Name: args[0], Hidden: hidden}
				if err := applyPathFlags(cmd, KindTool, &t, paths); err != nil {
					return err
				}
			}
			if err := cat.claimName(KindTool, t.Name, t.Aliases, force); err != nil {
				return err
			}
			if err := cat.SaveTool(t); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s)\n", t.Name, t.Kind)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	c.Flags().StringVar(&name, "name", "", "store under this name instead of the document's own")
	c.Flags().BoolVar(&hidden, "hidden", false, "omit this tool from default listings")
	paths.bind(c)
	return c
}

func newToolsSet(opts []Option) *cobra.Command {
	var binary, execTmpl, display string
	var addAlias, rmAlias []string
	var force bool
	var hidden bool
	var paths pathFlags
	c := &cobra.Command{
		Use:           "set <name>",
		Short:         "Modify a tool definition",
		Long:          "Modify a tool definition. Entries from a lower ring are copied into the local store first.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			t, ok := cat.Tool(args[0])
			if !ok {
				return fmt.Errorf("fleet: no tool %q", args[0])
			}
			from := t.Ring
			if cmd.Flags().Changed("binary") {
				t.CLI.Binary = binary
			}
			if cmd.Flags().Changed("exec") {
				t.CLI.Launch.Exec = execTmpl
			}
			if cmd.Flags().Changed("display") {
				t.Display = display
			}
			t.Aliases = mergeAliases(t.Aliases, addAlias, rmAlias)
			if cmd.Flags().Changed("hidden") {
				t.Hidden = hidden
			}
			if err := applyPathFlags(cmd, KindTool, &t, paths); err != nil {
				return err
			}

			if err := cat.claimName(KindTool, t.Name, t.Aliases, force); err != nil {
				return err
			}
			if err := cat.SaveTool(t); err != nil {
				return err
			}
			if from != ringLocal() {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: copied %s from the %s ring into the local store\n", t.Name, from)
			}
			if t.CLI.Launch.Exec != "" && !t.TakesModel() {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s has no %s placeholder, so agents bound to it cannot select a model\n", t.Name, ModelToken)
			}
			fmt.Fprintln(cmd.OutOrStdout(), t.Name)
			return nil
		},
	}
	c.Flags().StringVar(&binary, "binary", "", "the executable to run")
	c.Flags().StringVar(&execTmpl, "exec", "", "launch template; {prompt} and {model} are substituted")
	c.Flags().StringVar(&display, "display", "", "human-facing label")
	c.Flags().StringArrayVar(&addAlias, "add-alias", nil, "add an alias (repeatable)")
	c.Flags().StringArrayVar(&rmAlias, "rm-alias", nil, "drop an alias (repeatable)")
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	c.Flags().BoolVar(&hidden, "hidden", false, "omit this tool from default listings")
	paths.bind(c)
	return c
}

func newModelsAdd(opts []Option) *cobra.Command {
	var m Model
	var force bool
	var bandSource string
	var ids []string
	var paths pathFlags
	c := &cobra.Command{
		Use:   "add (<name> --provider P --kind K | <file>|-)",
		Short: "Add an inference backend to the local store",
		Long: "Add an inference backend to the local store.\n\n" +
			"Name a model for the exact version it is (`opus5`), not the family it\n" +
			"belongs to (`opus`). Declare --family and --version and the catalog will\n" +
			"point the bare family name at whichever version is newest — so the alias\n" +
			"follows the releases and the name in a record never changes meaning.\n\n" +
			"--band is the model's capability peg, 1 (basic) to 4 (frontier), measured\n" +
			"across providers rather than taken from the vendor's own tier ladder.",
		Example: "  bashy model add opus5 --family opus --version 5 --band 3 \\\n" +
			"      --provider anthropic --kind subscription --upstream claude-opus-5\n" +
			"  bashy model add ./deepseek.yaml",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			arg := args[0]

			if looksLikePath(arg) && m.Provider == "" && m.Kind == "" && len(paths.set) == 0 && len(paths.unset) == 0 && bandSource == "" && len(ids) == 0 {
				data, err := readSource(arg, cmd.InOrStdin())
				if err != nil {
					return err
				}
				parsed, err := ParseModel(baseName(arg), data, nil)
				if err != nil {
					return err
				}
				m = parsed
			} else {
				m.Name = arg
			}
			if bandSource != "" {
				m.BandSource = bandSource
			}
			if err := applyIDs(&m, ids); err != nil {
				return err
			}
			if err := applyPathFlags(cmd, KindModel, &m, paths); err != nil {
				return err
			}
			if err := cat.claimName(KindModel, m.Name, m.Aliases, force); err != nil {
				return err
			}
			if err := cat.SaveModel(m); err != nil {
				return err
			}
			chk := cat.VerifyModel(m.Name, Probes(nil))
			fmt.Fprintf(cmd.OutOrStdout(), "%s → %s\n", m.Name, m.Target())
			if !chk.OK {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", chk.Reason)
			}
			return nil
		},
	}
	c.Flags().StringVar(&m.Provider, "provider", "", "anthropic | openai | gemini | openai-compat | ollama")
	c.Flags().StringVar(&m.Kind, "kind", "", "subscription | api | local")
	c.Flags().StringVar(&m.UpstreamID, "upstream", "", "provider-side model id (the value passed to --model)")
	c.Flags().StringVar(&m.BaseURL, "base-url", "", "API base URL")
	c.Flags().StringVar(&m.APIKeyRef, "api-key-ref", "", "vault key name; never an inline secret")
	c.Flags().StringVar(&m.Display, "display", "", "human-facing label")
	c.Flags().StringVar(&m.Family, "family", "", "product line; the bare family name floats to its newest version")
	c.Flags().StringVar(&m.Version, "version", "", "version within the family, e.g. 4.9")
	c.Flags().IntVar(&m.Band, "band", 0, "capability peg 1-4, normalized across providers")
	c.Flags().StringVar(&bandSource, "band-source", "", "evidence for the capability band")
	c.Flags().StringArrayVar(&ids, "id", nil, "tool-specific model id as <tool>=<upstream> (repeatable)")
	c.Flags().StringArrayVar(&m.Aliases, "alias", nil, "an additional name (repeatable)")
	c.Flags().Float64Var(&m.Quality, "quality", 0, "capability prior in [0,1]; the router's quality term")
	c.Flags().Int64Var(&m.CostMicro, "cost-micro", 0, "relative per-turn cost; the router's cost term")
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	paths.bind(c)
	return c
}

func newModelsSet(opts []Option) *cobra.Command {
	var provider, kind, upstream, baseURL, keyRef, display string
	var quality float64
	var costMicro int64
	var addAlias, rmAlias []string
	var force bool
	var band int
	var bandSource string
	var ids []string
	var paths pathFlags
	c := &cobra.Command{
		Use:           "set <name>",
		Short:         "Modify an inference backend",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			m, ok := cat.Model(args[0])
			if !ok {
				return fmt.Errorf("fleet: no model %q", args[0])
			}
			from := m.Ring
			for flag, set := range map[string]func(){
				"provider":    func() { m.Provider = provider },
				"kind":        func() { m.Kind = kind },
				"upstream":    func() { m.UpstreamID = upstream },
				"base-url":    func() { m.BaseURL = baseURL },
				"api-key-ref": func() { m.APIKeyRef = keyRef },
				"display":     func() { m.Display = display },
				"quality":     func() { m.Quality = quality },
				"cost-micro":  func() { m.CostMicro = costMicro },
			} {
				if cmd.Flags().Changed(flag) {
					set()
				}
			}
			m.Aliases = mergeAliases(m.Aliases, addAlias, rmAlias)
			if cmd.Flags().Changed("band") {
				m.Band = band
			}
			if cmd.Flags().Changed("band-source") {
				m.BandSource = bandSource
			}
			if err := applyIDs(&m, ids); err != nil {
				return err
			}
			if err := applyPathFlags(cmd, KindModel, &m, paths); err != nil {
				return err
			}

			if err := cat.claimName(KindModel, m.Name, m.Aliases, force); err != nil {
				return err
			}
			if err := cat.SaveModel(m); err != nil {
				return err
			}
			if from != ringLocal() {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: copied %s from the %s ring into the local store\n", m.Name, from)
			}
			fmt.Fprintln(cmd.OutOrStdout(), m.Name)
			return nil
		},
	}
	c.Flags().StringVar(&provider, "provider", "", "anthropic | openai | gemini | openai-compat | ollama")
	c.Flags().StringVar(&kind, "kind", "", "subscription | api | local")
	c.Flags().StringVar(&upstream, "upstream", "", "provider-side model id")
	c.Flags().StringVar(&baseURL, "base-url", "", "API base URL")
	c.Flags().StringVar(&keyRef, "api-key-ref", "", "vault key name; never an inline secret")
	c.Flags().StringVar(&display, "display", "", "human-facing label")
	c.Flags().Float64Var(&quality, "quality", 0, "capability prior in [0,1]; the router's quality term")
	c.Flags().Int64Var(&costMicro, "cost-micro", 0, "relative per-turn cost; the router's cost term")
	c.Flags().IntVar(&band, "band", 0, "capability peg 1-4, normalized across providers")
	c.Flags().StringVar(&bandSource, "band-source", "", "evidence for the capability band")
	c.Flags().StringArrayVar(&ids, "id", nil, "tool-specific model id as <tool>=<upstream> (repeatable)")
	c.Flags().StringArrayVar(&addAlias, "add-alias", nil, "add an alias (repeatable)")
	c.Flags().StringArrayVar(&rmAlias, "rm-alias", nil, "drop an alias (repeatable)")
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	paths.bind(c)
	return c
}

type pathFlags struct {
	set   []string
	unset []string
}

func (f *pathFlags) bind(c *cobra.Command) {
	c.Flags().StringArrayVar(&f.set, "set", nil, "set a dotted path as <path>=<value> (repeatable)")
	c.Flags().StringArrayVar(&f.unset, "unset", nil, "clear a dotted path (repeatable)")
}

func applyPathFlags(cmd *cobra.Command, noun string, record any, flags pathFlags) error {
	if err := editPaths(noun, record, flags.set, flags.unset); err != nil {
		return reportPathError(cmd, noun, err)
	}
	return nil
}

func reportPathError(cmd *cobra.Command, noun string, err error) error {
	var unknown *unknownPathError
	if errors.As(err, &unknown) {
		if writeErr := writeSchema(cmd.ErrOrStderr(), noun, false); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func applyIDs(m *Model, ids []string) error {
	for _, assignment := range ids {
		tool, upstream, ok := strings.Cut(assignment, "=")
		if !ok || tool == "" || upstream == "" {
			return fmt.Errorf("fleet: --id needs <tool>=<upstream>, got %q", assignment)
		}
		if m.ToolIDs == nil {
			m.ToolIDs = make(map[string]string)
		}
		m.ToolIDs[tool] = upstream
	}
	return nil
}

// --- rm / edit / verify --------------------------------------------------

func newRm(noun string, opts []Option, remove func(*Catalog, string) error) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove an entry from the local store",
		Long: "Remove an entry from the local store.\n\n" +
			"Only the local ring is writable. Removing an entry that also exists in a\n" +
			"lower ring unshadows the original rather than deleting it.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			if err := remove(cat, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s %s\n", noun, args[0])
			return nil
		},
	}
}

// newEdit opens an entry in $EDITOR. An entry from a lower ring is
// materialized into the local store first, so the editor never opens a file
// the operator cannot save.
func newEdit(noun string, opts []Option, materialize func(*Catalog, string) (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:           "edit <name>",
		Short:         "Open an entry in $EDITOR",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			path, err := materialize(cat, args[0])
			if err != nil {
				return err
			}
			editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
			if editor == "" {
				fmt.Fprintln(cmd.OutOrStdout(), path)
				return fmt.Errorf("fleet: neither $VISUAL nor $EDITOR is set; the %s is at the path above", noun)
			}
			ed := exec.Command(editor, path)
			ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
			return ed.Run()
		},
	}
}

func newVerify(noun string, opts []Option, check func(*Catalog, string) Check) *cobra.Command {
	var asJSON, live bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "verify [<name>]",
		Short: "Check that an entry is usable on this host",
		Long: "Check that an entry is usable on this host. With no name, every entry is checked.\n\n" +
			"The default check is STRUCTURAL: it confirms the entry resolves and that\n" +
			"this host has what it names. It does not launch anything, so it cannot\n" +
			"tell you that a tool will reject the model an agent is bound to — and a\n" +
			"dead binding looks exactly like a live one until an agent tries to speak.\n\n" +
			"--live (agents only) actually LAUNCHES each agent on a trivial prompt and\n" +
			"reports what came back: ok, bad-model, stale-contract, or needs-auth. It\n" +
			"costs a real model call per agent, and it is the only thing that catches a\n" +
			"binding the registry believes in and the tool does not.",
		Example: "  bashy agent verify\n" +
			"  bashy agent verify --live\n" +
			"  bashy agent verify --live claude-opus5",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			if live && noun != KindAgent {
				return fmt.Errorf("fleet: --live launches an agent, so it only applies to `agents verify`")
			}
			names := args
			if len(names) == 0 {
				names = allNames(cat, noun)
			}
			var checks []Check
			for _, n := range names {
				if !live {
					checks = append(checks, check(cat, n))
					continue
				}
				chk, wired := cat.LiveProbeAgent(cmd.Context(), n, timeout)
				if !wired {
					// Never silently fall back to the weaker check: a caller that
					// asked to be sure must not be told "verified" on less evidence
					// than they asked for.
					return fmt.Errorf("fleet: --live needs a launcher, and this build has none wired")
				}
				checks = append(checks, chk)
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), checks)
			}
			bad, candidates := 0, 0
			for _, k := range checks {
				var mark string
				switch {
				case k.Skipped:
					mark = "skip"
				case k.OK:
					mark, candidates = "ok  ", candidates+1
				default:
					mark, bad, candidates = "FAIL", bad+1, candidates+1
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %-24s %s\n", mark, k.Name, k.Reason)
				// The detail is printed on FAILURE above all. It carries what the
				// tool actually said, which is the only thing that tells an operator
				// whether to re-peg a model, fix an argv, or go and log in. Showing it
				// only on success — as this did — hides it exactly when it is needed.
				if k.Detail != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "     %-24s %s\n", "", k.Detail)
				}
				if k.Warn != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "     %-24s ⚠ %s\n", "", k.Warn)
				}
			}
			if bad > 0 {
				return fmt.Errorf("fleet: %d of %d %ss are not usable here", bad, candidates, noun)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().BoolVar(&live, "live", false, "actually launch each agent on a trivial prompt (agents only; costs a model call each)")
	c.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "per-agent ceiling for --live")
	return c
}

func allNames(c *Catalog, noun string) []string {
	var out []string
	switch noun {
	case KindTool:
		tools, _ := c.Tools(false)
		for _, t := range tools {
			out = append(out, t.Name)
		}
	case KindModel:
		models, _ := c.Models()
		for _, m := range models {
			out = append(out, m.Name)
		}
	case KindAgent:
		agents, _ := c.Agents()
		for _, a := range agents {
			out = append(out, a.Name)
		}
	case KindCommand:
		cmds, _ := c.Commands()
		for _, r := range cmds {
			out = append(out, r.Name)
		}
	}
	return out
}

// --- helpers -------------------------------------------------------------

func baseName(path string) string {
	if path == "-" {
		return ""
	}
	s := path
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	for _, e := range []string{ext, ".yml"} {
		s = strings.TrimSuffix(s, e)
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// newSync builds the `sync` verb: pull one noun's org catalog into the
// overlay ring.
//
// Failing to reach the control plane is an error for `sync` (the caller asked
// to pull) but never for any other verb: the cached ring keeps answering, and
// an unpaired host never needed it.
func newSync(noun string, opts []Option) *cobra.Command {
	var cfg CloudConfig
	var asJSON bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "Pull the org catalog into the overlay ring",
		Long: "Pull the org catalog into the overlay ring.\n\n" +
			"The overlay sits above the compiled-in baseline and below the local\n" +
			"store: an org default beats what bashy shipped, and your own entry\n" +
			"beats the org. Everything works without it — pairing only enhances.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			cat := New(opts...)
			res, err := client.Sync(CloudCacheRoot(cat.Root()), noun+"s")
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d pulled into %s\n", res.Noun, res.Fetched, res.Dir)
			if res.Skipped > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: skipped %d non-cli tool kinds (function kits are not fleet tools)\n", res.Skipped)
			}
			return nil
		},
	}
	c.Flags().StringVar(&cfg.URL, "url", "", "control-plane base URL (default $BASHY_CLOUDBOX_URL)")
	c.Flags().StringVar(&cfg.Token, "token", "", "Bearer token (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}
