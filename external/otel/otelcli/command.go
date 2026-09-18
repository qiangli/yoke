package otelcli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/otel/stack"
)

type Options struct {
	DataDir       string
	ProxyPort     int
	ProxyBindAddr string
	OTLPGRPCPort  int
	OTLPHTTPPort  int
}

func NewCommand() *cobra.Command {
	var opts Options
	cmd := &cobra.Command{
		Use:   "otel",
		Short: "Run the embedded OTEL stack",
	}
	cmd.PersistentFlags().StringVar(&opts.DataDir, "data-dir", defaultDataDir(), "storage directory")
	cmd.PersistentFlags().IntVar(&opts.ProxyPort, "port", stack.DefaultProxyPort, "public proxy/UI port")
	cmd.PersistentFlags().StringVar(&opts.ProxyBindAddr, "bind", "127.0.0.1", "proxy bind address")
	cmd.PersistentFlags().IntVar(&opts.OTLPGRPCPort, "otlp-grpc-port", 4317, "OTLP gRPC ingress port; negative allocates ephemerally")
	cmd.PersistentFlags().IntVar(&opts.OTLPHTTPPort, "otlp-http-port", 4318, "OTLP HTTP ingress port; negative allocates ephemerally")

	cmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Start collector, traces, metrics, logs, dashboards, and alerts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.Context(), opts)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "import [spool-file]",
		Short: "Import spans spooled while nothing was running",
		Long: "Absorb a jsonline spool written by the file sink\n" +
			"(OTEL_TRACES_EXPORTER=file) into a running stack, so spans captured\n" +
			"with no collector up become queryable. Defaults to the standard\n" +
			"spool path; $BASHY_OTEL_SPOOL overrides. The spool is truncated only\n" +
			"after the store accepts it, so a failure loses nothing and can be\n" +
			"retried.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := SpoolPath()
			if len(args) == 1 {
				path = args[0]
			}
			base := fmt.Sprintf("http://127.0.0.1:%d", opts.ProxyPort)
			n, err := importSpool(base, path)
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no spooled spans to import")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d spooled records from %s\n", n, path)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "import-ycode [instances-dir]",
		Short: "Import ycode's per-session trace archive",
		Long: "Convert ycode's stdouttrace session files\n" +
			"(~/.agents/ycode/otel/instances/<id>/traces/) into the store so an\n" +
			"existing history becomes queryable. ycode has always written these;\n" +
			"nothing ever read them.\n\n" +
			"Files are NOT deleted — a session archive is a record, not a buffer.\n" +
			"Progress is tracked with a .imported marker, so re-running only picks\n" +
			"up what is new or changed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := YcodeInstancesDir()
			if len(args) == 1 {
				dir = args[0]
			}
			if dir == "" {
				return fmt.Errorf("cannot locate ycode instances dir; pass it explicitly")
			}
			base := fmt.Sprintf("http://127.0.0.1:%d", opts.ProxyPort)
			logf := func(f string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), f+"\n", a...) }
			recs, files, err := importYcodeDir(base, dir, logf)
			if err != nil {
				return err
			}
			if recs == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no new ycode spans to import")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d spans from %d ycode trace files\n", recs, files)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "ui",
		Short: "Print the observability web UIs (traces / logs / metrics)",
		Long: "Print the human-facing Victoria vmui URLs served by a running `otel serve`.\n" +
			"These are the RICH explorer views — trace waterfalls, log search, metric\n" +
			"graphs — as opposed to the agent-facing `otel failed/guessed/bounds` summaries.\n" +
			"Filter by service.name + trace_id in each UI to follow one session end to end.",
		RunE: func(cmd *cobra.Command, args []string) error {
			base := fmt.Sprintf("http://127.0.0.1:%d", opts.ProxyPort)
			fmt.Fprintf(cmd.OutOrStdout(), "traces   %s/traces/select/vmui/\n", base)
			fmt.Fprintf(cmd.OutOrStdout(), "logs     %s/logs/select/vmui/\n", base)
			fmt.Fprintf(cmd.OutOrStdout(), "metrics  %s/metrics/vmui/\n", base)
			return nil
		},
	})
	return cmd
}

func runServe(ctx context.Context, opts Options) error {
	cfg := &stack.Config{
		ProxyPort:     opts.ProxyPort,
		ProxyBindAddr: opts.ProxyBindAddr,
		OTLPGRPCPort:  opts.OTLPGRPCPort,
		OTLPHTTPPort:  opts.OTLPHTTPPort,
	}
	svc, err := stack.NewService(cfg, opts.DataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := svc.Manager.Start(ctx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "OTEL at http://%s/\n", svc.HTTPAddr)
	fmt.Fprintf(os.Stdout, "OTLP gRPC at %s\n", svc.CollectorAddr)

	// Absorb anything the file sink captured while no stack was running.
	// Without this the spool is written and never read — the data would be
	// collected and still invisible, which is the same silent gap the sink
	// exists to close, just moved one step later. Best-effort: a stack that
	// refuses to start because an old spool is malformed would be a worse
	// failure than a spool that waits for the next attempt.
	if n, ierr := importSpool(fmt.Sprintf("http://%s", svc.HTTPAddr), SpoolPath()); ierr != nil {
		fmt.Fprintf(os.Stderr, "otel: spooled spans not imported (left in place): %v\n", ierr)
	} else if n > 0 {
		fmt.Fprintf(os.Stdout, "imported %d spooled records\n", n)
	}

	<-ctx.Done()
	return svc.Manager.Stop(context.Background())
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "otel")
	}
	return filepath.Join(home, ".agents", "otel")
}
