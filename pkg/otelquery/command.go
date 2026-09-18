package otelquery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func NewCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "otel",
		Short:         "Query OTEL telemetry with bounded agent-readable summaries",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	AddCommands(root)
	return root
}

func AddCommands(root *cobra.Command) {
	root.SilenceUsage = true
	root.SilenceErrors = true
	opts := Options{BaseURL: "", Since: time.Hour}
	root.PersistentFlags().StringVar(&opts.BaseURL, "url", "", "OTEL query proxy URL (default BASHY_OTEL_QUERY_URL or http://127.0.0.1:31415)")
	root.PersistentFlags().BoolVar(&opts.JSON, "json", false, "emit JSON schema "+SchemaVersion)
	query := &cobra.Command{
		Use:   "query",
		Short: "Agent-facing OTEL query verbs",
	}
	root.AddCommand(query)
	addQuestionCommands(query, &opts)
	addQuestionCommands(root, &opts)
}

func addQuestionCommands(parent *cobra.Command, opts *Options) {
	parent.AddCommand(sinceCmd("guessed", "Show decisions that ran on guessed or estimated values", opts, func(ctx context.Context) (Envelope, error) {
		return NewClient(opts.BaseURL).Guessed(ctx, *opts)
	}))
	parent.AddCommand(sinceCmd("bounds", "Show limits that actually bound execution", opts, func(ctx context.Context) (Envelope, error) {
		return NewClient(opts.BaseURL).Bounds(ctx, *opts)
	}))
	parent.AddCommand(sinceCmd("failed", "Show failed exec spans grouped by command and cwd", opts, func(ctx context.Context) (Envelope, error) {
		return NewClient(opts.BaseURL).Failed(ctx, *opts)
	}))
	cost := &cobra.Command{
		Use:   "cost",
		Short: "Show LLM token and cost totals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := NewClient(opts.BaseURL).Cost(cmd.Context(), *opts)
			return render(cmd.OutOrStdout(), cmd.ErrOrStderr(), env, err, opts.JSON)
		},
	}
	cost.Flags().BoolVar(&opts.Suspect, "suspect", false, "show only costs with pricing.known=false")
	parent.AddCommand(cost)
	parent.AddCommand(&cobra.Command{
		Use:   "why-slow TRACE_ID",
		Short: "Explain where one trace spent wall-clock time",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := NewClient(opts.BaseURL).WhySlow(cmd.Context(), args[0], *opts)
			return render(cmd.OutOrStdout(), cmd.ErrOrStderr(), env, err, opts.JSON)
		},
	})
}

func sinceCmd(use, short string, opts *Options, run func(context.Context) (Envelope, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := run(cmd.Context())
			return render(cmd.OutOrStdout(), cmd.ErrOrStderr(), env, err, opts.JSON)
		},
	}
	cmd.Flags().DurationVar(&opts.Since, "since", time.Hour, "lookback window")
	return cmd
}

func render(stdout, stderr io.Writer, env Envelope, err error, asJSON bool) error {
	if err != nil {
		fmt.Fprintf(stderr, "otel query: %v\n", err)
		return printedError{err: err}
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	}
	fmt.Fprintln(stdout, env.Summary)
	for _, item := range env.Items {
		printItem(stdout, item)
	}
	if env.Trace != nil {
		fmt.Fprintf(stdout, "trace %s: %.0fms total; %.0fms bound waits; %.0fms work\n",
			env.Trace.TraceID, env.Trace.DurationMS, env.Trace.BoundWaitMS, env.Trace.WorkMS)
	}
	return nil
}

type printedError struct {
	err error
}

func (e printedError) Error() string { return e.err.Error() }
func (e printedError) Unwrap() error { return e.err }

func ErrorAlreadyPrinted(err error) bool {
	_, ok := err.(printedError)
	return ok
}

func printItem(w io.Writer, item SummaryItem) {
	switch {
	case item.Command != "":
		fmt.Fprintf(w, "%dx %s exit %s in %s", item.Count, item.Command, item.ExitCode, item.CWD)
	case item.Kind != "":
		fmt.Fprintf(w, "%dx %s limit=%s actual=%s", item.Count, item.Kind, item.Limit, item.Actual)
	case item.Source != "":
		fmt.Fprintf(w, "%dx %s %s amount=%s", item.Count, item.Source, item.ValueName, item.Amount)
	case item.CostUSD != 0 || item.Tokens != 0:
		fmt.Fprintf(w, "%s cost=$%.4f tokens=%.0f pricing_known=%s", item.Model, item.CostUSD, item.Tokens, item.PricingKnown)
	default:
		fmt.Fprintf(w, "%dx %s", item.Count, item.Key)
	}
	if item.TraceID != "" {
		fmt.Fprintf(w, " trace=%s", item.TraceID)
	}
	if item.DurationMS != 0 {
		fmt.Fprintf(w, " duration=%.0fms", item.DurationMS)
	}
	fmt.Fprintln(w)
}

func Execute(args []string) int {
	cmd := NewCommand()
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		return 1
	}
	return 0
}

func Main() {
	os.Exit(Execute(os.Args[1:]))
}
