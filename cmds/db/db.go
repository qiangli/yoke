// Package dbcmd registers the explicit database lifecycle tool. MCP's shared
// run_tool dispatch invokes this same registered command and envelope.
package dbcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

var command = &tool.Tool{
	Name: "db", Synopsis: "Manage an explicit database target and owner session.",
	Usage: "db create|show|check|backup|restore|drop [--engine E --mode persistent|memory --path P|--session S --name N] [--json]",
}

func init() { command.Run = run; tool.Register(command) }

type output struct {
	SchemaVersion string     `json:"schema_version"`
	OK            bool       `json:"ok"`
	Op            string     `json:"op"`
	Target        any        `json:"target,omitempty"`
	MutationClass string     `json:"mutation_class"`
	Effects       []string   `json:"effects"`
	PreviewLevel  string     `json:"preview_level,omitempty"`
	Result        any        `json:"result,omitempty"`
	Error         *errorBody `json:"error,omitempty"`
}
type errorBody struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	SafeToRetry bool           `json:"safe_to_retry"`
	Details     map[string]any `json:"details,omitempty"`
}

func (o output) emit(rc *tool.RunContext, asJSON bool) int {
	if asJSON {
		_ = json.NewEncoder(rc.Out).Encode(o)
	} else if o.Error != nil {
		fmt.Fprintf(rc.Err, "db %s: %s\n", o.Op, o.Error.Message)
	} else if o.Result != nil {
		fmt.Fprintln(rc.Out, o.Result)
	} else if o.Target != nil {
		fmt.Fprintln(rc.Out, o.Target)
	}
	if o.Error == nil {
		return 0
	}
	switch o.Error.Code {
	case "invalid_input", "unsupported", "exclusion_unsupported":
		return 2
	case "claim_held", "stale_epoch":
		return 9
	case "timeout":
		return 124
	case "cancelled":
		return 130
	default:
		return 1
	}
}

func failure(o output, err error) output {
	code := dbstore.Code(err)
	var conflict *coord.Conflict
	if errors.As(err, &conflict) {
		code = "claim_held"
	}
	if errors.Is(err, coord.ErrFenced) {
		code = "stale_epoch"
	}
	o.SchemaVersion, o.OK = "bashy-db-error-v1", false
	body := &errorBody{Code: code, Message: err.Error(), Retryable: code == "busy" || code == "conflict" || code == "timeout" || code == "claim_held", SafeToRetry: o.MutationClass == "read" || code == "not_found" || code == "exists" || code == "claim_held" || code == "stale_epoch" || code == "invalid_input" || code == "unsupported"}
	if conflict != nil {
		b, _ := conflict.JSON()
		var claim any
		if json.Unmarshal(b, &claim) == nil {
			body.Details = map[string]any{"claim": claim, "scope": "database"}
		}
	}
	o.Error = body
	return o
}

func run(rc *tool.RunContext, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(rc.Err, command.Usage)
		return 2
	}
	op := args[0]
	fs := tool.NewFlags("db " + op)
	engine := fs.String("engine", "", "explicit storage engine")
	mode := fs.String("mode", "", "memory or persistent")
	path := fs.String("path", "", "persistent file or directory")
	session := fs.String("session", "", "explicit owner session ID")
	name := fs.String("name", "", "database name inside owner session")
	aliases := fs.StringSlice("alias", nil, "additional registered name")
	backup := fs.String("out", "", "new backup output path")
	source := fs.String("in", "", "native backup input path")
	ready := fs.String("ready", "", "private owner rendezvous")
	_ = fs.MarkHidden("ready")
	idleTTL := fs.Duration("idle-ttl", 0, "end an idle owner session after this duration")
	requestID := fs.String("request-id", "", "deduplicate an owner mutation within the bounded window")
	attachment := fs.String("attachment", "", "explicit attached-client handle")
	detach := fs.Bool("detach", false, "release the selected attached-client handle")
	epoch := fs.Uint64("epoch", 0, "current exclusive claim epoch")
	jsonFlag := fs.Bool("json", true, "emit the versioned JSON envelope")
	yes := fs.Bool("yes", false, "confirm destructive operation")
	replace := fs.Bool("replace", false, "replace an existing target (where supported)")
	dryRun := fs.Bool("dry-run", false, "preview local effects without executing")
	explain := fs.Bool("explain", false, "explain operation without executing")
	wait := fs.Duration("wait", dbstore.DefaultWait, "maximum lock/admission wait")
	timeout := fs.Duration("timeout", 30*time.Second, "operation deadline")
	operands, code := tool.Parse(rc, command, fs, args[1:])
	if code >= 0 {
		return code
	}
	if op == "_serve" {
		if *ready == "" {
			return 2
		}
		if err := dblifecycle.Serve(rc.Ctx, *ready, *idleTTL); err != nil {
			fmt.Fprintln(rc.Err, err)
			return 1
		}
		return 0
	}
	spec := dblifecycle.Spec(op, dbstore.Mode(*mode))
	if op == "session" && len(operands) == 1 {
		spec = dblifecycle.Spec("session."+operands[0], "")
	}
	o := output{SchemaVersion: "bashy-db-result-v1", OK: true, Op: op, MutationClass: spec.MutationClass, Effects: spec.Effects}
	if *explain || *dryRun {
		o.PreviewLevel = "local-preview"
		if *explain {
			o.PreviewLevel = "explain-only"
		} else if op != "session" && op != "list" && op != "caps" && op != "register" && op != "unregister" {
			if *engine == "" || *mode == "" {
				return failure(o, fmt.Errorf("%w: --engine and --mode are required", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
			}
			t := dbstore.Target{Engine: *engine, Mode: dbstore.Mode(*mode), Path: *path, Session: *session, Name: *name}
			if t.Mode == dbstore.Persistent {
				t.Session = ""
				if t.Path != "" && !filepath.IsAbs(t.Path) {
					t.Path = filepath.Join(rc.Dir, t.Path)
				}
			}
			id, err := dbstore.Resolve(t)
			if err != nil {
				return failure(o, err).emit(rc, *jsonFlag)
			}
			o.Target = id
		} else if op == "session" && (len(operands) != 1 || (*idleTTL < 0)) {
			return failure(o, fmt.Errorf("%w: session verb or idle TTL", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
		}
		o.Result = map[string]any{"op": op, "declaration": spec, "schema": tool.SchemaFromFlags(fs)}
		return o.emit(rc, *jsonFlag)
	}
	if op == "session" {
		return sessionCommand(rc, operands, *session, *attachment, *detach, *idleTTL, *yes, *jsonFlag, *timeout)
	}
	ctx, cancel := context.WithTimeout(rc.Ctx, *timeout)
	defer cancel()
	if op == "caps" {
		if *engine == "" {
			return failure(o, fmt.Errorf("%w: --engine is required", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
		}
		a, err := dblifecycle.Engine(*engine)
		if err != nil {
			return failure(o, err).emit(rc, *jsonFlag)
		}
		o.Result = a.Caps()
		return o.emit(rc, *jsonFlag)
	}
	if op == "list" {
		return list(rc, o, *jsonFlag)
	}
	if op == "unregister" {
		return registration(rc, o, op, operands, "", nil, *jsonFlag)
	}
	if len(operands) != 0 && op != "register" {
		return failure(o, fmt.Errorf("%w: unexpected operand %q", dbstore.ErrInvalidInput, operands[0])).emit(rc, *jsonFlag)
	}
	if *engine == "" || *mode == "" {
		return failure(o, fmt.Errorf("%w: --engine and --mode are required", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
	}
	targetSession := *session
	if dbstore.Mode(*mode) == dbstore.Persistent {
		targetSession = ""
	}
	t := dbstore.Target{Engine: *engine, Mode: dbstore.Mode(*mode), Path: *path, Session: targetSession, Name: *name}
	if t.Mode == dbstore.Persistent && t.Path != "" && !filepath.IsAbs(t.Path) {
		t.Path = filepath.Join(rc.Dir, t.Path)
	}
	id, err := dbstore.Resolve(t)
	if err != nil {
		return failure(o, err).emit(rc, *jsonFlag)
	}
	o.Target = dblifecycle.ViewTarget(id, "")
	if op == "register" {
		if len(operands) != 1 {
			return failure(o, fmt.Errorf("%w: expected registered NAME", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
		}
		if *session != "" {
			res, e := dblifecycle.Call(ctx, *session, dblifecycle.Request{Op: "show", Target: id.Target})
			if e != nil {
				return failure(o, e).emit(rc, *jsonFlag)
			}
			if !res.OK {
				return ownerOutput(o, res).emit(rc, *jsonFlag)
			}
		} else {
			_, _, e := (&dblifecycle.Service{}).Show(ctx, id.Target, *wait)
			if e != nil {
				return failure(o, e).emit(rc, *jsonFlag)
			}
		}
		return registration(rc, o, op, operands, id.ID, *aliases, *jsonFlag)
	}
	if (op == "hold" || op == "release") && *session == "" {
		return failure(o, fmt.Errorf("%w: hold/release requires an explicit --session", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
	}
	if *session != "" {
		res, callErr := dblifecycle.Call(ctx, *session, dblifecycle.Request{Op: op, Target: id.Target, Wait: *wait, Out: *backup, In: *source, Replace: *replace, Yes: *yes, RequestID: *requestID, Epoch: *epoch, Attachment: *attachment})
		if callErr != nil {
			return failure(o, callErr).emit(rc, *jsonFlag)
		}
		return ownerOutput(o, res).emit(rc, *jsonFlag)
	}
	s := dblifecycle.Service{}
	switch op {
	case "create":
		_, err = s.Create(ctx, id.Target, *wait)
	case "show":
		_, o.Result, err = s.Show(ctx, id.Target, *wait)
	case "check":
		var level string
		_, level, err = s.Inspect(ctx, id.Target, *wait)
		if err == nil {
			o.Result = map[string]any{"check": level, "id": id.ID}
		}
	case "backup":
		_, err = s.Backup(ctx, id.Target, *backup, *wait)
	case "restore":
		if !*yes {
			err = fmt.Errorf("%w: restore requires --yes", dbstore.ErrInvalidInput)
		} else {
			_, err = s.Restore(ctx, id.Target, *source, *replace, *wait)
		}
	case "drop":
		if !*yes {
			err = fmt.Errorf("%w: drop requires --yes", dbstore.ErrInvalidInput)
		} else {
			_, err = s.Drop(ctx, id.Target, *wait)
		}
	default:
		err = fmt.Errorf("%w: db %s", dbstore.ErrUnsupported, op)
	}
	if err != nil {
		return failure(o, err).emit(rc, *jsonFlag)
	}
	if o.Result == nil {
		o.Result = map[string]any{"id": id.ID}
	}
	return o.emit(rc, *jsonFlag)
}

func ownerOutput(o output, res dblifecycle.Response) output {
	o.SchemaVersion, o.OK, o.Op, o.Target, o.MutationClass, o.Effects, o.Result = res.SchemaVersion, res.OK, res.Op, res.Target, res.MutationClass, res.Effects, res.Result
	if res.Error != nil {
		o.Error = &errorBody{Code: res.Error.Code, Message: res.Error.Message, Retryable: res.Error.Retryable, SafeToRetry: res.Error.SafeToRetry, Details: res.Error.Details}
	}
	return o
}

func sessionCommand(rc *tool.RunContext, args []string, id, attachment string, detach bool, ttl time.Duration, yes, asJSON bool, timeout time.Duration) int {
	o := output{SchemaVersion: "bashy-db-result-v1", OK: true, Op: "session", MutationClass: "write", Effects: []string{"proc.spawn", "fs.write"}}
	if len(args) != 1 {
		return failure(o, fmt.Errorf("%w: db session requires start|attach|list|show|stop", dbstore.ErrInvalidInput)).emit(rc, asJSON)
	}
	op := args[0]
	o.Op = "session." + op
	spec := dblifecycle.Spec(o.Op, "")
	o.MutationClass = spec.MutationClass
	o.Effects = spec.Effects
	ctx, cancel := context.WithTimeout(rc.Ctx, timeout)
	defer cancel()
	switch op {
	case "start":
		ready, err := os.CreateTemp("", "bashy-db-ready-*")
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		readyPath := ready.Name()
		_ = ready.Close()
		defer os.Remove(readyPath)
		exe, err := os.Executable()
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		cmd := exec.Command(exe, "db", "_serve", "--ready", readyPath, "--idle-ttl", ttl.String())
		cmd.Env = rc.Env
		cmd.Dir = rc.Dir
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		_ = cmd.Process.Release()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			b, err := os.ReadFile(readyPath)
			if err == nil && len(b) > 0 {
				id = string(b)
				break
			}
			select {
			case <-ctx.Done():
				return failure(o, dbstore.FromContext(ctx.Err())).emit(rc, asJSON)
			case <-ticker.C:
			}
		}
		info, err := dblifecycle.ReadSession(id)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		o.Result = info
		return o.emit(rc, asJSON)
	case "list":
		rows, err := dblifecycle.ListSessions()
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		o.Result = rows
		return o.emit(rc, asJSON)
	case "show":
		info, err := dblifecycle.ReadSession(id)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		if info.State == "running" {
			status, callErr := dblifecycle.Call(ctx, id, dblifecycle.Request{Op: "status"})
			if callErr == nil && status.OK {
				o.Result = map[string]any{"info": info, "status": status.Result}
				return o.emit(rc, asJSON)
			}
		}
		o.Result = info
		return o.emit(rc, asJSON)
	case "attach", "stop":
		if id == "" {
			return failure(o, fmt.Errorf("%w: --session is required", dbstore.ErrInvalidInput)).emit(rc, asJSON)
		}
		wireOp := op
		if op == "attach" && detach {
			wireOp = "detach"
		}
		res, err := dblifecycle.Call(ctx, id, dblifecycle.Request{Op: wireOp, Yes: yes, Attachment: attachment})
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		return ownerOutput(o, res).emit(rc, asJSON)
	default:
		return failure(o, fmt.Errorf("%w: session %s", dbstore.ErrUnsupported, op)).emit(rc, asJSON)
	}
}

func list(rc *tool.RunContext, o output, asJSON bool) int {
	rows, errs := fleet.New().Resources()
	if len(errs) > 0 {
		return failure(o, errs[0]).emit(rc, asJSON)
	}
	var dbs []any
	for _, r := range rows {
		if r.Kind == "db" {
			dbs = append(dbs, map[string]any{"schema_version": "bashy-node-v1", "kind": "db", "name": r.Name, "aliases": r.Aliases, "target": r.Members})
		}
	}
	sessions, err := dblifecycle.ListSessions()
	if err != nil {
		return failure(o, err).emit(rc, asJSON)
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		ctx, cancel := context.WithTimeout(rc.Ctx, 2*time.Second)
		res, err := dblifecycle.Call(ctx, session.ID, dblifecycle.Request{Op: "status"})
		cancel()
		if err != nil || !res.OK {
			continue
		}
		info, ok := res.Result.(map[string]any)
		if !ok {
			continue
		}
		values, ok := info["databases"].([]any)
		if !ok {
			continue
		}
		for _, raw := range values {
			id, ok := raw.(string)
			if !ok {
				continue
			}
			dbs = append(dbs, map[string]any{"schema_version": "bashy-node-v1", "kind": "db", "id": id, "session": session.ID})
		}
	}
	o.Result = dbs
	return o.emit(rc, asJSON)
}

func registration(rc *tool.RunContext, o output, op string, args []string, canonical string, aliases []string, asJSON bool) int {
	cat := fleet.New()
	if op == "unregister" {
		if len(args) != 1 {
			return failure(o, fmt.Errorf("%w: expected one registered name", dbstore.ErrInvalidInput)).emit(rc, asJSON)
		}
		r, ok := cat.Resource(args[0])
		if !ok || r.Kind != "db" {
			return failure(o, fmt.Errorf("%w: registered database %q", dbstore.ErrNotFound, args[0])).emit(rc, asJSON)
		}
		if len(r.Members) != 1 {
			return failure(o, fmt.Errorf("%w: registered database has no canonical identity", dbstore.ErrInvalidInput)).emit(rc, asJSON)
		}
		if err := coord.Guard(rc.Ctx, coord.Self(), coord.Use{Kind: "db", Name: r.Members[0]}); err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		err := cat.RemoveResource(r.Name)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		o.Result = map[string]any{"name": r.Name}
		return o.emit(rc, asJSON)
	}
	if len(args) != 1 || canonical == "" {
		return failure(o, fmt.Errorf("%w: expected NAME and explicit existing target", dbstore.ErrInvalidInput)).emit(rc, asJSON)
	}
	if _, ok := cat.Resource(args[0]); ok {
		return failure(o, fmt.Errorf("%w: registered name %q", dbstore.ErrExists, args[0])).emit(rc, asJSON)
	}
	for _, alias := range aliases {
		if _, ok := cat.Resource(alias); ok {
			return failure(o, fmt.Errorf("%w: registered alias %q", dbstore.ErrExists, alias)).emit(rc, asJSON)
		}
	}
	err := cat.SaveResource(fleet.Resource{Name: args[0], Kind: "db", Members: []string{canonical}, Aliases: aliases})
	if err != nil {
		return failure(o, err).emit(rc, asJSON)
	}
	o.Result = map[string]any{"name": args[0], "target": canonical, "aliases": aliases}
	return o.emit(rc, asJSON)
}
