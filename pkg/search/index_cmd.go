package search

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/qiangli/yoke/pkg/indexer"
	"github.com/qiangli/yoke/pkg/kbmemory"
	"github.com/spf13/cobra"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func indexHome() string {
	if h := os.Getenv("BASHY_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".bashy")
}

// newIndexCmd controls the explicitly initialized managed content index.
// It is distinct from the existing --files filename snapshot.
func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "index", Short: "Manage the local document index"}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "emit structured index state")
	action := func(name, desc string, nargs int, run func(context.Context, *indexer.Manager, []string) error) {
		c := &cobra.Command{Use: name, Short: desc, Args: cobra.ExactArgs(nargs)}
		c.RunE = func(c *cobra.Command, args []string) error {
			m := indexer.New(indexHome())
			if err := run(c.Context(), m, args); err != nil {
				return err
			}
			st, err := m.Status()
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(c.OutOrStdout()).Encode(st)
			}
			b, _ := json.MarshalIndent(st, "", "  ")
			fmt.Fprintln(c.OutOrStdout(), string(b))
			return nil
		}
		cmd.AddCommand(c)
	}
	action("init", "Explicitly create this home's document index", 0, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.Init() })
	action("add DIR", "Pin a directory for indexing", 1, func(ctx context.Context, m *indexer.Manager, a []string) error {
		if err := m.Add(a[0]); err != nil {
			return err
		}
		return m.Sync(ctx)
	})
	action("forget DIR", "Remove a root and its derived entries", 1, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.Forget(a[0]) })
	action("status", "Show roots, inclusion reasons, freshness and errors", 0, func(context.Context, *indexer.Manager, []string) error { return nil })
	action("pause", "Pause ingestion without deleting results", 0, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.SetPaused(true) })
	action("resume", "Resume ingestion", 0, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.SetPaused(false) })
	action("off", "Disable background indexing globally", 0, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.SetEnabled(false) })
	action("on", "Enable an explicitly initialized index", 0, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.SetEnabled(true) })
	action("model MODEL", "Explicitly enable chunk embeddings with a local model", 1, func(ctx context.Context, m *indexer.Manager, a []string) error {
		if err := m.EnableEmbeddings(ctx, a[0]); err != nil {
			return err
		}
		return m.Sync(ctx)
	})
	action("sync", "Refresh eligible roots once", 0, func(ctx context.Context, m *indexer.Manager, a []string) error { return m.Sync(ctx) })
	cmd.AddCommand(&cobra.Command{Use: "source URI", Short: "Read an eligible source's indexed, scrubbed text", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, a []string) error {
		r, e := indexer.New(indexHome()).Source(c.Context(), a[0])
		if e != nil {
			return e
		}
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(r)
		}
		fmt.Fprintln(c.OutOrStdout(), r.Body)
		return nil
	}})
	var cfg indexer.Config
	configure := &cobra.Command{Use: "config", Short: "Set promotion, decay and resource limits", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		m := indexer.New(indexHome())
		if err := m.Configure(cfg); err != nil {
			return err
		}
		st, err := m.Status()
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(st)
	}}
	f := configure.Flags()
	f.IntVar(&cfg.PromotionDays, "promotion-days", 0, "distinct days required (default 3)")
	f.IntVar(&cfg.WindowDays, "window-days", 0, "promotion window (default 14)")
	f.IntVar(&cfg.UnwatchDays, "unwatch-days", 0, "idle automatic root unwatch age (default 30)")
	f.IntVar(&cfg.PruneDays, "prune-days", 0, "idle automatic root prune age (default 90)")
	f.IntVar(&cfg.MaxRoots, "max-roots", 0, "root cap (default 64)")
	f.IntVar(&cfg.MaxFiles, "max-files", 0, "corpus file cap (default 1000)")
	f.Int64Var(&cfg.MaxFileBytes, "max-file-bytes", 0, "source byte cap (default 8MiB)")
	cmd.AddCommand(configure)
	var port int
	serve := &cobra.Command{Use: "serve", Short: "Run the index worker (normally supervised)", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"ok":true}`)
		}), ReadHeaderTimeout: 2 * time.Second}
		defer srv.Close()
		go func() { _ = srv.Serve(listener) }()
		return indexer.New(indexHome()).Run(ctx)
	}}
	serve.Flags().IntVar(&port, "port", 24557, "loopback health port")
	cmd.AddCommand(serve)
	return cmd
}

func runIndexed(c *cobra.Command, text, mode string, max int, asJSON bool) error {
	ctx, cancel := context.WithTimeout(c.Context(), 60*time.Second)
	defer cancel()
	env := indexer.New(indexHome()).Query(ctx, text, mode, max)
	if asJSON {
		if err := json.NewEncoder(c.OutOrStdout()).Encode(env); err != nil {
			return err
		}
	}
	if !env.OK {
		return fmt.Errorf("indexed search: %s: %s", env.Error.Code, env.Error.Message)
	}
	if !asJSON {
		r := env.Result.(kbmemory.RetrieveResult)
		for _, h := range r.Hits {
			fmt.Fprintf(c.OutOrStdout(), "%s — %s\n", h.Record.Source.Ref, truncate(h.Record.Body, 200))
		}
		fmt.Fprintf(c.ErrOrStderr(), "(%d indexed results · %s; lexical fallback=%t)\n", len(r.Hits), r.Mode, r.Fallback)
	}
	return nil
}
