// Package dbts is the Sprint 453 time-series and analytical data category
// (story 01b029625a5e) over the sqlitedb adapter: `db ts ingest|range|agg|
// retain`, the time-series logical interchange `db ts export|import`, and
// bounded VictoriaMetrics endpoint tools `db ts remote query|ingest`.
//
// A ts database holds named series of timestamped float samples in two
// tables the package manages itself (bashy_ts_series, bashy_ts_points); a
// series is registered by its first ingest, never earlier, and SQLite is
// described as SQL analytics, not full OLAP. Ingest, range and agg address
// one exact series (name plus full label set); retain addresses every
// series matching a name and label subset. Every operation registers with
// dblifecycle, so CLI, MCP and owner sessions reach it through one claim
// guard, owner admission, request-id replay and hold-epoch check. Bashy
// writes all SQL itself: every series name, label, timestamp and value is a
// bound parameter, never interpolated text. Reads issue only fixed SELECTs,
// so a read cannot mutate even on a held read-write handle. Schema changes
// (import --replace) and retention deletes are exclusive.
//
// Remote endpoints are explicit per request: the input names the endpoint,
// there is no default, no environment fallback and nothing is provisioned or
// started. The lifecycle target a remote op is admitted through is only the
// claim-guard anchor; the endpoint in the input is the data-plane target.
package dbts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// Operation names, as registered with dblifecycle.
const (
	OpIngest       = "ts.ingest"
	OpRange        = "ts.range"
	OpAgg          = "ts.agg"
	OpRetain       = "ts.retain"
	OpExport       = "ts.export"
	OpImport       = "ts.import"
	OpRemoteQuery  = "ts.remote.query"
	OpRemoteIngest = "ts.remote.ingest"
)

// Bounds declared for the ts category (contract §6).
const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10
	MaxMaxBytes     = 8 << 20
	MaxBatchPoints  = 10000
	MaxLabels       = 32
	MaxLabelBytes   = 1024
	MaxSeriesName   = 255
)

// Point is one timestamped sample in typed input or output. Ts and Value
// are contract §8.3 tagged values: ts is {"t":"ts","v":RFC3339,"unit"} or
// {"t":"int","v":ns}, value is {"t":"f64","v":N} or {"t":"int","v":N}.
type Point struct {
	Ts    json.RawMessage `json:"ts"`
	Value json.RawMessage `json:"value"`
}

// Series is one remote series for ts.remote.ingest.
type Series struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Points []Point           `json:"points,omitempty"`
}

// Input is the typed input every ts operation accepts, from CLI flags, an
// --input JSON document or the MCP/owner request.
type Input struct {
	Series     string            `json:"series,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Points     []Point           `json:"points,omitempty"`
	Start      json.RawMessage   `json:"start,omitempty"`
	End        json.RawMessage   `json:"end,omitempty"`
	Bucket     string            `json:"bucket,omitempty"`
	Funcs      []string          `json:"funcs,omitempty"`
	Retention  string            `json:"retention,omitempty"`
	OnConflict string            `json:"on_conflict,omitempty"`
	Replace    bool              `json:"replace,omitempty"`
	Limit      int               `json:"limit,omitempty"`
	MaxBytes   int               `json:"max_bytes,omitempty"`
	Since      string            `json:"since,omitempty"`
	Out        string            `json:"out,omitempty"`
	In         string            `json:"in,omitempty"`
	Endpoint   string            `json:"endpoint,omitempty"`
	Expr       string            `json:"expr,omitempty"`
	Step       string            `json:"step,omitempty"`
	Timeout    string            `json:"timeout,omitempty"`
	SeriesList []Series          `json:"series_list,omitempty"`
}

// DecodeInput strictly decodes a typed input document.
func DecodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: ts input: %v", dbstore.ErrInvalidInput, err)
	}
	if dec.More() {
		return in, fmt.Errorf("%w: ts input has trailing data", dbstore.ErrInvalidInput)
	}
	return in, nil
}

func (in Input) bounds() (limit, maxBytes int, err error) {
	limit, maxBytes = in.Limit, in.MaxBytes
	if limit == 0 {
		limit = DefaultLimit
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	if limit < 0 || limit > MaxLimit || maxBytes < 0 || maxBytes > MaxMaxBytes {
		return 0, 0, fmt.Errorf("%w: --limit 1..%d and --max-bytes 1..%d", dbstore.ErrInvalidInput, MaxLimit, MaxMaxBytes)
	}
	return limit, maxBytes, nil
}

// fields lists the input fields an operation accepts; any other set field
// is invalid_input, never silently ignored.
func (in Input) only(op string, allowed ...string) error {
	set := map[string]bool{
		"series": len(in.Series) != 0, "labels": len(in.Labels) != 0, "points": len(in.Points) != 0,
		"start": len(in.Start) != 0, "end": len(in.End) != 0, "bucket": len(in.Bucket) != 0,
		"funcs": len(in.Funcs) != 0, "retention": len(in.Retention) != 0,
		"on_conflict": len(in.OnConflict) != 0, "replace": in.Replace,
		"limit": in.Limit != 0, "max_bytes": in.MaxBytes != 0, "since": len(in.Since) != 0,
		"out": len(in.Out) != 0, "in": len(in.In) != 0,
		"endpoint": len(in.Endpoint) != 0, "expr": len(in.Expr) != 0, "step": len(in.Step) != 0,
		"timeout": len(in.Timeout) != 0, "series_list": len(in.SeriesList) != 0,
	}
	for _, a := range allowed {
		delete(set, a)
	}
	for name, on := range set {
		if on {
			return fmt.Errorf("%w: %s does not take %s", dbstore.ErrInvalidInput, op, name)
		}
	}
	return nil
}

var seriesNameRE = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_:]*$`)
var labelKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func checkSeriesName(name string) error {
	if name == "" || len(name) > MaxSeriesName || !seriesNameRE.MatchString(name) {
		return fmt.Errorf("%w: series name %q (want [A-Za-z_:][A-Za-z0-9_:]{0,254})", dbstore.ErrInvalidInput, name)
	}
	return nil
}

// canonicalLabels validates a label set and renders it in canonical
// (sorted-key) JSON, which is the stored and compared form.
func canonicalLabels(labels map[string]string) (string, error) {
	if len(labels) > MaxLabels {
		return "", fmt.Errorf("%w: at most %d labels per series", dbstore.ErrInvalidInput, MaxLabels)
	}
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return "", fmt.Errorf("%w: label key %q (want [A-Za-z_][A-Za-z0-9_]*)", dbstore.ErrInvalidInput, k)
		}
		if !utf8.ValidString(v) || len(v) > MaxLabelBytes {
			return "", fmt.Errorf("%w: label %q value must be UTF-8 text of at most %d bytes", dbstore.ErrInvalidInput, k, MaxLabelBytes)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(labels[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String(), nil
}

// labelClauses renders exact-match label predicates over the canonical
// labels JSON. Keys are path-safe by validation, values are bound.
func labelClauses(labelsJSON string, args *[]any) (string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(labelsJSON), &m); err != nil {
		return "", fmt.Errorf("%w: labels: %v", dbstore.ErrInvalidInput, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, ` and json_extract(s.labels, '$.%s') = ?`, k)
		*args = append(*args, m[k])
	}
	return sb.String(), nil
}

// parseDuration parses a bounded duration string with unit
// ns|us|ms|s|m|h|d|w into nanoseconds, refusing overflow.
func parseDuration(field, s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: %s needs a duration like 30s, 15m, 2h, 7d", dbstore.ErrInvalidInput, field)
	}
	var num, unit string
	for i, r := range s {
		if r < '0' || r > '9' {
			num, unit = s[:i], s[i:]
			break
		}
	}
	if num == "" {
		num, unit = s, "s"
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: %s %q must be a positive duration", dbstore.ErrInvalidInput, field, s)
	}
	var mult int64
	switch unit {
	case "ns":
		mult = 1
	case "us":
		mult = 1000
	case "ms":
		mult = 1000000
	case "s":
		mult = 1000000000
	case "m":
		mult = 60 * 1000000000
	case "h":
		mult = 3600 * 1000000000
	case "d":
		mult = 24 * 3600 * 1000000000
	case "w":
		mult = 7 * 24 * 3600 * 1000000000
	default:
		return 0, fmt.Errorf("%w: %s %q unit (ns, us, ms, s, m, h, d, w)", dbstore.ErrInvalidInput, field, s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%w: %s %q overflows", dbstore.ErrInvalidInput, field, s)
	}
	return n * mult, nil
}

// parseTS decodes a tagged timestamp into int64 nanoseconds since the Unix
// epoch. int is raw nanoseconds; ts converts its unit to nanoseconds.
func parseTS(field string, raw json.RawMessage) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("%w: %s needs a timestamp", dbstore.ErrInvalidInput, field)
	}
	var t struct {
		T    string `json:"t"`
		V    string `json:"v"`
		Unit string `json:"unit"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return 0, fmt.Errorf("%w: %s timestamp: %v", dbstore.ErrInvalidInput, field, err)
	}
	switch t.T {
	case "int":
		n, err := strconv.ParseInt(t.V, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %s int timestamp %q", dbstore.ErrInvalidInput, field, t.V)
		}
		return n, nil
	case "ts":
		tm, err := time.Parse(time.RFC3339, t.V)
		if err != nil {
			return 0, fmt.Errorf("%w: %s ts %q is not RFC 3339", dbstore.ErrInvalidInput, field, t.V)
		}
		unit := t.Unit
		if unit == "" {
			unit = "ns"
		}
		sec := tm.Unix()
		var mult int64
		switch unit {
		case "s":
			mult = 1e9
		case "ms":
			mult = 1e6
		case "us":
			mult = 1e3
		case "ns":
			mult = 1
		default:
			return 0, fmt.Errorf("%w: %s timestamp unit %q (s, ms, us, ns)", dbstore.ErrInvalidInput, field, unit)
		}
		sub := int64(tm.Nanosecond())
		if sec > 0 && sec > (math.MaxInt64-sub)/1e9 {
			return 0, fmt.Errorf("%w: %s timestamp %q overflows nanoseconds", dbstore.ErrInvalidInput, field, t.V)
		}
		if sec < 0 && sec < (math.MinInt64-sub)/1e9 {
			return 0, fmt.Errorf("%w: %s timestamp %q overflows nanoseconds", dbstore.ErrInvalidInput, field, t.V)
		}
		total := sec*1e9 + sub
		q, r := total/mult, total%mult
		if r != 0 && total < 0 {
			q-- // floor, so the unit still contains the instant
		}
		return q * mult, nil
	}
	return 0, fmt.Errorf("%w: %s timestamp tag %q (int or ts)", dbstore.ErrInvalidInput, field, t.T)
}

// encodeTS renders nanoseconds as a contract §8.3 ts value in UTC.
func encodeTS(ns int64) map[string]any {
	return map[string]any{"t": "ts", "v": time.Unix(0, ns).UTC().Format(time.RFC3339Nano), "unit": "ns"}
}

// parseValue decodes a tagged float sample. int converts exactly.
func parseValue(field string, raw json.RawMessage) (float64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("%w: %s needs a value", dbstore.ErrInvalidInput, field)
	}
	var t struct {
		T string          `json:"t"`
		V json.RawMessage `json:"v"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return 0, fmt.Errorf("%w: %s value: %v", dbstore.ErrInvalidInput, field, err)
	}
	switch t.T {
	case "int":
		var s string
		if err := json.Unmarshal(t.V, &s); err != nil {
			return 0, fmt.Errorf("%w: %s int value must be a decimal string", dbstore.ErrInvalidInput, field)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %s int value %q", dbstore.ErrInvalidInput, field, s)
		}
		// Samples are REALs: an int outside float-exact range would round
		// silently, so it is refused instead of coerced.
		if n > 1<<53-1 || n < -(1<<53-1) {
			return 0, fmt.Errorf("%w: %s int value %q exceeds float-exact range ±%d", dbstore.ErrInvalidInput, field, s, 1<<53-1)
		}
		return float64(n), nil
	case "f64":
		var s string
		if json.Unmarshal(t.V, &s) == nil {
			switch s {
			case "NaN":
				return 0, fmt.Errorf("%w: %s NaN is not stored: the driver binds it as NULL", dbstore.ErrInvalidInput, field)
			case "Inf":
				return math.Inf(1), nil
			case "-Inf":
				return math.Inf(-1), nil
			}
			return 0, fmt.Errorf("%w: %s f64 string must be NaN, Inf or -Inf", dbstore.ErrInvalidInput, field)
		}
		var n json.Number
		d := json.NewDecoder(bytes.NewReader(t.V))
		d.UseNumber()
		if err := d.Decode(&n); err != nil {
			return 0, fmt.Errorf("%w: %s f64 value must be a JSON number", dbstore.ErrInvalidInput, field)
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %s f64 %s: %v", dbstore.ErrInvalidInput, field, n, err)
		}
		if math.IsNaN(f) {
			return 0, fmt.Errorf("%w: %s NaN is not stored: the driver binds it as NULL", dbstore.ErrInvalidInput, field)
		}
		return f, nil
	}
	return 0, fmt.Errorf("%w: %s value tag %q (int or f64); untagged numbers are refused", dbstore.ErrInvalidInput, field, t.T)
}

// encodeValue renders a REAL sample as a contract §8.3 f64 value.
func encodeValue(f float64) map[string]any {
	switch {
	case math.IsNaN(f):
		return map[string]any{"t": "f64", "v": "NaN"}
	case math.IsInf(f, 1):
		return map[string]any{"t": "f64", "v": "Inf"}
	case math.IsInf(f, -1):
		return map[string]any{"t": "f64", "v": "-Inf"}
	}
	return map[string]any{"t": "f64", "v": f}
}

func handleDB(h dbstore.Handle) (*sql.DB, error) {
	db, ok := h.(*sqlitedb.DB)
	if !ok {
		return nil, fmt.Errorf("%w: ts category needs the sqlite engine", dbstore.ErrUnsupported)
	}
	return db.SQL(), nil
}

// ts tables, managed by this package. Series are registered by the first
// ingest that names them, never earlier.
const (
	tableSeries = "bashy_ts_series"
	tablePoints = "bashy_ts_points"
)

const schemaSQL = `create table if not exists bashy_ts_series(
 id integer primary key,
 name text not null,
 labels text not null default '{}',
 retention_ns integer,
 unique(name, labels)
);
create table if not exists bashy_ts_points(
 series_id integer not null references bashy_ts_series(id) on delete cascade,
 ts integer not null,
 value real not null,
 primary key(series_id, ts)
);
create index if not exists bashy_ts_points_ts on bashy_ts_points(ts);`

// tx is one connection running one transaction for one operation.
type tx struct {
	conn *sql.Conn
	ctx  context.Context
	done bool
}

// begin starts a read transaction (one snapshot) or an IMMEDIATE write
// transaction (the writer lock up front, so a contested write is busy at
// begin rather than failing half way).
func begin(ctx context.Context, h dbstore.Handle, write bool) (*tx, error) {
	db, err := handleDB(h)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	stmt := "begin"
	if write {
		stmt = "begin immediate"
	}
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		conn.Close()
		return nil, sqlitedb.ClassifyCtx(ctx, err)
	}
	return &tx{conn: conn, ctx: ctx}, nil
}

func (t *tx) classify(err error) error {
	if err == nil {
		return nil
	}
	if dbstore.Code(err) != "backend_error" {
		return err
	}
	return sqlitedb.ClassifyCtx(t.ctx, err)
}

func (t *tx) exec(q string, args ...any) (sql.Result, error) {
	r, err := t.conn.ExecContext(t.ctx, q, args...)
	return r, t.classify(err)
}

func (t *tx) query(q string, args ...any) (*sql.Rows, error) {
	r, err := t.conn.QueryContext(t.ctx, q, args...)
	return r, t.classify(err)
}

func (t *tx) queryRow(q string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(t.ctx, q, args...)
}

func (t *tx) commit() error {
	_, err := t.conn.ExecContext(t.ctx, "commit")
	if err == nil {
		t.done = true
	}
	return t.classify(err)
}

func (t *tx) close() {
	if !t.done {
		_, _ = t.conn.ExecContext(context.Background(), "rollback")
	}
	_ = t.conn.Close()
}

// ensureSchema creates the ts tables when absent. It runs only inside a
// write transaction, so concurrent first ingests serialize on the writer.
func (t *tx) ensureSchema() error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := t.exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// hasSchema reports whether this database already holds ts tables. Reads on
// a database that never ingested return empty, never an error.
func (t *tx) hasSchema() (bool, error) {
	var n int
	err := t.queryRow("select count(*) from sqlite_schema where name in ('" + tableSeries + "','" + tablePoints + "')").Scan(&n)
	if err != nil {
		return false, t.classify(err)
	}
	return n == 2, nil
}

// seriesID resolves a series, registering it on a write transaction when
// absent. Registration is data, not a database: ingest names the series it
// writes, the same way a put names its key.
func (t *tx) seriesID(name, labels string, create bool) (int64, error) {
	var id int64
	err := t.queryRow("select id from "+tableSeries+" where name = ? and labels = ?", name, labels).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, t.classify(err)
	}
	if !create {
		return 0, fmt.Errorf("%w: series %q", dbstore.ErrNotFound, name)
	}
	r, err := t.exec("insert into "+tableSeries+"(name, labels) values (?, ?)", name, labels)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// cursor is the shared bashy-cursor-v1 envelope (common-options.md; the
// joint decision spells its discriminator schema_version). SQLite exposes
// no revision, so the token is an offset bound to target and query only.
type cursor struct {
	SchemaVersion string        `json:"schema_version"`
	Kind          string        `json:"kind"`
	Token         string        `json:"token"`
	Binding       cursorBinding `json:"binding"`
}

type cursorBinding struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Query    string `json:"query"`
}

// requestHash binds a cursor to the logical request: the op plus the
// canonical selector, hashed before the op runs.
func requestHash(op string, canonical any) string {
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256([]byte(op + "\x00" + string(b)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func resumeOffset(since, target, query string) (int, error) {
	if since == "" {
		return 0, nil
	}
	var c cursor
	dec := json.NewDecoder(strings.NewReader(since))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.SchemaVersion != "bashy-cursor-v1" || c.Kind != "pagination" {
		return 0, fmt.Errorf("%w: --since wants a bashy-cursor-v1 pagination envelope", dbstore.ErrInvalidInput)
	}
	if c.Binding.Provider != "db" || c.Binding.Target != target || c.Binding.Query != query {
		return 0, fmt.Errorf("%w: cursor is bound to another target or query; restart without --since", dbstore.ErrInvalidInput)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(c.Token, "o:"))
	if err != nil || n < 0 || !strings.HasPrefix(c.Token, "o:") {
		return 0, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	return n, nil
}

type page struct {
	Returned  int     `json:"returned"`
	Limit     int     `json:"limit"`
	Truncated bool    `json:"truncated"`
	Bytes     int     `json:"bytes"`
	Cursor    *cursor `json:"cursor"`
}

func newPage(target, query string, offset, returned, limit, bytes int, truncated bool) page {
	p := page{Returned: returned, Limit: limit, Truncated: truncated, Bytes: bytes}
	if truncated {
		p.Cursor = &cursor{SchemaVersion: "bashy-cursor-v1", Kind: "pagination", Token: "o:" + strconv.Itoa(offset+returned),
			Binding: cursorBinding{Provider: "db", Target: target, Query: query}}
	}
	return p
}

const cursorWarning = "a cursor resumes by re-running the read at the next offset; SQLite exposes no revision, so pages are not one snapshot"

func init() {
	rd := func(Input) dblifecycle.Access { return dblifecycle.AccessRead }
	wr := func(Input) dblifecycle.Access { return dblifecycle.AccessWrite }
	register(OpIngest, wr, runIngest)
	register(OpRange, rd, runRange)
	register(OpAgg, rd, runAgg)
	register(OpRetain, func(Input) dblifecycle.Access { return dblifecycle.AccessExclusive }, runRetain)
	register(OpExport, rd, runExport)
	register(OpImport, func(in Input) dblifecycle.Access {
		if in.Replace {
			return dblifecycle.AccessExclusive
		}
		return dblifecycle.AccessWrite
	}, runImport)
	register(OpRemoteQuery, rd, runRemoteQuery)
	register(OpRemoteIngest, wr, runRemoteIngest)
}

type runFn func(context.Context, dbstore.Handle, Input) (dblifecycle.Outcome, error)

func register(name string, access func(Input) dblifecycle.Access, run runFn) {
	dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			in, err := DecodeInput(raw)
			if err != nil {
				return 0, err
			}
			// Typed values are validated before admission, so a dry run
			// and the owner both refuse them without touching the target.
			// Wall-clock time and generated ids stay inside Run.
			if err := validateFor(name, &in); err != nil {
				return 0, err
			}
			return access(in), nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return Spec(name, mode, a) },
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := DecodeInput(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if err := validateFor(name, &in); err != nil {
				return dblifecycle.Outcome{}, err
			}
			if a != access(in) {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: %s admitted with the wrong access", dbstore.ErrInvalidInput, name)
			}
			if name != OpRemoteQuery && name != OpRemoteIngest {
				if _, err := handleDB(h); err != nil {
					return dblifecycle.Outcome{}, err
				}
			}
			return run(ctx, h, in)
		},
	})
}

// Spec is the declaration shared by CLI, --explain, MCP and owner IPC.
func Spec(name string, mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
	s := dblifecycle.Operation{Name: name, MutationClass: "read", Hold: "none", Atomic: "op", DryRun: "local-preview", Modes: []string{"memory", "persistent"}}
	fs := func(tokens ...string) []string {
		if mode == dbstore.Persistent {
			return tokens
		}
		var out []string
		for _, t := range tokens {
			if !strings.HasPrefix(t, "fs.") {
				out = append(out, t)
			}
		}
		return out
	}
	switch a {
	case dblifecycle.AccessRead:
		s.Effects = fs("fs.read", "db.read")
	case dblifecycle.AccessWrite:
		s.MutationClass, s.Hold = "write", "shared-writer"
		s.Effects = fs("fs.write", "db.write")
	case dblifecycle.AccessExclusive:
		s.MutationClass, s.Hold = "destructive", "exclusive"
		s.Effects = fs("fs.write", "db.write", "db.delete")
	}
	switch name {
	case OpExport:
		s.Effects = append(fs("fs.read", "db.read"), "fs.write")
	case OpImport:
		s.Effects = append([]string{"fs.read"}, s.Effects...)
	case OpRemoteQuery:
		s.Modes = []string{"remote"}
		s.Effects = []string{"net.egress"}
		s.DryRun = "provider-validated-preview"
	case OpRemoteIngest:
		s.Modes = []string{"remote"}
		s.Effects = []string{"net.egress"}
		s.DryRun = "provider-validated-preview"
	}
	return s
}
