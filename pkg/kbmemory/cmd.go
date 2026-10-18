package kbmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// ExitError carries the contract exit code of a failed verb.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// NewMemoryCmd is `kb memory`, mounted by bashy's dispatch beside recall
// (pkg/kb stays a leaf, so it cannot register this itself). Cancellation
// comes from the command's own context.
func NewMemoryCmd() *cobra.Command {
	dir, _ := os.Getwd()
	return newCmd(nil, dir)
}

// inheritedStoreFlags are kb's root store selectors. A memory target is named
// explicitly, so setting one is refused rather than silently ignored.
var inheritedStoreFlags = []string{"dir", "ring", "repo", "user", "base-dir"}

// MCPCommand is the typed MCP tool. It runs the same command and flags, so
// the CLI and MCP share one parser, one Run and one envelope.
func MCPCommand() mcp.RegisteredCommand {
	schema := Schema()
	return mcp.RegisteredCommand{
		Name: "kb_memory", Synopsis: "explicit kb memory collections: init, select, add, inspect, update",
		Usage: usage, Schema: &schema, Effects: []string{"fs.read", "fs.write", "db.read", "db.write"},
		Run: func(ctx context.Context, argv []string, stdin, dir string) (string, string, int) {
			cmd := newCmd(nil, dir)
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

// Schema projects the command's flags for MCP. Positionals: VERB TARGET [ID].
func Schema() tool.ArgSchema {
	s := tool.ArgSchema{Positionals: []tool.ArgParameter{{Name: "verb", Required: true}, {Name: "target", Required: true}, {Name: "id"}}}
	newCmd(nil, "").Flags().VisitAll(func(f *pflag.Flag) {
		typ := "string"
		switch f.Value.Type() {
		case "bool":
			typ = "bool"
		case "int64", "int":
			typ = "int"
		}
		s.Flags = append(s.Flags, tool.ArgFlag{Name: f.Name, Shorthand: f.Shorthand, Type: typ})
	})
	return s
}

const usage = `memory init|select|add|inspect|update TARGET [ID] [flags]`

func newCmd(ctxFn func() context.Context, dir string) *cobra.Command {
	var (
		typ, title, body, file, osName, episode, source, requestID, sessionID string
		tags, repos                                                           []string
		ifRevision                                                            int64
		wait, timeout                                                         time.Duration
		asJSON                                                                bool
	)
	cmd := &cobra.Command{
		Use:   "memory init|select|add|inspect|update TARGET [ID]",
		Short: "Opt-in memory collections in an explicit db target (both storage modes)",
		Long: `An opt-in memory collection is a document collection in an explicitly
named db target. Nothing is implicit: there is no default collection and no
remembered selection — every call names TARGET, and only init creates.

  TARGET  the db locator, as 'bashy db' prints it (colons escaped):
          db:sqlite%3Afile%3APATH          persistent (survives restarts)
          db:sqlite%3Amem%3ASESSION/NAME   in-memory, held by an explicit
                                           'bashy db session start' owner

  init TARGET          create the database and its memory collection; reports
                       each store location (fails 'exists' if present)
  select TARGET        verify TARGET holds a memory collection (read-only)
  add TARGET           write a candidate record (--body, -f FILE, -f - stdin);
                       prints its stable ID and revision
  inspect TARGET ID    read one record and its revision
  update TARGET ID --if-revision N
                       change fields; a stale N is a 'conflict', never a
                       lost update

Records keep kb's vocabulary (type, candidate status, tags, scope, source
provenance, created/updated). Supersede and forget belong to kb's lifecycle
(Sprint 284 M6) and are not offered here. Nothing captures conversations.`,
		Args:          cobra.RangeArgs(2, 3),
		SilenceUsage:  true,
		SilenceErrors: true,
		// kb's root hook resolves one OKF store and prints its header; a
		// memory target is named explicitly, so suppress it as recall does.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(c *cobra.Command, args []string) error {
			base := c.Context()
			if ctxFn != nil {
				base = ctxFn()
			}
			if base == nil {
				base = context.Background()
			}
			ctx, cancel := context.WithTimeout(base, timeout)
			defer cancel()
			op := Envelope{Op: "memory." + args[0], Collection: Collection, MutationClass: mutationClass(args[0])}
			for _, name := range inheritedStoreFlags {
				if f := c.InheritedFlags().Lookup(name); f != nil && f.Changed {
					return finishCmd(c, fail(op, invalid("--"+name+" selects a kb store; a memory TARGET is named explicitly")), asJSON)
				}
			}
			req := Request{Verb: args[0], Target: args[1], Session: sessionID, Dir: dir, Episode: episode, SourceRef: source, IfRevision: ifRevision, RequestID: requestID, Wait: wait}
			if len(args) == 3 {
				req.ID = args[2]
			}
			set := func(name string, v *string) *string {
				if c.Flags().Changed(name) {
					return v
				}
				return nil
			}
			req.Type, req.Title, req.OS = set("type", &typ), set("title", &title), set("os", &osName)
			req.Body = set("body", &body)
			if c.Flags().Changed("tags") {
				req.Tags = &tags
			}
			if c.Flags().Changed("repos") {
				req.Repos = &repos
			}

			if file != "" {
				if req.Body != nil {
					return finishCmd(c, fail(op, invalid("choose --body or -f, not both")), asJSON)
				}
				text, ref, err := readBody(ctx, c.InOrStdin(), dir, file)
				if err != nil {
					return finishCmd(c, fail(op, err), asJSON)
				}
				req.Body = &text
				if req.SourceRef == "" {
					req.SourceRef = ref
				}
			}
			env := Run(ctx, req)
			return finishCmd(c, env, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&typ, "type", "lesson", "record type: lesson|gotcha|runbook|decision|fact")
	f.StringVar(&title, "title", "", "record title")
	f.StringVar(&body, "body", "", "record body text")
	f.StringVarP(&file, "file", "f", "", "read the body from FILE ('-' = stdin)")
	f.StringSliceVar(&tags, "tags", nil, "tags (comma-separated or repeated)")
	f.StringSliceVar(&repos, "repos", nil, "scope: repo basenames")
	f.StringVar(&osName, "os", "", "scope: GOOS value")
	f.StringVar(&episode, "episode", "", "add: episode to stamp (default: this process's episode)")
	f.StringVar(&source, "source", "", "source ref the record came from (default for -f FILE: file:PATH)")
	f.Int64Var(&ifRevision, "if-revision", 0, "update: the revision the change is based on (required)")
	f.StringVar(&requestID, "request-id", "", "owner-session mutation dedup key")
	f.StringVar(&sessionID, "session", "", "explicit owner session for a persistent target")
	f.DurationVar(&wait, "wait", 5*time.Second, "bound on waiting for a lock, admission or hold")
	f.DurationVar(&timeout, "timeout", 30*time.Second, "bound on the whole operation")
	f.BoolVar(&asJSON, "json", false, "emit the bashy-kb-memory-result-v1 envelope")
	return cmd
}

func invalid(msg string) error { return fmt.Errorf("%w: %s", dbstore.ErrInvalidInput, msg) }

// readBody reads -f FILE or -f - (stdin), bounded by MaxBody and by ctx: a
// blocked read is abandoned when the operation's timeout or cancel fires.
func readBody(ctx context.Context, stdin io.Reader, dir, file string) (string, string, error) {
	var r io.Reader = stdin
	ref := ""
	if file != "-" {
		p := file
		if !filepath.IsAbs(p) && dir != "" {
			p = filepath.Join(dir, p)
		}
		fh, err := os.Open(p)
		if err != nil {
			return "", "", fmt.Errorf("%w: %v", dbstore.ErrNotFound, err)
		}
		defer fh.Close()
		r, ref = fh, "file:"+file
	}
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
		done <- read{b, err}
	}()
	var b []byte
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case got := <-done:
		if got.err != nil {
			return "", "", got.err
		}
		b = got.b
	}
	if len(b) > MaxBody {
		return "", "", fmt.Errorf("%w: body is over %d bytes", dbstore.ErrLimitExceeded, MaxBody)
	}
	return string(b), ref, nil
}

// finishCmd writes the envelope. A failed write is a failure: a caller that
// never saw the result (a broken pipe) must not read exit 0 as success.
func finishCmd(c *cobra.Command, env Envelope, asJSON bool) error {
	var werr error
	if asJSON {
		werr = json.NewEncoder(c.OutOrStdout()).Encode(env)
	} else if env.OK {
		_, werr = fmt.Fprintln(c.OutOrStdout(), summary(env))
	}
	if werr != nil {
		msg := fmt.Sprintf("kb %s: writing result: %v", env.Op, werr)
		if env.OK && env.MutationClass == "write" {
			msg += " (the write committed; inspect before retrying)"
		}
		return &ExitError{Code: 1, Msg: msg}
	}
	if env.Error != nil {
		return &ExitError{Code: env.ExitCode(), Msg: fmt.Sprintf("kb %s: %s: %s", env.Op, env.Error.Code, env.Error.Message)}
	}
	return nil
}

func summary(env Envelope) string {
	where := ""
	if env.Target != nil {
		where = env.Target.ID
	}
	rev := ""
	if env.Revision != nil {
		rev = fmt.Sprintf(" revision %d", *env.Revision)
	}
	var res struct {
		Record  *Record `json:"record"`
		Records any     `json:"records"`
	}
	if decode(env.Result, &res) == nil {
		if r := res.Record; r != nil {
			if env.Op == "memory.inspect" {
				return fmt.Sprintf("%s [%s %s]%s\n%s", r.ID, r.Type, r.Status, rev, r.Body)
			}
			return fmt.Sprintf("%s %s%s in %s", strings.TrimPrefix(env.Op, "memory."), r.ID, rev, where)
		}
		if res.Records != nil {
			return fmt.Sprintf("selected %s (%v records)", where, res.Records)
		}
	}
	return fmt.Sprintf("%s %s", strings.TrimPrefix(env.Op, "memory."), where)
}
