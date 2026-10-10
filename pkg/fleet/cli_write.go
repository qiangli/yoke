package fleet

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// --- agents add / set --------------------------------------------------

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

func changedOverlayPaths(cmd *cobra.Command, paths pathFlags, flags map[string]string) []string {
	var out []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if path := flags[f.Name]; path != "" {
			out = append(out, path)
		}
	})
	for _, s := range paths.set {
		if p, _, ok := strings.Cut(s, "="); ok {
			out = append(out, p)
		}
	}
	out = append(out, paths.unset...)
	return out
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
	k, ok := kindByName(noun)
	if !ok || k.Names == nil {
		return nil
	}
	return k.Names(c)
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
