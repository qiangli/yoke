package dataclients

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// Bounds. Defaults are finite; caps are the most any caller may request.
const (
	DefaultMaxRows  = 1000
	CapMaxRows      = 100000
	DefaultMaxBytes = 1 << 20
	CapMaxBytes     = 16 << 20
	DefaultTimeout  = 30 * time.Second
	CapTimeout      = 10 * time.Minute
	MaxStatement    = 1 << 20
	maxStderr       = 64 << 10
)

// Op is one bounded operation against an explicit endpoint. The statement is
// sent on the child's stdin, never argv. A write executes inside a
// transaction that is rolled back (a provider-validated preview) unless
// Commit is set; Commit without Write is refused.
type Op struct {
	Statement string
	Write     bool
	Commit    bool
	MaxRows   int
	MaxBytes  int64
	Timeout   time.Duration
}

// Invocation is a fully built, not-yet-run child: argv without secrets,
// environment additions (secrets travel here), and the stdin payload.
type Invocation struct {
	Client    string
	Bin       string
	Args      []string
	Env       []string // additions; ambient client variables are scrubbed first
	Stdin     string
	Op        Op
	NullToken string // psql: the per-invocation NULL sentinel
	Secret    string // for redaction only
}

// ambientPrefixes are client variables scrubbed from the inherited env so a
// shell-level PGHOST/REDISCLI_AUTH can never override or supply the explicit
// selection.
var ambientPrefixes = map[string][]string{
	"psql":       {"PG"},
	"mongosh":    {"MONGODB", "MONGOSH", "DC_"},
	"valkey-cli": {"REDISCLI", "VALKEYCLI", "VALKEY"},
	"snow":       {"SNOWFLAKE", "SNOW"},
}

func (o Op) normalized() (Op, *gws.Error) {
	if strings.TrimSpace(o.Statement) == "" {
		return o, gws.NewError(gws.CodeInvalidInput, "data client: empty statement — missing required input fails promptly", nil)
	}
	if len(o.Statement) > MaxStatement {
		return o, gws.NewError(gws.CodeOutputLimit, fmt.Sprintf("data client: statement exceeds the %d-byte input limit", MaxStatement), nil)
	}
	if o.Commit && !o.Write {
		return o, gws.NewError(gws.CodeInvalidInput, "data client: commit requires write; a read never commits", nil)
	}
	if o.MaxRows < 0 || o.MaxRows > CapMaxRows || o.MaxBytes < 0 || o.MaxBytes > CapMaxBytes || o.Timeout < 0 || o.Timeout > CapTimeout {
		return o, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("data client: bounds over cap (rows<=%d bytes<=%d timeout<=%s)", CapMaxRows, CapMaxBytes, CapTimeout), nil)
	}
	if o.MaxRows == 0 {
		o.MaxRows = DefaultMaxRows
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	return o, nil
}

// Build validates the whole operation locally (no network, no effects) and
// returns the invocation. Unsupported combinations are refused here, before
// any provider execution, identically for CLI and MCP callers.
func Build(ep Endpoint, op Op, bin string) (Invocation, *gws.Error) {
	c, ok := Lookup(ep.Client)
	if !ok {
		return Invocation{}, gws.NewError(gws.CodeUnsupported, ep.Client+": not an admitted data client", nil)
	}
	op, err := op.normalized()
	if err != nil {
		return Invocation{}, err.WithOp("query", ep.Client)
	}
	inv := Invocation{Client: ep.Client, Bin: bin, Op: op, Secret: ep.Password}
	var e *gws.Error
	switch ep.Client {
	case "psql":
		e = buildPsql(&inv, ep, op)
	case "mongosh":
		buildMongosh(&inv, ep, op)
	case "valkey-cli":
		e = buildValkey(&inv, ep, op)
	case "snow":
		buildSnow(&inv, ep, op)
	}
	if e != nil {
		return Invocation{}, e.WithOp("query", c.Name)
	}
	return inv, nil
}

func txWrap(op Op, begin, commit, rollback string) string {
	end := rollback
	if op.Commit {
		end = commit
	}
	return begin + "\n" + strings.TrimSpace(op.Statement) + "\n" + end + "\n"
}

func buildPsql(inv *Invocation, ep Endpoint, op Op) *gws.Error {
	for _, line := range strings.Split(op.Statement, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) {
			return gws.NewError(gws.CodeUnsupported, `psql: backslash meta-commands (\!, \copy, \o …) are not admitted in a typed statement — SQL only`, nil)
		}
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	inv.NullToken = "NULL-" + hex.EncodeToString(nonce)
	inv.Args = []string{"-X", "-q", "-w", "-v", "ON_ERROR_STOP=1",
		"-h", ep.Host, "-p", strconv.Itoa(ep.Port), "-U", ep.User, "-d", ep.Database,
		"-A", "-F", "\x1f", "-R", "\x1e", "-P", "null=" + inv.NullToken, "-P", "footer=off"}
	opts := fmt.Sprintf("-c statement_timeout=%d", op.Timeout.Milliseconds())
	if !op.Write {
		opts += " -c default_transaction_read_only=on"
		inv.Stdin = "BEGIN READ ONLY;\n" + strings.TrimSpace(op.Statement) + "\nROLLBACK;\n"
	} else {
		inv.Stdin = txWrap(op, "BEGIN;", "COMMIT;", "ROLLBACK;")
	}
	inv.Env = []string{"PGPASSWORD=" + ep.Password, "PGOPTIONS=" + opts, "PGCONNECT_TIMEOUT=10", "PGSSLMODE=" + map[TLSMode]string{TLSVerify: "verify-full", TLSPlaintextLoopback: "disable"}[ep.TLS]}
	if ep.CAFile != "" && ep.TLS == TLSVerify {
		inv.Env = append(inv.Env, "PGSSLROOTCERT="+ep.CAFile)
	}
	return nil
}

func buildMongosh(inv *Invocation, ep Endpoint, op Op) {
	q := url.Values{}
	q.Set("tls", strconv.FormatBool(ep.TLS == TLSVerify))
	q.Set("serverSelectionTimeoutMS", "10000")
	if ep.CAFile != "" && ep.TLS == TLSVerify {
		q.Set("tlsCAFile", ep.CAFile)
	}
	uri := "mongodb://" + net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)) + "/?" + q.Encode()
	db, _ := json.Marshal(ep.Database)
	end := "await s.abortTransaction();"
	if op.Commit {
		end = "await s.commitTransaction();"
	}
	txStart, txEnd := "", ""
	if op.Write {
		txStart, txEnd = "s.startTransaction();", end
	}
	// The statement is the operator's own JavaScript expression evaluated
	// against the session-bound database; credentials come from env only.
	inv.Stdin = fmt.Sprintf(`const conn = connect(process.env.DC_URI, process.env.DC_USER, process.env.DC_PASSWORD);
const s = conn.getMongo().startSession();
%s
try {
  const r = (function(db){ return (%s); })(s.getDatabase(%s));
  const docs = (r && typeof r.toArray === 'function') ? r.limit(%d).toArray() : [r];
  for (const d of docs) print(EJSON.stringify(d, {relaxed:false}));
  %s
} catch (e) {
  try { await s.abortTransaction(); } catch (_) {}
  throw e;
} finally { s.endSession(); }
`, txStart, strings.TrimSpace(op.Statement), db, op.MaxRows+1, txEnd)
	inv.Args = []string{"--nodb", "--quiet", "--norc"}
	inv.Env = []string{"DC_URI=" + uri, "DC_USER=" + ep.User, "DC_PASSWORD=" + ep.Password}
}

var valkeyRead = setOf("GET", "MGET", "STRLEN", "EXISTS", "TYPE", "TTL", "PTTL", "HGET", "HMGET", "HGETALL", "HLEN", "HEXISTS", "LRANGE", "LLEN", "LINDEX", "SMEMBERS", "SCARD", "SISMEMBER", "ZRANGE", "ZCARD", "ZSCORE", "ZRANK", "SCAN", "HSCAN", "SSCAN", "ZSCAN", "DBSIZE", "PING")
var valkeyDenied = setOf("MONITOR", "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "SYNC", "PSYNC", "SHUTDOWN", "FLUSHALL", "FLUSHDB", "CONFIG", "REPLICAOF", "SLAVEOF", "DEBUG", "MIGRATE", "MULTI", "EXEC", "DISCARD", "WATCH", "BLPOP", "BRPOP", "BLMOVE", "BZPOPMIN", "BZPOPMAX", "XREAD", "XREADGROUP", "WAIT", "SCRIPT", "EVAL", "EVALSHA", "FUNCTION", "CLIENT", "ACL")

func setOf(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

func buildValkey(inv *Invocation, ep Endpoint, op Op) *gws.Error {
	var cmds []string
	for _, line := range strings.Split(op.Statement, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := strings.ToUpper(strings.Fields(line)[0])
		switch {
		case valkeyDenied[name]:
			return gws.NewError(gws.CodeUnsupported, "valkey-cli: "+name+" is not admitted (long-lived, blocking, server-administering or transaction-control command; transactions are managed by the client wrapper)", nil)
		case !op.Write && !valkeyRead[name]:
			return gws.NewError(gws.CodeUnsupported, "valkey-cli: "+name+" is not a known read command; declare write to run it (rolled back unless commit)", nil)
		}
		cmds = append(cmds, line)
	}
	if len(cmds) == 0 {
		return gws.NewError(gws.CodeInvalidInput, "valkey-cli: no command in statement", nil)
	}
	inv.Args = []string{"-h", ep.Host, "-p", strconv.Itoa(ep.Port), "--quoted-json", "--no-auth-warning"}
	if ep.User != "" {
		inv.Args = append(inv.Args, "--user", ep.User)
	}
	if ep.TLS == TLSVerify {
		inv.Args = append(inv.Args, "--tls")
		if ep.CAFile != "" {
			inv.Args = append(inv.Args, "--cacert", ep.CAFile)
		}
	}
	if op.Write {
		inv.Stdin = "MULTI\n" + strings.Join(cmds, "\n") + "\n" + map[bool]string{true: "EXEC", false: "DISCARD"}[op.Commit] + "\n"
	} else {
		inv.Stdin = strings.Join(cmds, "\n") + "\n"
	}
	inv.Env = []string{"REDISCLI_AUTH=" + ep.Password}
	return nil
}

func buildSnow(inv *Invocation, ep Endpoint, op Op) {
	inv.Args = []string{"sql", "-i", "--format", "json", "--temporary-connection", "--account", ep.Host, "--user", ep.User}
	if ep.Database != "" {
		inv.Args = append(inv.Args, "--database", ep.Database)
	}
	if op.Write {
		inv.Stdin = txWrap(op, "BEGIN;", "COMMIT;", "ROLLBACK;")
	} else {
		inv.Stdin = "BEGIN;\n" + strings.TrimSpace(op.Statement) + "\nROLLBACK;\n"
	}
	inv.Env = []string{"SNOWFLAKE_PASSWORD=" + ep.Password}
}

// Explain describes what would run without executing: argv (secret-masked),
// the redacted scope, and the declared effects. It never touches the
// network. It is a local preview, not execution evidence.
func (inv Invocation) Explain(ep Endpoint) map[string]any {
	argv, _ := gws.RedactArgs(inv.Args, inv.Secret)
	mut := "none (read-only transaction, rolled back)"
	if inv.Op.Write {
		mut = "rolled back after execution (preview)"
		if inv.Op.Commit {
			mut = "COMMITTED on success"
		}
	}
	return map[string]any{
		"preview":   "local",
		"client":    inv.Client,
		"argv":      argv,
		"scope":     ep.RedactedScope(),
		"mutation":  mut,
		"effects":   "net.egress(" + ep.Host + ") child.exec(" + inv.Client + ")",
		"bounds":    map[string]any{"max_rows": inv.Op.MaxRows, "max_bytes": inv.Op.MaxBytes, "timeout": inv.Op.Timeout.String()},
		"fidelity":  clients[inv.Client].Fidelity,
		"statement": gws.RedactText(strings.TrimSpace(inv.Op.Statement), inv.Secret),
	}
}
