package kbmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file owns the Wave2 retrieval commands. They are standalone
// constructors for a later Bashy mount (same holder, separate run): nothing
// here touches the `kb memory` capture command in cmd.go, and nothing here
// touches Bashy.
//
//   - `retrieve TARGET --index INDEX` runs the shared read pipeline: scoped
//     lexical recall with scope/tags/time filters, stable refs, scores, the
//     actual mode, and source/index freshness. Semantic and hybrid return
//     explicit unsupported.
//   - `index-sync TARGET --index INDEX` runs the explicit index sync this
//     story owns: a bounded consistent source snapshot, then one atomic
//     whole-index batch. Recall never syncs on its own.

// NewRetrieveCmd is `retrieve TARGET --index INDEX [flags]`.
func NewRetrieveCmd() *cobra.Command {
	dir := ""
	return newRetrieveCmd(nil, dir)
}

// NewSyncIndexCmd is `index-sync TARGET --index INDEX [flags]`.
func NewSyncIndexCmd() *cobra.Command {
	return newSyncIndexCmd(nil, "")
}

// MCPRetrieveCommand is the typed MCP tool for retrieval. It runs the same
// command and flags, so the CLI and MCP share one parser and one envelope.
func MCPRetrieveCommand() mcp.RegisteredCommand {
	schema := RetrieveSchema()
	return mcp.RegisteredCommand{
		Name: "kb_memory_retrieve", Synopsis: "scoped memory recall with freshness (lexical, semantic, hybrid)",
		Usage: retrieveUsage, Schema: &schema, Effects: []string{"fs.read", "db.read"},
		Run: func(ctx context.Context, argv []string, stdin, dir string) (string, string, int) {
			cmd := newRetrieveCmd(nil, dir)
			cmd.SetContext(ctx)
			var out, errb bytes.Buffer
			cmd.SetArgs(argv)
			cmd.SetIn(strings.NewReader(stdin))
			cmd.SetOut(&out)
			cmd.SetErr(&errb)
			if err := cmd.Execute(); err != nil {
				code := 2
				if ee, ok := err.(*ExitError); ok {
					code = ee.Code
				}
				fmt.Fprintln(&errb, err)
				return out.String(), errb.String(), code
			}
			return out.String(), errb.String(), 0
		},
	}
}

// MCPSyncIndexCommand is the typed MCP tool for the explicit index sync.
func MCPSyncIndexCommand() mcp.RegisteredCommand {
	schema := SyncSchema()
	return mcp.RegisteredCommand{
		Name: "kb_memory_index_sync", Synopsis: "explicit memory index sync from a bounded source snapshot",
		Usage: syncUsage, Schema: &schema, Effects: []string{"fs.read", "fs.write", "db.read", "db.write"},
		Run: func(ctx context.Context, argv []string, stdin, dir string) (string, string, int) {
			cmd := newSyncIndexCmd(nil, dir)
			cmd.SetContext(ctx)
			var out, errb bytes.Buffer
			cmd.SetArgs(argv)
			cmd.SetIn(strings.NewReader(stdin))
			cmd.SetOut(&out)
			cmd.SetErr(&errb)
			if err := cmd.Execute(); err != nil {
				code := 2
				if ee, ok := err.(*ExitError); ok {
					code = ee.Code
				}
				fmt.Fprintln(&errb, err)
				return out.String(), errb.String(), code
			}
			return out.String(), errb.String(), 0
		},
	}
}

// RetrieveSchema projects the retrieve flags for MCP. Positional: TARGET.
func RetrieveSchema() tool.ArgSchema {
	s := tool.ArgSchema{Positionals: []tool.ArgParameter{{Name: "target", Required: true}}}
	newRetrieveCmd(nil, "").Flags().VisitAll(func(f *pflag.Flag) {
		s.Flags = append(s.Flags, tool.ArgFlag{Name: f.Name, Shorthand: f.Shorthand, Type: flagType(f)})
	})
	return s
}

// SyncSchema projects the index-sync flags for MCP. Positional: TARGET.
func SyncSchema() tool.ArgSchema {
	s := tool.ArgSchema{Positionals: []tool.ArgParameter{{Name: "target", Required: true}}}
	newSyncIndexCmd(nil, "").Flags().VisitAll(func(f *pflag.Flag) {
		s.Flags = append(s.Flags, tool.ArgFlag{Name: f.Name, Shorthand: f.Shorthand, Type: flagType(f)})
	})
	return s
}

func flagType(f *pflag.Flag) string {
	switch f.Value.Type() {
	case "bool":
		return "bool"
	case "int64", "int":
		return "int"
	default:
		return "string"
	}
}

const retrieveUsage = `retrieve TARGET --index INDEX [flags]`

const syncUsage = `index-sync TARGET --index INDEX [flags]`

func newRetrieveCmd(ctxFn func() context.Context, dir string) *cobra.Command {
	var (
		index, mode, text, osName, since, until, asOf, requestID, sessionID string
		vector, vectorCollection, embedModel                                string
		tags, repos                                                         []string
		limit, maxBytes                                                     int
		wait, timeout                                                       time.Duration
		fallbackLexical                                                     bool
		asJSON                                                              bool
	)
	cmd := &cobra.Command{
		Use:   "retrieve TARGET --index INDEX",
		Short: "Scoped memory recall over explicit targets (lexical, semantic, hybrid)",
		Long: `Recall reads explicit targets and writes nothing: the sqlite
memory collection (source of truth) and the bleve rank index. Lexical mode
ranks on the bleve side. Semantic mode (--vector, --embed-model) embeds the
rank text through the local model door and ranks the vector collection
through the vec category; hybrid fuses both lists with reciprocal rank
fusion. Without a vector target and model the vector modes fail with
unsupported, unless --fallback-lexical labels a lexical fallback.

  TARGET  the db locator for the memory collection, as 'bashy db' prints it
          (colons escaped): db:sqlite%3Afile%3APATH (persistent) or
          db:sqlite%3Amem%3ASESSION/NAME (in-memory, held by an explicit
          'bashy db session start' owner)
  --index the db locator for the bleve index, e.g.
          db:bleve%3Afile%3APATH or db:bleve%3Amem%3ASESSION/NAME

Filters: --tags (all must match), --repos (any overlap), --os (exact),
--since (inclusive) and --until (exclusive) on the record created stamp.
--as-of reads lifecycle visibility at a historical instant (default: now);
a forgotten, superseded, expired or not-yet-valid record is never served.
Freshness reports the source and index populations, per-hit staleness and
whether anything is stale — including newly added unindexed records.`,
		Args:              cobra.ExactArgs(1),
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(c *cobra.Command, args []string) error {
			// kb's root hook resolves one OKF store and prints its header;
			// a retrieval target is named explicitly, so suppress it as
			// capture does — and refuse an inherited store selector rather
			// than silently ignoring it, exactly like capture.
			op := Envelope{Op: RetrievalOp, Collection: Collection, MutationClass: "read"}
			if err := refuseInherited(c); err != nil {
				return finishCmd(c, fail(op, err), asJSON)
			}
			base := c.Context()
			if ctxFn != nil {
				base = ctxFn()
			}
			if base == nil {
				base = context.Background()
			}
			ctx, cancel := context.WithTimeout(base, timeout)
			defer cancel()
			q := RetrieveQuery{Target: args[0], Index: index, Session: sessionID, Dir: dir,
				Mode: mode, Text: text, Tags: tags, Repos: repos, OS: osName,
				Since: since, Until: until, AsOf: asOf, Limit: limit, MaxBytes: maxBytes,
				RequestID: requestID, Wait: wait, Vector: vector,
				VectorCollection: vectorCollection, EmbedModel: embedModel,
				FallbackLexical: fallbackLexical}
			return finishRetrieveCmd(c, Retrieve(ctx, q), asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&index, "index", "", "db locator for the bleve rank index (required; retrieval never provisions one)")
	f.StringVar(&mode, "mode", ModeLexical, "retrieval mode: lexical|semantic|hybrid (vector modes need --vector and --embed-model, or --fallback-lexical)")
	f.StringVar(&vector, "vector", "", "db locator for the sqlite vector rank target (required for semantic|hybrid; retrieval never provisions one)")
	f.StringVar(&vectorCollection, "vector-collection", "", "vec collection holding memory embeddings (default memory)")
	f.StringVar(&embedModel, "embed-model", "", "explicit local embedding model for semantic|hybrid (must match the vector collection)")
	f.BoolVar(&fallbackLexical, "fallback-lexical", false, "serve labeled lexical results instead of failing when a vector mode has no backend")
	f.StringVar(&text, "text", "", "rank text (empty matches every indexed record: a pure filter browse)")
	f.StringSliceVar(&tags, "tags", nil, "tags, all of which must match (comma-separated or repeated)")
	f.StringSliceVar(&repos, "repos", nil, "scope: repo basenames, any overlap matches")
	f.StringVar(&osName, "os", "", "scope: GOOS value, exact match")
	f.StringVar(&since, "since", "", "earliest created stamp, RFC 3339, inclusive")
	f.StringVar(&until, "until", "", "latest created stamp, RFC 3339, exclusive")
	f.StringVar(&asOf, "as-of", "", "lifecycle visibility instant, RFC 3339 (default: now)")
	f.IntVar(&limit, "limit", 0, "max hits (default: the full-text category default)")
	f.IntVar(&maxBytes, "max-bytes", 0, "max result bytes (default: the full-text category default)")
	f.StringVar(&requestID, "request-id", "", "owner-session read identity (needs --session on a sessionless path)")
	f.StringVar(&sessionID, "session", "", "explicit owner session for a persistent target")
	f.DurationVar(&wait, "wait", 5*time.Second, "bound on waiting for a lock, admission or hold")
	f.DurationVar(&timeout, "timeout", 30*time.Second, "bound on the whole operation")
	f.BoolVar(&asJSON, "json", false, "emit the bashy-kb-memory-retrieval-v1 envelope")
	return cmd
}

func newSyncIndexCmd(ctxFn func() context.Context, dir string) *cobra.Command {
	var (
		index, sessionID string
		limit, maxBytes  int
		wait, timeout    time.Duration
		asJSON           bool
	)
	cmd := &cobra.Command{
		Use:   "index-sync TARGET --index INDEX",
		Short: "Reconcile the memory full-text index with its source collection",
		Long: `SyncIndex is the explicit index maintenance this story owns (the
capture side owns no FTS target or config). It takes one bounded consistent
source snapshot — a single doc.find read transaction — beside one bounded
match-all index snapshot, and applies the adds, updates and deletes the
snapshots imply in one atomic whole-index batch (updates carry the indexed
IfRevision). A snapshot past --limit/--max-bytes, or more operations than
one batch holds, fails before anything is written. Recall never syncs: run
this verb after capture, then recall reports the revisions fresh.`,
		Args:              cobra.ExactArgs(1),
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(c *cobra.Command, args []string) error {
			// Same explicit-target rule as retrieve (and capture): an
			// inherited kb store selector is refused, never ignored.
			op := Envelope{Op: SyncOp, Collection: Collection, MutationClass: "write"}
			if err := refuseInherited(c); err != nil {
				return finishCmd(c, fail(op, err), asJSON)
			}
			base := c.Context()
			if ctxFn != nil {
				base = ctxFn()
			}
			if base == nil {
				base = context.Background()
			}
			ctx, cancel := context.WithTimeout(base, timeout)
			defer cancel()
			q := SyncQuery{Target: args[0], Index: index, Session: sessionID, Dir: dir,
				Limit: limit, MaxBytes: maxBytes, Wait: wait}
			return finishSyncCmd(c, SyncIndex(ctx, q), asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&index, "index", "", "db locator for the bleve rank index (required; never provisioned)")
	f.IntVar(&limit, "limit", 0, "max source records to reconcile (default: 10000, the atomic batch bound)")
	f.IntVar(&maxBytes, "max-bytes", 0, "max snapshot bytes to reconcile (default: 8MiB)")
	f.StringVar(&sessionID, "session", "", "explicit owner session for a persistent target")
	f.DurationVar(&wait, "wait", 5*time.Second, "bound on waiting for a lock, admission or hold")
	f.DurationVar(&timeout, "timeout", 30*time.Second, "bound on the whole operation")
	f.BoolVar(&asJSON, "json", false, "emit the bashy-kb-memory-retrieval-v1 envelope")
	return cmd
}

// refuseInherited applies the same explicit-target rule as capture: a
// memory target is named explicitly, so a set inherited kb store selector
// (--dir/--ring/--repo/--user/--base-dir) is refused rather than silently
// ignored. Production mounts these commands under `kb memory`, where the
// parent flags apply.
func refuseInherited(c *cobra.Command) error {
	for _, name := range inheritedStoreFlags {
		if f := c.InheritedFlags().Lookup(name); f != nil && f.Changed {
			return invalid("--" + name + " selects a kb store; a memory TARGET is named explicitly")
		}
	}
	return nil
}

// finishRetrieveCmd writes a retrieval envelope: the JSON envelope with
// --json, otherwise one human line per hit plus a freshness line. It reuses
// the shared ExitError contract; a failed write is a failure.
func finishRetrieveCmd(c *cobra.Command, env Envelope, asJSON bool) error {
	if asJSON {
		return finishCmd(c, env, true)
	}
	if env.Error != nil {
		return finishCmd(c, env, false)
	}
	var res RetrieveResult
	b, _ := json.Marshal(env.Result)
	if err := json.Unmarshal(b, &res); err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("kb %s: result: %v", env.Op, err)}
	}
	for _, h := range res.Hits {
		stale := ""
		if h.Stale {
			stale = " stale"
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "%s score=%.4g rev=%d mode=%s%s\n%s\n",
			h.Ref, h.Score, h.Revision, h.Mode, stale, h.Record.Body); err != nil {
			return &ExitError{Code: 1, Msg: fmt.Sprintf("kb %s: writing result: %v", env.Op, err)}
		}
	}
	fresh := "fresh"
	if res.Freshness.Stale {
		fresh = "stale"
	}
	mode := res.Mode
	if res.Fallback {
		mode += "+fallback"
	}
	if _, err := fmt.Fprintf(c.OutOrStdout(), "%d hits (%s, %s): %d source, %d indexed, %d examined, %d dropped src=%s idx=%s\n",
		len(res.Hits), mode, fresh, res.Freshness.SourceDocuments,
		res.Freshness.IndexDocuments, res.Freshness.Examined, res.Freshness.DroppedStale,
		shortFP(res.Freshness.SourceRevision), shortFP(res.Freshness.IndexRevision)); err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("kb %s: writing result: %v", env.Op, err)}
	}
	return nil
}

// finishSyncCmd writes a sync envelope: the JSON envelope with --json,
// otherwise one human line with the applied counts.
func finishSyncCmd(c *cobra.Command, env Envelope, asJSON bool) error {
	if asJSON {
		return finishCmd(c, env, true)
	}
	if env.Error != nil {
		return finishCmd(c, env, false)
	}
	var res SyncResult
	b, _ := json.Marshal(env.Result)
	if err := json.Unmarshal(b, &res); err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("kb %s: result: %v", env.Op, err)}
	}
	_, err := fmt.Fprintf(c.OutOrStdout(), "index-sync: %d source, +%d ~%d -%d (%d indexed)\n",
		res.SourceDocuments, res.Added, res.Updated, res.Deleted, res.IndexDocuments)
	if err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("kb %s: writing result: %v", env.Op, err)}
	}
	return nil
}
