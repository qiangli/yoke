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

const maintainUsage = `maintain candidates|supersede|expire|delete|export|import|backup|restore TARGET [ID] [flags]`

// NewMaintainCmd is `maintain VERB TARGET [ID] [flags]`. It is not mounted by
// this package: bashy's dispatch mounts it beside retrieve and index-sync.
func NewMaintainCmd() *cobra.Command { return newMaintainCmd(nil, "") }

// MCPMaintainCommand is the typed MCP tool for maintenance. It runs the same
// command and flags, so the CLI and MCP share one parser and one envelope.
func MCPMaintainCommand() mcp.RegisteredCommand {
	schema := MaintainSchema()
	return mcp.RegisteredCommand{
		Name: "kb_memory_maintain", Synopsis: "explicit memory maintenance: candidates, supersede, expire, delete, export, import, backup, restore",
		Usage: maintainUsage, Schema: &schema, Effects: []string{"fs.read", "fs.write", "db.read", "db.write"},
		Run: func(ctx context.Context, argv []string, stdin, dir string) (string, string, int) {
			cmd := newMaintainCmd(nil, dir)
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

// MaintainSchema projects the maintain flags for MCP. Positionals: VERB TARGET [ID].
func MaintainSchema() tool.ArgSchema {
	s := tool.ArgSchema{Positionals: []tool.ArgParameter{{Name: "verb", Required: true}, {Name: "target", Required: true}, {Name: "id"}}}
	newMaintainCmd(nil, "").Flags().VisitAll(func(f *pflag.Flag) {
		s.Flags = append(s.Flags, tool.ArgFlag{Name: f.Name, Shorthand: f.Shorthand, Type: flagType(f)})
	})
	return s
}

func newMaintainCmd(ctxFn func() context.Context, dir string) *cobra.Command {
	var (
		index, asOf, staleBefore, replacement, out, in, onConflict, requestID, sessionID string
		ifRevision, replacementRevision                                                  int64
		replace, yes, asJSON                                                             bool
		wait, timeout                                                                    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "maintain candidates|supersede|expire|delete|export|import|backup|restore TARGET [ID]",
		Short: "Explicit memory maintenance over a named target (both storage modes)",
		Long: `Maintenance never acts on its own: no autonomous forgetting, no model
consolidation. candidates is a read-only report; every change names one
record and its revision.

  candidates TARGET [--as-of T] [--stale-before T]
                       list expired records, still-valid candidate records
                       last updated before --stale-before, and duplicates
                       (same type and whitespace-normalized body; the oldest
                       is kept, the rest name it in "of")
  supersede TARGET ID --if-revision N --replacement NEW --replacement-revision M
                       the capture verb (kb lifecycle interval, one batch)
  expire TARGET ID --if-revision N
                       close a valid record's interval now; kept for audit
  delete TARGET ID --if-revision N --yes
                       permanently remove the record (forget hides it instead)
  export TARGET --out DIR      dbdoc JSONL export; IDs and documents verbatim
  import TARGET --in DIR [--on-conflict error|skip|replace] [--replace --yes]
  backup TARGET --out FILE     native snapshot (exclusive destination)
  restore TARGET --in FILE --yes
                       replace the target from a snapshot

--index INDEX (supersede, expire, delete, import, restore) runs the explicit
index-sync after the change commits. If that sync fails the change still
committed: the result carries index_error and the recovery (rerun
index-sync), and recall keeps dropping hits the collection no longer holds.`,
		Args:              cobra.RangeArgs(2, 3),
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(c *cobra.Command, args []string) error {
			op := Envelope{Op: "memory.maintain." + args[0], Collection: Collection, MutationClass: maintainClass(args[0])}
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
			req := MaintainRequest{Verb: args[0], Target: args[1], Session: sessionID, Dir: dir, Index: index,
				AsOf: asOf, StaleBefore: staleBefore, IfRevision: ifRevision, Replacement: replacement,
				ReplacementRevision: replacementRevision, Out: out, In: in, OnConflict: onConflict,
				Replace: replace, Yes: yes, RequestID: requestID, Wait: wait}
			if len(args) == 3 {
				req.ID = args[2]
			}
			return finishMaintainCmd(c, Maintain(ctx, req), asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&index, "index", "", "db locator for the bleve index to sync after a committed change")
	f.StringVar(&asOf, "as-of", "", "candidates: lifecycle instant, RFC 3339 (default: now)")
	f.StringVar(&staleBefore, "stale-before", "", "candidates: valid candidate records last updated before this RFC 3339 instant are stale")
	f.Int64Var(&ifRevision, "if-revision", 0, "supersede/expire/delete: the revision the change is based on (required)")
	f.StringVar(&replacement, "replacement", "", "supersede: the existing record that replaces ID")
	f.Int64Var(&replacementRevision, "replacement-revision", 0, "supersede: the replacement's revision (required)")
	f.StringVar(&out, "out", "", "export: new directory; backup: new file")
	f.StringVar(&in, "in", "", "import: export directory; restore: backup file")
	f.StringVar(&onConflict, "on-conflict", "", "import: error|skip|replace for same-ID records (default error)")
	f.BoolVar(&replace, "replace", false, "import: empty the collection first (needs --yes)")
	f.BoolVar(&yes, "yes", false, "confirm delete, import --replace and restore")
	f.StringVar(&requestID, "request-id", "", "owner-session mutation dedup key")
	f.StringVar(&sessionID, "session", "", "explicit owner session for a persistent target")
	f.DurationVar(&wait, "wait", 5*time.Second, "bound on waiting for a lock, admission or hold")
	f.DurationVar(&timeout, "timeout", 30*time.Second, "bound on the whole operation")
	f.BoolVar(&asJSON, "json", false, "emit the bashy-kb-memory-maintenance-v1 envelope")
	return cmd
}

// finishMaintainCmd writes a maintenance envelope: the JSON envelope with
// --json, otherwise one human line (a line per candidate for candidates).
func finishMaintainCmd(c *cobra.Command, env Envelope, asJSON bool) error {
	if asJSON || env.Error != nil {
		return finishCmd(c, env, asJSON)
	}
	var lines []string
	if env.Op == "memory.maintain.candidates" {
		var rep CandidateReport
		b, _ := json.Marshal(env.Result)
		if err := json.Unmarshal(b, &rep); err != nil {
			return &ExitError{Code: 1, Msg: fmt.Sprintf("kb %s: result: %v", env.Op, err)}
		}
		for _, cd := range rep.Candidates {
			of := ""
			if cd.Of != "" {
				of = " of " + cd.Of
			}
			lines = append(lines, fmt.Sprintf("%s %s rev=%d%s", cd.ID, cd.Reason, cd.Revision, of))
		}
		lines = append(lines, fmt.Sprintf("%d candidates (%d expired, %d stale, %d duplicate) of %d records as of %s",
			len(rep.Candidates), rep.Counts[ReasonExpired], rep.Counts[ReasonStale], rep.Counts[ReasonDuplicate], rep.Records, rep.AsOf))
	} else {
		line := strings.TrimPrefix(env.Op, "memory.")
		if env.Target != nil {
			line += " " + env.Target.ID
		}
		if m, ok := env.Result.(map[string]any); ok {
			if e, bad := m["index_error"]; bad {
				line += fmt.Sprintf(" (committed; index-sync failed: %v)", e)
			}
		}
		lines = append(lines, line)
	}
	if _, err := fmt.Fprintln(c.OutOrStdout(), strings.Join(lines, "\n")); err != nil {
		msg := fmt.Sprintf("kb %s: writing result: %v", env.Op, err)
		if env.MutationClass == "write" {
			msg += " (the change committed; inspect before retrying)"
		}
		return &ExitError{Code: 1, Msg: msg}
	}
	return nil
}
