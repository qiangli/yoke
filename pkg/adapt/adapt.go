// Package adapt rewrites only declared common options for foreign argv.
// It never provisions executables, interprets shell text or captures output.
package adapt

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Request is the accepted common-option interface; only explicitly requested
// options are present. Listing is a provider API, not a new common CLI flag.
type Request struct {
	Options map[string]string
	Explain bool
	Listing bool
}
type Plan struct {
	Argv    []string
	Adapter fleet.Adapter
	Lens    string
}

// PrepareLens is S3's preflight seam. It must validate row availability and
// bounds before returning a streaming writer. Close flushes the conversion.
// A nil seam refuses lens mappings before the foreign command starts.
type PrepareLens func(context.Context, string, io.Writer) (io.WriteCloser, error)

func Build(rows []fleet.Adapter, argv []string, req Request) (Plan, error) {
	if len(argv) == 0 {
		return Plan{}, fmt.Errorf("missing command after --")
	}
	var selected *fleet.Adapter
	ambiguous := false
	for i := range rows {
		a := &rows[i]
		if len(argv) < len(a.Match) || !slices.Equal(argv[:len(a.Match)], a.Match) {
			continue
		}
		if selected == nil || len(a.Match) > len(selected.Match) {
			selected = a
			ambiguous = false
		} else if len(a.Match) == len(selected.Match) {
			ambiguous = true
		}
	}
	if selected == nil {
		return Plan{}, fmt.Errorf("no adapter matches command")
	}
	if ambiguous {
		return Plan{}, fmt.Errorf("ambiguous adapter prefix")
	}
	if _, j := req.Options["json"]; j {
		if _, n := req.Options["null"]; n {
			return Plan{}, fmt.Errorf("--json and --null cannot be combined")
		}
	}
	a := *selected
	if err := a.Validate(); err != nil {
		return Plan{}, err
	}
	p := Plan{Argv: append([]string(nil), argv...), Adapter: a}
	mappings := map[string]fleet.AdapterMapping{"json": a.JSON, "fields": a.Fields, "null": a.Null, "dry-run": a.DryRun, "no-input": a.NoInput, "depth": a.Depth, "since": a.Since}
	// Stable order makes combined mappings reproducible. No cursor interpretation
	// belongs here: S19 must supply validation/binding before since can execute.
	order := []string{"json", "fields", "null", "dry-run", "no-input", "depth", "since"}
	for key := range req.Options {
		if _, ok := mappings[key]; !ok {
			return Plan{}, fmt.Errorf("unsupported common option")
		}
	}
	for _, key := range order {
		value, ok := req.Options[key]
		if !ok {
			continue
		}
		m := mappings[key]
		if key == "since" {
			return Plan{}, fmt.Errorf("--since requires the shared S19 cursor binding seam")
		}
		if m == nil {
			return Plan{}, fmt.Errorf("adapter %s does not support --%s", a.Name, key)
		}
		if key == "fields" {
			if _, ok := req.Options["json"]; !ok {
				return Plan{}, fmt.Errorf("--fields requires --json")
			}
		}
		if key == "json" && len(m) == 1 && strings.HasPrefix(m[0], "lens: cmd:") {
			p.Lens = strings.TrimPrefix(m[0], "lens: ")
			continue
		}
		if key == "dry-run" && a.PreviewLevel != "local-preview" && a.PreviewLevel != "provider-validated-preview" {
			return Plan{}, fmt.Errorf("adapter preview unsupported")
		}
		// Appended switches after a target's -- would be operands. Refuse instead
		// of silently running the original operation (especially a dry-run).
		if len(m) > 0 && slices.Contains(argv[len(a.Match):], "--") {
			return Plan{}, fmt.Errorf("cannot append mapping after target option terminator")
		}
		for _, s := range m {
			if strings.Contains(s, "{args}") {
				return Plan{}, fmt.Errorf("{args} is supported only for listing templates")
			}
			p.Argv = append(p.Argv, strings.ReplaceAll(s, "{1}", value))
		}
	}
	if req.Listing {
		if len(req.Options) > 0 || a.Listing == nil {
			return Plan{}, fmt.Errorf("listing mapping unavailable or combined with common options")
		}
		tail := argv[len(a.Match):]
		// Listing accepts a single archive/container operand, never mutation flags.
		if len(tail) != 1 || strings.HasPrefix(tail[0], "-") {
			return Plan{}, fmt.Errorf("listing requires one non-option operand")
		}
		p.Argv = append([]string(nil), argv[:len(a.Match)]...)
		used := false
		for _, s := range a.Listing {
			if s == "{args}" {
				if used {
					return Plan{}, fmt.Errorf("duplicate {args}")
				}
				used = true
				p.Argv = append(p.Argv, tail...)
			} else {
				p.Argv = append(p.Argv, s)
			}
		}
		if !used {
			p.Argv = append(p.Argv, tail...)
		}
	}
	return p, nil
}

// Run preserves the child's status, stdin, stderr and native output bytes.
// Diagnostic failures after execution never replace a foreign exit status.
func Run(ctx context.Context, p Plan, in io.Reader, out, errOut io.Writer, lens PrepareLens) int {
	if len(p.Argv) == 0 {
		fmt.Fprintln(errOut, "adapt: missing command")
		return 125
	}
	var converted io.WriteCloser
	if p.Lens != "" {
		if lens == nil {
			fmt.Fprintln(errOut, "adapt: declared lens row is not integrated")
			return 125
		}
		var err error
		converted, err = lens(ctx, p.Lens, out)
		if err != nil || converted == nil {
			fmt.Fprintln(errOut, "adapt: lens preflight failed")
			return 125
		}
		out = converted
	}
	cmd := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	err := cmd.Run()
	if converted != nil {
		if e := converted.Close(); e != nil {
			fmt.Fprintln(errOut, "adapt: lens conversion failed after execution")
		}
	}
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		if code := e.ExitCode(); code >= 0 {
			return code
		}
		return signalStatus(e)
	}
	if cmd.ProcessState != nil {
		// os/exec can return a stream-copy error even after a successful child.
		// Once executed, 125 must not replace that child's actual status.
		fmt.Fprintln(errOut, "adapt: output forwarding failed after execution")
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
		return signalStatus(&exec.ExitError{ProcessState: cmd.ProcessState})
	}
	fmt.Fprintln(errOut, "adapt: could not start target executable")
	return 125
}

type statusError int

func (e statusError) Error() string { return "adapt invocation failed" }
func (e statusError) ExitCode() int { return int(e) }
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(interface{ ExitCode() int }); ok {
		return e.ExitCode()
	}
	return 125
}

// NewCmd parses only the wrapper prefix. The required -- boundary protects every
// byte of target argv, including underscores, empty strings and option values.
func NewCmd(lens PrepareLens, opts ...fleet.Option) *cobra.Command {
	c := &cobra.Command{Use: "adapt [COMMON-OPTIONS] -- CMD [ARGS...]", Short: "Apply declared native options to a foreign command", DisableFlagParsing: true, SilenceErrors: true, SilenceUsage: true}
	bindFlags(c.Flags())
	c.RunE = func(c *cobra.Command, args []string) error {
		fail := func(message string) error { fmt.Fprintln(c.ErrOrStderr(), "adapt:", message); return statusError(125) }
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintln(c.OutOrStdout(), c.Use+"\nOptions: --json --fields --null --dry-run --no-input --explain --depth --since\nCatalog: bashy adapter list|show|add|set|rm|edit|schema")
			return nil
		}
		sep := slices.Index(args, "--")
		if sep < 0 || sep == len(args)-1 {
			return fail("expected COMMON-OPTIONS -- CMD")
		}
		fs := pflag.NewFlagSet("adapt", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.SetNormalizeFunc(func(_ *pflag.FlagSet, s string) pflag.NormalizedName {
			return pflag.NormalizedName(strings.ReplaceAll(s, "_", "-"))
		})
		bindFlags(fs)
		if e := fs.Parse(args[:sep]); e != nil || fs.NArg() != 0 {
			return fail("invalid common options")
		}
		req := Request{Options: map[string]string{}}
		fs.Visit(func(f *pflag.Flag) {
			if f.Name == "explain" {
				req.Explain = f.Value.String() == "true"
			} else if f.Value.Type() != "bool" || f.Value.String() == "true" {
				req.Options[f.Name] = f.Value.String()
			}
		})
		if d, _ := fs.GetInt("depth"); d < 0 {
			return fail("depth must be nonnegative")
		}
		rows, errs := fleet.New(opts...).Adapters()
		if len(errs) > 0 {
			return fail("adapter catalog is invalid; inspect adapter records")
		}
		p, e := Build(rows, args[sep+1:], req)
		if e != nil {
			return fail(e.Error())
		}
		if req.Explain {
			// No user argv or option values enter diagnostics. Names/effects come from
			// declarations; raw foreign output is never captured or represented as safe.
			fmt.Fprintf(c.OutOrStdout(), "adapter: %s\npreview_level: %s\neffects: %s\narguments: omitted (may contain credentials); no execution\n", p.Adapter.Name, p.Adapter.PreviewLevel, strings.Join(p.Adapter.Effects, ", "))
			return nil
		}
		code := Run(c.Context(), p, c.InOrStdin(), c.OutOrStdout(), c.ErrOrStderr(), lens)
		if code != 0 {
			return statusError(code)
		}
		return nil
	}
	return c
}

// S2 can replace this narrow binder without changing Request or target parsing.
func bindFlags(fs *pflag.FlagSet) {
	for _, n := range []string{"json", "null", "dry-run", "no-input", "explain"} {
		fs.Bool(n, false, "request the declared common option")
	}
	fs.String("fields", "", "native JSON field projection")
	fs.Int("depth", 0, "native tree walk depth")
	fs.String("since", "", "shared cursor envelope (requires S19 binding)")
}
