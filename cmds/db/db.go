// Package dbcmd registers the explicit database lifecycle tool. MCP's shared
// run_tool dispatch invokes this same registered command and envelope.
package dbcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	_ "github.com/qiangli/yoke/pkg/dbfts"
	_ "github.com/qiangli/yoke/pkg/dbgraph"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbsql"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/spf13/pflag"
)

var command = &tool.Tool{
	PreviewLevel: "local-preview",
	Name:         "db", Synopsis: "Manage an explicit database target and owner session.",
	Usage: "db create|show|check|backup|restore|drop|export|import [--engine E --mode persistent|memory --path P|--session S --name N] [--json]\n" +
		"db sql query|exec|schema show|schema apply TARGET [--sql SQL] [--param [NAME=]TYPE:VALUE]... [--params JSON] [--input FILE|-]\n" +
		"db doc get|find|put|patch|rm|batch|export|import|collection create|drop|list|index add|rm|list TARGET [--collection C] [--id ID] [--doc JSON] [--filter JSON] [--input FILE|-]" + "\n" +
		"db kv bucket create|drop|list|get|put|delete|scan|cas|batch TARGET [--bucket B] [--key text:K|b64:K] [--value text:V|b64:V] [--expect text:V|b64:V|--expect-absent] [--prefix ...] [--input FILE|-]" + "\n" +
		"db fts search|get|put|delete|batch|check|mapping show|rebuild TARGET [--input FILE|-]" + "\n" +
		"db vec put|get|rm|batch|search|export|import|collection add|rm|list TARGET [--collection C] [--id ID] [--vector JSON|--text T --endpoint URL [--api-key-env VAR]] [--metadata JSON] [--filter JSON] [--input FILE|-]" + "\n" +
		"db obj bucket create|drop|list|get|put|delete|rm|stat|list|transfer|export|import TARGET [--bucket B] [--key K] [--value text:...|b64:...] [--content-type T] [--metadata JSON] [--input FILE|-]" + "\n" +
		"db graph query|mutate|schema show|schema apply|node get|put|rm|edge get|put|rm|edges|export|import TARGET [--input FILE|-]" + "\n" +
		"db ts ingest|range|agg|retain|export|import|remote query|ingest TARGET [--series S] [--labels JSON] [--points JSON] [--bucket D] [--funcs F,...] [--retention D] [--expr E] [--step D] [--endpoint URL] [--input FILE|-]",
}

func init() { command.Run = run; tool.Register(command) }

type dbFlags struct {
	fs                                                                              *pflag.FlagSet
	engine, mode, path, session, name, backup, source, ready, requestID, attachment *string
	sqlText, params, input, since, onConflict                                       *string
	collection, docID, doc, patch, filter, ifRevision, index                        *string
	fields                                                                          *[]string
	ifAbsent, unique                                                                *bool
	bucket, key, value, expect, start, prefix, ttl                                  *string
	metric, model, vector, text, endpoint, apiKeyEnv, metadata                      *string
	contentType, remote, destination, checksum                                      *string
	graphQuery, graphVariables, graphSet, graphDel, graphSchema                     *string
	graphNode, graphEdge, graphFrom, graphType, graphTo, graphDirection             *string
	tsSeries, tsLabels, tsPoints, tsFuncs, tsRetention, tsExpr, tsStep              *string
	dimension                                                                       *int
	includeVectors                                                                  *bool
	deleteFlag                                                                      *bool
	expectAbsent                                                                    *bool
	aliases, param                                                                  *[]string
	limit, maxBytes                                                                 *int
	idleTTL, wait, timeout                                                          *time.Duration
	epoch                                                                           *uint64
	detach, jsonFlag, yes, replace, dryRun, explain                                 *bool
}

// The CLI parser, explain output and typed MCP schema share these declarations.
func declareFlags(op string) dbFlags {
	fs := tool.NewFlags("db " + op)
	f := dbFlags{fs: fs}
	f.engine = fs.String("engine", "", "explicit storage engine")
	f.mode = fs.String("mode", "", "memory or persistent")
	f.path = fs.String("path", "", "persistent file or directory")
	f.session = fs.String("session", "", "explicit owner session ID")
	f.name = fs.String("name", "", "database name inside owner session")
	f.aliases = fs.StringSlice("alias", nil, "additional registered name")
	f.backup = fs.String("out", "", "new backup file or export directory")
	f.source = fs.String("in", "", "native backup file or export directory")
	f.sqlText = fs.String("sql", "", "SQL text for db sql query/exec/schema apply (values are bound, never interpolated)")
	f.param = fs.StringArray("param", nil, "bound parameter [NAME=]TYPE:VALUE, TYPE int|f64|text|bytes|null")
	f.params = fs.String("params", "", "bound parameters as a JSON array or object of tagged values")
	f.input = fs.String("input", "", "typed JSON input document (FILE or - for stdin)")
	f.limit = fs.Int("limit", 0, "maximum rows or objects returned (default 100)")
	f.maxBytes = fs.Int("max-bytes", 0, "maximum result bytes (default 262144)")
	f.since = fs.String("since", "", "resume from a bashy-cursor-v1 envelope")
	f.onConflict = fs.String("on-conflict", "", "import record conflicts: error|skip|replace")
	f.collection = fs.String("collection", "", "document collection for db doc")
	f.docID = fs.String("id", "", "stable document ID for db doc (put generates one when omitted)")
	f.doc = fs.String("doc", "", "JSON object document for db doc put")
	f.patch = fs.String("patch", "", "JSON merge patch (RFC 7386) for db doc patch")
	f.filter = fs.String("filter", "", "structured JSON filter for db doc find (values are bound, never interpolated)")
	f.ifRevision = fs.String("if-revision", "", "apply only if the document is at this revision")
	f.ifAbsent = fs.Bool("if-absent", false, "insert only if the document ID does not exist")
	f.index = fs.String("index", "", "document index name for db doc index add|rm")
	f.fields = fs.StringArray("field", nil, "indexed field path (a.b[0].c) for db doc index add")
	f.unique = fs.Bool("unique", false, "make a db doc index unique")
	f.bucket = fs.String("bucket", "", "kv bucket name (UTF-8)")
	f.key = fs.String("key", "", "kv key as text:UTF8 or b64:BASE64")
	f.value = fs.String("value", "", "kv value as text:UTF8 or b64:BASE64")
	f.expect = fs.String("expect", "", "kv cas expected current value as text:... or b64:...")
	f.expectAbsent = fs.Bool("expect-absent", false, "kv cas: succeed only if the key is absent")
	f.start = fs.String("start", "", "kv scan: first key at or after text:... or b64:...")
	f.prefix = fs.String("prefix", "", "kv scan: only keys with this text:... or b64:... prefix")
	f.ttl = fs.String("ttl", "", "unsupported: kv has no TTL or expiry")
	f.dimension = fs.Int("dimension", 0, "db vec collection add: vector dimension (1..8192)")
	f.metric = fs.String("metric", "", "db vec collection add: distance metric cosine|l2|l1")
	f.model = fs.String("model", "", "db vec: embedding model identity a collection persists")
	f.vector = fs.String("vector", "", "db vec: supplied vector as {\"t\":\"vec\",\"dtype\":\"f32\",\"b64\":...} or a JSON number array")
	f.text = fs.String("text", "", "db vec put|search: text to embed (requires an explicit --endpoint)")
	f.endpoint = fs.String("endpoint", "", "db vec: explicit OpenAI-compatible embeddings endpoint for --text (no default)")
	f.apiKeyEnv = fs.String("api-key-env", "", "db vec: environment variable holding the --endpoint bearer key")
	f.metadata = fs.String("metadata", "", "db vec put: JSON object metadata")
	f.includeVectors = fs.Bool("include-vectors", false, "db vec get|search: return stored vectors")
	f.contentType = fs.String("content-type", "", "db obj put: MIME content type")
	f.remote = fs.String("remote", "", "db obj transfer: remote rclone target name or path")
	f.destination = fs.String("destination", "", "db obj transfer: destination directory or remote target")
	f.checksum = fs.String("checksum", "", "db obj transfer: expected SHA256 checksum")
	f.deleteFlag = fs.Bool("delete", false, "db obj transfer: delete destination extras not present in source (sync)")
	f.graphQuery = fs.String("query", "", "db graph query: parameterized DQL query")
	f.graphVariables = fs.String("variables", "", "db graph query: JSON object of bound DQL variables")
	f.graphSet = fs.String("set-nquads", "", "db graph mutate: RDF nquads to set")
	f.graphDel = fs.String("del-nquads", "", "db graph mutate: RDF nquads to delete")
	f.graphSchema = fs.String("schema", "", "db graph schema apply: DQL schema text")
	f.graphNode = fs.String("node", "", "db graph node put: typed node JSON")
	f.graphEdge = fs.String("edge", "", "db graph edge put: typed edge JSON")
	f.graphFrom = fs.String("from", "", "db graph edge identity: source node")
	f.graphType = fs.String("type", "", "db graph edge identity: edge type")
	f.graphTo = fs.String("to", "", "db graph edge identity: destination node")
	f.graphDirection = fs.String("direction", "", "db graph edges: in or out")
	f.tsSeries = fs.String("series", "", "db ts: series name for ingest|range|agg|retain")
	f.tsLabels = fs.String("labels", "", "db ts: label set as a JSON object of strings")
	f.tsPoints = fs.String("points", "", "db ts ingest: samples as a JSON array of {ts, value} tagged values")
	f.tsFuncs = fs.String("funcs", "", "db ts agg: comma-separated subset of count,min,max,sum,avg")
	f.tsRetention = fs.String("retention", "", "db ts retain: keep duration like 30d, or off")
	f.tsExpr = fs.String("expr", "", "db ts remote query: VictoriaMetrics expression")
	f.tsStep = fs.String("step", "", "db ts remote query: query resolution like 60s, 15m")
	f.ready = fs.String("ready", "", "private owner rendezvous")
	_ = fs.MarkHidden("ready")
	f.idleTTL = fs.Duration("idle-ttl", 0, "end an idle owner session after this duration")
	f.requestID = fs.String("request-id", "", "deduplicate an owner mutation within the bounded window")
	f.attachment = fs.String("attachment", "", "explicit attached-client handle")
	f.detach = fs.Bool("detach", false, "release the selected attached-client handle")
	f.epoch = fs.Uint64("epoch", 0, "current exclusive claim epoch")
	f.jsonFlag = fs.Bool("json", true, "emit the versioned JSON envelope")
	f.yes = fs.Bool("yes", false, "confirm destructive operation")
	f.replace = fs.Bool("replace", false, "replace an existing target (where supported)")
	f.dryRun = fs.Bool("dry-run", false, "preview local effects without executing")
	f.explain = fs.Bool("explain", false, "explain operation without executing")
	f.wait = fs.Duration("wait", dbstore.DefaultWait, "maximum lock/admission wait")
	f.timeout = fs.Duration("timeout", 30*time.Second, "operation deadline")
	return f
}

// Schema is the shared typed flag source with the command's positional shape.
func Schema() tool.ArgSchema {
	s := tool.SchemaFromFlags(declareFlags("").fs)
	s.Positionals = []tool.ArgParameter{{Name: "verb", Required: true}, {Name: "subject"}, {Name: "target"}}
	for i := range s.Flags {
		switch s.Flags[i].Type {
		case "duration", "uint64", "stringSlice", "stringArray":
			if s.Flags[i].Type == "stringSlice" || s.Flags[i].Type == "stringArray" {
				s.Flags[i].Default = ""
			}
			s.Flags[i].Type = "string" // exact textual wire value; pflag validates it
		}
	}
	return s
}

type output struct {
	SchemaVersion string     `json:"schema_version"`
	OK            bool       `json:"ok"`
	Op            string     `json:"op"`
	Target        any        `json:"target,omitempty"`
	MutationClass string     `json:"mutation_class"`
	Effects       []string   `json:"effects"`
	PreviewLevel  string     `json:"preview_level,omitempty"`
	Result        any        `json:"result,omitempty"`
	Page          any        `json:"page,omitempty"`
	Warnings      []string   `json:"warnings,omitempty"`
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
	var child []string
	if op == "hold" {
		for i, arg := range args[1:] {
			if arg == "--" {
				child = append([]string(nil), args[i+2:]...)
				args = args[:i+1]
				break
			}
		}
	}
	f := declareFlags(op)
	fs := f.fs
	engine, mode, path, session, name, aliases := f.engine, f.mode, f.path, f.session, f.name, f.aliases
	backup, source, ready, idleTTL, requestID := f.backup, f.source, f.ready, f.idleTTL, f.requestID
	attachment, detach, epoch, jsonFlag, yes := f.attachment, f.detach, f.epoch, f.jsonFlag, f.yes
	replace, dryRun, explain, wait, timeout := f.replace, f.dryRun, f.explain, f.wait, f.timeout
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
	catOp, raw := "", json.RawMessage(nil)
	if op == "doc" || op == "vec" || op == "obj" || op == "graph" || op == "ts" {
		var err error
		if op == "vec" {
			if catOp, operands, err = vecVerb(operands); err == nil {
				raw, err = vecInput(rc, f)
			}
		} else if op == "obj" {
			if catOp, operands, err = objVerb(operands); err == nil {
				raw, err = objInput(rc, f)
			}
		} else if op == "graph" {
			if catOp, operands, err = graphVerb(operands); err == nil {
				raw, err = graphInput(rc, f)
			}
		} else if op == "ts" {
			if catOp, operands, err = tsVerb(operands); err == nil {
				raw, err = tsInput(rc, f)
			}
		} else if catOp, operands, err = docVerb(operands); err == nil {
			raw, err = docInput(rc, f, catOp)
		}
		if err != nil {
			o := output{SchemaVersion: "bashy-db-result-v1", Op: op, MutationClass: "read"}
			return failure(o, err).emit(rc, *jsonFlag)
		}
	} else if op == "sql" || op == "kv" || op == "fts" || op == "export" || op == "import" {
		var err error
		if op == "kv" {
			if catOp, operands, err = kvVerb(operands); err == nil {
				raw, err = kvInput(rc, f)
			}
		} else if op == "fts" {
			if catOp, operands, err = ftsVerb(operands); err == nil {
				raw, err = ftsInput(rc, f, op)
			}
		} else if catOp, operands, err = sqlVerb(op, operands); err == nil && op != "export" && op != "import" {
			raw, err = categoryInput(rc, f, op)
		}
		if err != nil {
			o := output{SchemaVersion: "bashy-db-result-v1", Op: op, MutationClass: "read"}
			return failure(o, err).emit(rc, *jsonFlag)
		}
	}
	spec := dblifecycle.Spec(op, dbstore.Mode(*mode))
	if catOp != "" {
		cat, access, ok, err := dblifecycle.LookupCategoryOp(catOp, raw)
		if ok && err != nil {
			o := output{SchemaVersion: "bashy-db-result-v1", Op: catOp, MutationClass: "read"}
			return failure(o, err).emit(rc, *jsonFlag)
		}
		if ok {
			spec = cat.Declare(raw, dbstore.Mode(*mode), access)
		}
	}
	if op == "session" && len(operands) == 1 {
		spec = dblifecycle.Spec("session."+operands[0], "")
	}
	o := output{SchemaVersion: "bashy-db-result-v1", OK: true, Op: op, MutationClass: spec.MutationClass, Effects: spec.Effects}
	if catOp != "" {
		o.Op = catOp
	}
	if *explain || *dryRun {
		o.PreviewLevel = "local-preview"
		if *explain {
			o.PreviewLevel = "explain-only"
		} else if op != "session" && op != "list" && op != "caps" && op != "register" && op != "unregister" {
			var t dbstore.Target
			if len(operands) == 1 {
				var err error
				t, err = targetOperand(operands[0])
				if err != nil {
					return failure(o, err).emit(rc, *jsonFlag)
				}
			} else {
				if *engine == "" || *mode == "" {
					return failure(o, fmt.Errorf("%w: explicit TARGET or --engine and --mode are required", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
				}
				t = dbstore.Target{Engine: *engine, Mode: dbstore.Mode(*mode), Path: *path, Session: *session, Name: *name}
			}
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
		o.Result = map[string]any{"op": op, "declaration": spec, "schema": Schema()}
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
		return list(ctx, rc, o, *jsonFlag)
	}
	if op == "unregister" {
		return registration(rc, o, op, operands, "", nil, *jsonFlag)
	}
	var rawTarget string
	if op == "register" {
		if len(operands) == 2 {
			rawTarget = operands[1]
		} else if len(operands) != 1 {
			return failure(o, fmt.Errorf("%w: register requires NAME and TARGET", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
		}
	} else if len(operands) == 1 {
		rawTarget = operands[0]
	} else if len(operands) > 1 {
		return failure(o, fmt.Errorf("%w: too many operands", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
	}
	if rawTarget != "" && (*engine != "" || *mode != "" || *path != "" || *name != "") {
		return failure(o, fmt.Errorf("%w: choose a locator or target flags", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
	}
	var t dbstore.Target
	if rawTarget != "" {
		var e error
		t, e = targetOperand(rawTarget)
		if e != nil {
			return failure(o, e).emit(rc, *jsonFlag)
		}
	} else {
		if *engine == "" || *mode == "" {
			return failure(o, fmt.Errorf("%w: explicit TARGET or --engine and --mode are required", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
		}
		targetSession := *session
		if dbstore.Mode(*mode) == dbstore.Persistent {
			targetSession = ""
		}
		t = dbstore.Target{Engine: *engine, Mode: dbstore.Mode(*mode), Path: *path, Session: targetSession, Name: *name}
	}
	if t.Mode == dbstore.Persistent && t.Path != "" && !filepath.IsAbs(t.Path) {
		t.Path = filepath.Join(rc.Dir, t.Path)
	}
	id, err := dbstore.Resolve(t)
	if err != nil {
		return failure(o, err).emit(rc, *jsonFlag)
	}
	o.Target = dblifecycle.ViewTarget(id, "")
	if op == "create" && id.Engine == "bleve" {
		raw, err = ftsInput(rc, f, op)
		if err != nil {
			return failure(o, err).emit(rc, *jsonFlag)
		}
	}
	if op == "export" || op == "import" {
		// The logical interchange belongs to the target's category.
		catOp = dblifecycle.ViewTarget(id, "").Category + "." + op
		o.Op = catOp
		if strings.HasPrefix(catOp, "graph.") {
			raw, err = graphInput(rc, f)
		} else if strings.HasPrefix(catOp, "fts.") {
			raw, err = ftsInput(rc, f, op)
		} else {
			raw, err = categoryInput(rc, f, op)
		}
		if err != nil {
			return failure(o, err).emit(rc, *jsonFlag)
		}
	}
	if catOp != "" {
		cat, access, ok, err := dblifecycle.LookupCategoryOp(catOp, raw)
		if !ok {
			err = fmt.Errorf("%w: %s on engine %s", dbstore.ErrUnsupported, catOp, id.Engine)
		}
		if err != nil {
			return failure(o, err).emit(rc, *jsonFlag)
		}
		spec = cat.Declare(raw, id.Mode, access)
		o.MutationClass, o.Effects = spec.MutationClass, spec.Effects
	}
	callSession := *session
	if t.Mode == dbstore.Memory {
		callSession = t.Session
	}
	// A `db hold -- CMD` child presents the hold's epoch. A memory target
	// already names its session, so match the session rather than require
	// it to be unset.
	if holdSession := envValue(rc.Env, "BASHY_DB_HOLD_SESSION"); holdSession != "" && envValue(rc.Env, "BASHY_DB_HOLD_TARGET") == id.ID && (callSession == "" || callSession == holdSession) {
		callSession = holdSession
		if *epoch == 0 {
			*epoch, _ = strconv.ParseUint(envValue(rc.Env, "BASHY_DB_HOLD_EPOCH"), 10, 64)
		}
	}
	if op == "register" {
		operands = operands[:1]
		if callSession != "" {
			res, e := dblifecycle.Call(ctx, callSession, dblifecycle.Request{Op: "show", Target: id.Target})
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
	if op == "hold" && len(child) > 0 {
		return holdChild(ctx, rc, o, id.Target, callSession, child, *wait, *timeout, *jsonFlag)
	}
	startedOwner := false
	if op == "hold" && callSession == "" {
		info, startErr := startSession(ctx, rc, 0)
		if startErr != nil {
			return failure(o, startErr).emit(rc, *jsonFlag)
		}
		callSession, startedOwner = info.ID, true
	}
	if (op == "hold" || op == "release") && callSession == "" {
		return failure(o, fmt.Errorf("%w: hold/release requires an explicit --session or -- CMD", dbstore.ErrInvalidInput)).emit(rc, *jsonFlag)
	}
	if callSession != "" {
		req := dblifecycle.Request{Op: op, Target: id.Target, Wait: *wait, Out: *backup, In: *source, Replace: *replace, Yes: *yes, RequestID: *requestID, Epoch: *epoch, Attachment: *attachment, Input: raw}
		if catOp != "" {
			req = dblifecycle.Request{Op: catOp, Target: id.Target, Wait: *wait, Yes: *yes, RequestID: *requestID, Epoch: *epoch, Attachment: *attachment, Input: raw}
		}
		res, callErr := dblifecycle.Call(ctx, callSession, req)
		if startedOwner && (callErr != nil || !res.OK) {
			_, _ = dblifecycle.Call(context.Background(), callSession, dblifecycle.Request{Op: "stop", Yes: true})
		}
		if callErr != nil {
			return failure(o, callErr).emit(rc, *jsonFlag)
		}
		if startedOwner && res.OK {
			if result, ok := res.Result.(map[string]any); ok {
				result["session"] = callSession
			}
		}
		return ownerOutput(o, res).emit(rc, *jsonFlag)
	}
	s := dblifecycle.Service{}
	if catOp != "" {
		if *requestID != "" {
			return failure(o, fmt.Errorf("%w: --request-id deduplication is an owner-session guarantee; pass --session", dbstore.ErrUnsupported)).emit(rc, *jsonFlag)
		}
		var out dblifecycle.Outcome
		_, out, err = s.RunCategory(ctx, id.Target, catOp, raw, *yes, *wait)
		if err != nil {
			return failure(o, err).emit(rc, *jsonFlag)
		}
		o.Result, o.Page, o.Warnings = out.Result, out.Page, out.Warnings
		return o.emit(rc, *jsonFlag)
	}
	switch op {
	case "create":
		_, err = s.CreateConfigured(ctx, id.Target, raw, *wait)
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

func envValue(env []string, key string) string {
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, key+"="); ok {
			return value
		}
	}
	return ""
}

func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, pair := range env {
		if !strings.HasPrefix(pair, key+"=") {
			out = append(out, pair)
		}
	}
	return append(out, key+"="+value)
}

func holdChildEnv(env []string, holderEpisode, session, target string, epoch uint64) []string {
	// Call may have minted BASHY_EPISODE after RunContext.Env was captured.
	// Carry the exact holder episode into the child so owner admission sees
	// the same claimant as the hold, including in a bare CLI process.
	env = withEnv(env, coord.EpisodeEnv, holderEpisode)
	env = withEnv(env, "BASHY_DB_HOLD_SESSION", session)
	env = withEnv(env, "BASHY_DB_HOLD_TARGET", target)
	return withEnv(env, "BASHY_DB_HOLD_EPOCH", strconv.FormatUint(epoch, 10))
}

func holdChild(ctx context.Context, rc *tool.RunContext, o output, target dbstore.Target, session string, child []string, wait, timeout time.Duration, asJSON bool) int {
	if len(child) == 0 || child[0] == "" {
		return failure(o, fmt.Errorf("%w: hold requires a command after --", dbstore.ErrInvalidInput)).emit(rc, asJSON)
	}
	owned := false
	if session == "" {
		info, err := startSession(ctx, rc, 0)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		session, owned = info.ID, true
	}
	if owned {
		defer func() {
			_, _ = dblifecycle.Call(context.Background(), session, dblifecycle.Request{Op: "stop", Yes: true})
		}()
	}
	holder := coord.Self()
	hold, err := dblifecycle.Call(ctx, session, dblifecycle.Request{Op: "hold", Target: target, Wait: wait, Holder: holder})
	if err != nil {
		return failure(o, err).emit(rc, asJSON)
	}
	if !hold.OK {
		return ownerOutput(o, hold).emit(rc, asJSON)
	}
	result, ok := hold.Result.(map[string]any)
	if !ok {
		return failure(o, fmt.Errorf("%w: owner hold response", dbstore.ErrBackend)).emit(rc, asJSON)
	}
	epoch, ok := result["epoch"].(json.Number)
	value, err := strconv.ParseUint(epoch.String(), 10, 64)
	if !ok || err != nil {
		return failure(o, fmt.Errorf("%w: owner hold epoch", dbstore.ErrBackend)).emit(rc, asJSON)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, _ = dblifecycle.Call(releaseCtx, session, dblifecycle.Request{Op: "release", Target: target, Epoch: value})
	}()
	id, err := dbstore.Resolve(target)
	if err != nil {
		return failure(o, err).emit(rc, asJSON)
	}
	cmd := exec.CommandContext(ctx, child[0], child[1:]...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = rc.Dir, rc.In, rc.Out, rc.Err
	cmd.Env = holdChildEnv(rc.Env, holder.Episode, session, id.ID, value)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return failure(o, err).emit(rc, asJSON)
	}
	return 0
}

func targetOperand(raw string) (dbstore.Target, error) {
	if !strings.HasPrefix(raw, "db:") {
		r, ok := fleet.New().Resource(raw)
		if !ok || r.Kind != "db" || len(r.Members) != 1 {
			return dbstore.Target{}, fmt.Errorf("%w: registered database %q", dbstore.ErrNotFound, raw)
		}
		canonical := r.Members[0]
		if !strings.HasPrefix(canonical, "db:") {
			return dbstore.Target{}, fmt.Errorf("%w: invalid registered database identity", dbstore.ErrInvalidInput)
		}
		raw = (ref.Locator{Scheme: "db", Target: strings.TrimPrefix(canonical, "db:")}).String()
	}
	return dblifecycle.ParseTarget(raw)
}

func ownerOutput(o output, res dblifecycle.Response) output {
	o.SchemaVersion, o.OK, o.Op, o.Target, o.MutationClass, o.Effects, o.Result = res.SchemaVersion, res.OK, res.Op, res.Target, res.MutationClass, res.Effects, res.Result
	o.Page, o.Warnings = res.Page, res.Warnings
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
		info, err := startSession(ctx, rc, ttl)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		o.Result = info
		return o.emit(rc, asJSON)
	case "list":
		rows, err := dblifecycle.ListSessionsContext(ctx)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		o.Result = rows
		return o.emit(rc, asJSON)
	case "show":
		info, err := dblifecycle.ReadSessionContext(ctx, id)
		if err != nil {
			return failure(o, err).emit(rc, asJSON)
		}
		if info.State == "running" {
			status, callErr := dblifecycle.Call(ctx, id, dblifecycle.Request{Op: "status"})
			if callErr != nil {
				return failure(o, callErr).emit(rc, asJSON)
			}
			if status.OK {
				o.Result = map[string]any{"info": info, "status": status.Result}
				return o.emit(rc, asJSON)
			}
			return ownerOutput(o, status).emit(rc, asJSON)
		}
		return failure(o, fmt.Errorf("%w: session %s", dbstore.ErrLifetimeEnded, id)).emit(rc, asJSON)
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

func startSession(ctx context.Context, rc *tool.RunContext, ttl time.Duration) (dblifecycle.SessionInfo, error) {
	ready, err := os.CreateTemp("", "bashy-db-ready-*")
	if err != nil {
		return dblifecycle.SessionInfo{}, err
	}
	readyPath := ready.Name()
	_ = ready.Close()
	defer os.Remove(readyPath)
	exe, err := os.Executable()
	if err != nil {
		return dblifecycle.SessionInfo{}, err
	}
	cmd := exec.Command(exe, "db", "_serve", "--ready", readyPath, "--idle-ttl", ttl.String())
	cmd.Env, cmd.Dir = rc.Env, rc.Dir
	if err := cmd.Start(); err != nil {
		return dblifecycle.SessionInfo{}, err
	}
	readyOwner := false
	defer func() {
		if !readyOwner {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var id string
	for id == "" {
		b, err := os.ReadFile(readyPath)
		if err == nil && len(b) > 0 {
			id = string(b)
			break
		}
		select {
		case <-ctx.Done():
			return dblifecycle.SessionInfo{}, dbstore.FromContext(ctx.Err())
		case <-ticker.C:
		}
	}
	info, err := dblifecycle.ReadSessionContext(ctx, id)
	if err != nil {
		return dblifecycle.SessionInfo{}, err
	}
	if info.State != "running" {
		return dblifecycle.SessionInfo{}, fmt.Errorf("%w: owner exited before rendezvous", dbstore.ErrLifetimeEnded)
	}
	readyOwner = true
	_ = cmd.Process.Release()
	return info, nil
}

func list(ctx context.Context, rc *tool.RunContext, o output, asJSON bool) int {
	rows, errs := fleet.New().Resources()
	if len(errs) > 0 {
		return failure(o, errs[0]).emit(rc, asJSON)
	}
	var dbs []any
	for _, r := range rows {
		if r.Kind == "db" {
			locator := ""
			if len(r.Members) == 1 {
				locator = locatorForIdentity(r.Members[0])
			}
			dbs = append(dbs, map[string]any{"schema_version": "bashy-node-v1", "kind": "db", "name": r.Name, "aliases": r.Aliases, "target": r.Members, "locator": locator})
			if len(dbs) > 256 {
				return failure(o, fmt.Errorf("%w: db list exceeds 256 entries", dbstore.ErrInvalidInput)).emit(rc, asJSON)
			}
		}
	}
	sessions, err := dblifecycle.ListSessionsContext(ctx)
	if err != nil {
		return failure(o, err).emit(rc, asJSON)
	}
	for _, session := range sessions {
		if session.State != "running" {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		res, err := dblifecycle.Call(requestCtx, session.ID, dblifecycle.Request{Op: "status"})
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
			dbs = append(dbs, map[string]any{"schema_version": "bashy-node-v1", "kind": "db", "id": id, "locator": locatorForIdentity(id), "session": session.ID})
			if len(dbs) > 256 {
				return failure(o, fmt.Errorf("%w: db list exceeds 256 entries", dbstore.ErrInvalidInput)).emit(rc, asJSON)
			}
		}
	}
	o.Result = dbs
	return o.emit(rc, asJSON)
}

func locatorForIdentity(id string) string {
	if !strings.HasPrefix(id, "db:") {
		return ""
	}
	return (ref.Locator{Scheme: "db", Target: strings.TrimPrefix(id, "db:")}).String()
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

// sqlVerb splits `db sql query|exec|schema show|schema apply` (schema-show
// and schema-apply are the one-word spellings typed MCP uses) from the
// remaining operands. export and import pick their category from the target.
func sqlVerb(op string, operands []string) (string, []string, error) {
	if op != "sql" {
		return "", operands, nil
	}
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db sql query|exec|schema show|schema apply TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if sub == "schema" && len(rest) > 0 {
		sub, rest = "schema."+rest[0], rest[1:]
	} else if v, ok := strings.CutPrefix(sub, "schema-"); ok {
		sub = "schema." + v
	}
	switch sub {
	case "query", "exec", "schema.show", "schema.apply":
		return "sql." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: db sql %s (want query, exec, schema show or schema apply)", dbstore.ErrInvalidInput, operands[0])
}

// categoryInput builds the one typed input document from --input and the
// flags. A value given twice is an error, never a silent override.
func categoryInput(rc *tool.RunContext, f dbFlags, op string) (json.RawMessage, error) {
	var in dbsql.Input
	if *f.input != "" {
		var r io.Reader = rc.In
		if *f.input != "-" {
			file, err := os.Open(resolvePath(rc, *f.input))
			if err != nil {
				return nil, fmt.Errorf("%w: --input: %v", dbstore.ErrInvalidInput, err)
			}
			defer file.Close()
			r = file
		}
		if r == nil {
			return nil, fmt.Errorf("%w: --input - needs stdin", dbstore.ErrInvalidInput)
		}
		b, err := io.ReadAll(io.LimitReader(r, maxInput+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxInput {
			return nil, fmt.Errorf("%w: --input is over %d bytes", dbstore.ErrLimitExceeded, maxInput)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("%w: --input: %v", dbstore.ErrInvalidInput, err)
		}
	}
	twice := func(name string, set bool) error {
		if set {
			return fmt.Errorf("%w: %s given in --input and as a flag", dbstore.ErrInvalidInput, name)
		}
		return nil
	}
	if *f.sqlText != "" {
		if err := twice("sql", in.SQL != ""); err != nil {
			return nil, err
		}
		in.SQL = *f.sqlText
	}
	if len(*f.param) > 0 || *f.params != "" {
		if err := twice("params", len(in.Params) != 0); err != nil {
			return nil, err
		}
		if len(*f.param) > 0 && *f.params != "" {
			return nil, fmt.Errorf("%w: use --param or --params, not both", dbstore.ErrInvalidInput)
		}
		if *f.params != "" {
			in.Params = json.RawMessage(*f.params)
		} else {
			named, positional := map[string]json.RawMessage{}, []json.RawMessage{}
			for _, p := range *f.param {
				name, value, err := dbsql.ParseParam(p)
				if err != nil {
					return nil, err
				}
				if name == "" {
					positional = append(positional, value)
				} else if _, dup := named[name]; dup {
					return nil, fmt.Errorf("%w: --param %s given twice", dbstore.ErrInvalidInput, name)
				} else {
					named[name] = value
				}
			}
			if len(named) > 0 && len(positional) > 0 {
				return nil, fmt.Errorf("%w: mix of named and positional --param", dbstore.ErrInvalidInput)
			}
			var b []byte
			if len(named) > 0 {
				b, _ = json.Marshal(named)
			} else {
				b, _ = json.Marshal(positional)
			}
			in.Params = b
		}
	}
	for _, v := range []struct {
		name string
		flag int
		dst  *int
	}{{"limit", *f.limit, &in.Limit}, {"max_bytes", *f.maxBytes, &in.MaxBytes}} {
		if v.flag != 0 {
			if err := twice(v.name, *v.dst != 0); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	for _, v := range []struct {
		name, flag string
		dst        *string
	}{{"since", *f.since, &in.Since}, {"on_conflict", *f.onConflict, &in.OnConflict}} {
		if v.flag != "" {
			if err := twice(v.name, *v.dst != ""); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	// --out/--in/--replace mean the export directory and import mode here.
	if op == "export" && *f.backup != "" {
		in.Out = *f.backup
	}
	if op == "import" {
		if *f.source != "" {
			in.In = *f.source
		}
		in.Replace = in.Replace || *f.replace
	}
	if in.Out != "" && in.Out != "-" {
		in.Out = resolvePath(rc, in.Out)
	}
	if in.In != "" && in.In != "-" {
		in.In = resolvePath(rc, in.In)
	}
	return json.Marshal(in)
}

// maxInput keeps a typed input inside one owner request.
const maxInput = 48 << 10

func resolvePath(rc *tool.RunContext, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(rc.Dir, p)
	}
	return filepath.Clean(p)
}
