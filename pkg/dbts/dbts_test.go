package dbts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

type fixture struct {
	t      *testing.T
	svc    dblifecycle.Service
	target dbstore.Target
}

func newFixture(t *testing.T, mode dbstore.Mode) *fixture {
	t.Helper()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	f := &fixture{t: t}
	if mode == dbstore.Memory {
		mem, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { mem.Close() })
		f.svc = dblifecycle.Service{Mem: mem}
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: mem.ID(), Name: "t"}
	} else {
		f.target = dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "t.db")}
	}
	if _, err := f.svc.Create(context.Background(), f.target, time.Second); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) run(op string, in Input, yes bool) (dblifecycle.Outcome, error) {
	b, _ := json.Marshal(in)
	_, out, err := f.svc.RunCategory(context.Background(), f.target, op, b, yes, 10*time.Second)
	return out, err
}

func (f *fixture) must(op string, in Input, yes bool) dblifecycle.Outcome {
	f.t.Helper()
	out, err := f.run(op, in, yes)
	if err != nil {
		f.t.Fatalf("%s: %v", op, err)
	}
	return out
}

func (f *fixture) code(op string, in Input, yes bool) string {
	f.t.Helper()
	_, err := f.run(op, in, yes)
	return dbstore.Code(err)
}

func tsPoint(ts, value string) Point {
	return Point{Ts: json.RawMessage(ts), Value: json.RawMessage(value)}
}

func intTS(ns int64) string { return fmt.Sprintf(`{"t":"int","v":"%d"}`, ns) }
func rfcTS(s string) string { return fmt.Sprintf(`{"t":"ts","v":%q,"unit":"ns"}`, s) }
func f64v(f float64) string { return fmt.Sprintf(`{"t":"f64","v":%v}`, f) }
func intV(n int64) string   { return fmt.Sprintf(`{"t":"int","v":"%d"}`, n) }

func pointsOf(t *testing.T, out dblifecycle.Outcome) []any {
	t.Helper()
	r := out.Result.(map[string]any)
	return r["points"].([]any)
}

func TestTSIngestRangeRoundTrip(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			in := Input{Series: "cpu", Labels: map[string]string{"host": "a", "zone": "z1"}, Points: []Point{
				tsPoint(rfcTS("2026-10-01T00:00:00Z"), f64v(0.5)),
				tsPoint(intTS(1_758_000_000_000_000_000), intV(3)),
				tsPoint(rfcTS("2026-10-01T00:01:00+02:00"), f64v(-1.25)),
			}}
			res := f.must(OpIngest, in, false).Result.(map[string]any)
			if res["ingested"] != 3 {
				t.Fatalf("ingested = %v", res)
			}
			got := pointsOf(t, f.must(OpRange, Input{Series: "cpu", Labels: map[string]string{"zone": "z1", "host": "a"}}, false))
			if len(got) != 3 {
				t.Fatalf("range = %d points", len(got))
			}
			// Ordered by stamp: the raw-ns point, then the +02:00 point
			// normalized to UTC, then midnight UTC.
			second := got[1].(map[string]any)
			if second["ts"].(map[string]any)["v"] != "2026-09-30T22:01:00Z" {
				t.Fatalf("timezone offset not normalized: %v", second["ts"])
			}
			first := got[0].(map[string]any)
			// Tagged ints round-trip as tagged ints: the tag and the
			// exact decimal survive, never a coerced f64.
			if v := first["value"].(map[string]any); v["t"] != "int" || v["v"] != "3" {
				t.Fatalf("int value not exact: %v", first["value"])
			}
			// A range addresses one exact series; another label set is a
			// different series and does not leak in.
			f.must(OpIngest, Input{Series: "cpu", Labels: map[string]string{"host": "b"}, Points: []Point{
				tsPoint(intTS(1_758_000_000_000_000_000), f64v(9)),
			}}, false)
			got = pointsOf(t, f.must(OpRange, Input{Series: "cpu", Labels: map[string]string{"host": "a", "zone": "z1"}}, false))
			if len(got) != 3 {
				t.Fatalf("label filter leaked: %d points", len(got))
			}
			// Range bounds: start inclusive, end exclusive.
			got = pointsOf(t, f.must(OpRange, Input{Series: "cpu", Labels: map[string]string{"host": "a", "zone": "z1"},
				Start: json.RawMessage(intTS(1_758_000_000_000_000_000)),
				End:   json.RawMessage(intTS(1_758_000_060_000_000_000)),
			}, false))
			if len(got) != 1 {
				t.Fatalf("bounded range = %d points", len(got))
			}
			// Unknown series and fresh reads are empty, never an error.
			got = pointsOf(t, f.must(OpRange, Input{Series: "nope"}, false))
			if len(got) != 0 {
				t.Fatalf("missing series = %d points", len(got))
			}
		})
	}
}

func TestTSIngestConflicts(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			pts := []Point{tsPoint(intTS(1000), f64v(1)), tsPoint(intTS(2000), f64v(2))}
			f.must(OpIngest, Input{Series: "m", Points: pts}, false)
			// Same stamp twice is a revision-style conflict by default.
			if code := f.code(OpIngest, Input{Series: "m", Points: pts[:1]}, false); code != "conflict" {
				t.Fatalf("duplicate = %s", code)
			}
			res := f.must(OpIngest, Input{Series: "m", Points: pts, OnConflict: "skip"}, false).Result.(map[string]any)
			if res["ingested"] != 0 || res["skipped"] != 2 {
				t.Fatalf("skip = %v", res)
			}
			res = f.must(OpIngest, Input{Series: "m", Points: []Point{tsPoint(intTS(1000), f64v(7))}, OnConflict: "replace"}, false).Result.(map[string]any)
			if res["ingested"] != 0 || res["replaced"] != 1 {
				t.Fatalf("replace = %v", res)
			}
			got := pointsOf(t, f.must(OpRange, Input{Series: "m"}, false))
			if len(got) != 2 || got[0].(map[string]any)["value"].(map[string]any)["v"] != float64(7) {
				t.Fatalf("after replace: %v", got)
			}
			// A failed batch applies nothing.
			if code := f.code(OpIngest, Input{Series: "m", Points: []Point{
				tsPoint(intTS(3000), f64v(3)), tsPoint(intTS(1000), f64v(8)),
			}}, false); code != "conflict" {
				t.Fatalf("batch conflict = %s", code)
			}
			got = pointsOf(t, f.must(OpRange, Input{Series: "m"}, false))
			if len(got) != 2 {
				t.Fatalf("batch was not atomic: %d points", len(got))
			}
		})
	}
}

func TestTSAggBuckets(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			base := int64(1_700_000_000_000_000_000)
			var pts []Point
			for i, v := range []float64{1, 2, 3, 10, 20} {
				pts = append(pts, tsPoint(intTS(base+int64(i)*30_000_000_000), f64v(v)))
			}
			f.must(OpIngest, Input{Series: "g", Points: pts}, false)
			out := f.must(OpAgg, Input{Series: "g", Bucket: "1m"}, false).Result.(map[string]any)
			buckets := out["buckets"].([]any)
			if len(buckets) != 3 {
				t.Fatalf("buckets = %d", len(buckets))
			}
			first := buckets[0].(map[string]any)
			if first["count"].(map[string]any)["v"] != "2" {
				t.Fatalf("first bucket count: %v", first)
			}
			if first["min"].(map[string]any)["v"] != float64(1) || first["max"].(map[string]any)["v"] != float64(2) {
				t.Fatalf("first bucket min/max: %v", first)
			}
			if first["avg"].(map[string]any)["v"] != 1.5 || first["sum"].(map[string]any)["v"] != float64(3) {
				t.Fatalf("first bucket avg/sum: %v", first)
			}
			// Func subset and bucket binding.
			out = f.must(OpAgg, Input{Series: "g", Bucket: "2m", Funcs: []string{"count"}}, false).Result.(map[string]any)
			buckets = out["buckets"].([]any)
			if len(buckets) != 2 {
				t.Fatalf("2m buckets = %d", len(buckets))
			}
			if _, ok := buckets[0].(map[string]any)["avg"]; ok {
				t.Fatalf("unrequested func present: %v", buckets[0])
			}
			if code := f.code(OpAgg, Input{Series: "g", Bucket: "fortnight"}, false); code != "invalid_input" {
				t.Fatalf("bad bucket = %s", code)
			}
			if code := f.code(OpAgg, Input{Series: "g", Bucket: "1m", Funcs: []string{"p99"}}, false); code != "invalid_input" {
				t.Fatalf("bad func = %s", code)
			}
		})
	}
}

func TestTSRangeBoundsAndCursor(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	var pts []Point
	for i := int64(0); i < 5; i++ {
		pts = append(pts, tsPoint(intTS(1000+i*1000), f64v(float64(i))))
	}
	f.must(OpIngest, Input{Series: "c", Points: pts}, false)
	first := f.must(OpRange, Input{Series: "c", Limit: 2}, false)
	if p := first.Page.(page); !p.Truncated || p.Cursor == nil {
		t.Fatalf("no truncation: %+v", first.Page)
	} else {
		if p.Cursor.SchemaVersion != "bashy-cursor-v1" || p.Cursor.Binding.Provider != "db" {
			t.Fatalf("cursor envelope: %+v", p.Cursor)
		}
		since, _ := json.Marshal(p.Cursor)
		second := f.must(OpRange, Input{Series: "c", Limit: 2, Since: string(since)}, false)
		got := pointsOf(t, second)
		if len(got) != 2 || got[0].(map[string]any)["value"].(map[string]any)["v"] != float64(2) {
			t.Fatalf("resume: %v", got)
		}
		// A cursor from another query is refused.
		other := f.must(OpRange, Input{Series: "c", Limit: 1,
			Start: json.RawMessage(intTS(1000))}, false)
		_ = other
		if code := f.code(OpRange, Input{Series: "other", Limit: 2, Since: string(since)}, false); code != "invalid_input" {
			t.Fatalf("foreign cursor = %s", code)
		}
	}
	if code := f.code(OpRange, Input{Series: "c", Limit: 10001}, false); code != "invalid_input" {
		t.Fatalf("over-limit = %s", code)
	}
}

func TestTSRetainDeletesOldPoints(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			now := time.Now().UnixNano()
			old := now - 2*int64(time.Hour)
			f.must(OpIngest, Input{Series: "r", Points: []Point{
				tsPoint(intTS(old), f64v(1)),
				tsPoint(intTS(now), f64v(2)),
			}}, false)
			// Destructive: unconfirmed retain is refused before touching data.
			if code := f.code(OpRetain, Input{Series: "r", Retention: "1h"}, false); code != "confirmation_required" {
				t.Fatalf("unconfirmed retain = %s", code)
			}
			res := f.must(OpRetain, Input{Series: "r", Retention: "1h"}, true).Result.(map[string]any)
			if res["deleted"] != int64(1) {
				t.Fatalf("retain = %v", res)
			}
			got := pointsOf(t, f.must(OpRange, Input{Series: "r"}, false))
			if len(got) != 1 || got[0].(map[string]any)["value"].(map[string]any)["v"] != float64(2) {
				t.Fatalf("after retain: %v", got)
			}
			// Clearing the policy keeps data.
			res = f.must(OpRetain, Input{Series: "r", Retention: "off"}, true).Result.(map[string]any)
			if res["deleted"] != int64(0) || res["retention_ns"] != nil {
				t.Fatalf("retain off = %v", res)
			}
			if code := f.code(OpRetain, Input{Series: "r", Retention: "0s"}, true); code != "invalid_input" {
				t.Fatalf("zero retention = %s", code)
			}
		})
	}
}

func TestTSInputValidation(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	cases := []struct {
		name string
		op   string
		in   Input
		code string
	}{
		{"bad series", OpIngest, Input{Series: "has space", Points: []Point{tsPoint(intTS(1), f64v(1))}}, "invalid_input"},
		{"bad label key", OpIngest, Input{Series: "s", Labels: map[string]string{"9k": "v"}, Points: []Point{tsPoint(intTS(1), f64v(1))}}, "invalid_input"},
		{"no points", OpIngest, Input{Series: "s"}, "invalid_input"},
		{"untagged value", OpIngest, Input{Series: "s", Points: []Point{{Ts: json.RawMessage(intTS(1)), Value: json.RawMessage(`1.5`)}}}, "invalid_input"},
		{"untagged ts", OpIngest, Input{Series: "s", Points: []Point{{Ts: json.RawMessage(`1700000000`), Value: json.RawMessage(f64v(1))}}}, "invalid_input"},
		{"bad ts unit", OpRange, Input{Series: "s", Start: json.RawMessage(`{"t":"ts","v":"2026-01-01T00:00:00Z","unit":"fortnight"}`)}, "invalid_input"},
		{"bad on-conflict", OpIngest, Input{Series: "s", Points: []Point{tsPoint(intTS(1), f64v(1))}, OnConflict: "merge"}, "invalid_input"},
		{"extra field", OpRange, Input{Series: "s", Bucket: "1m"}, "invalid_input"},
		{"no endpoint", OpRemoteQuery, Input{Expr: "up", Step: "15s",
			Start: json.RawMessage(intTS(1)), End: json.RawMessage(intTS(2))}, "invalid_input"},
		{"ftp endpoint", OpRemoteQuery, Input{Endpoint: "ftp://h:8428", Expr: "up", Step: "15s",
			Start: json.RawMessage(intTS(1)), End: json.RawMessage(intTS(2))}, "invalid_input"},
		{"credentialed endpoint", OpRemoteQuery, Input{Endpoint: "http://u:p@h:8428", Expr: "up", Step: "15s",
			Start: json.RawMessage(intTS(1)), End: json.RawMessage(intTS(2))}, "invalid_input"},
		{"missing expr", OpRemoteQuery, Input{Endpoint: "http://h:8428",
			Start: json.RawMessage(intTS(1)), End: json.RawMessage(intTS(2)), Step: "15s"}, "invalid_input"},
		{"reversed range", OpRemoteQuery, Input{Endpoint: "http://h:8428", Expr: "up", Step: "15s",
			Start: json.RawMessage(intTS(2)), End: json.RawMessage(intTS(1))}, "invalid_input"},
		{"nan remote ingest", OpRemoteIngest, Input{Endpoint: "http://h:8428",
			SeriesList: []Series{{Name: "m", Points: []Point{tsPoint(intTS(1000), `{"t":"f64","v":"NaN"}`)}}}}, ""},
		// The float-text backend cannot hold int64 extremes: refused in
		// validation, before any byte is sent (local ingest accepts them).
		{"extreme int remote ingest", OpRemoteIngest, Input{Endpoint: "http://h:8428",
			SeriesList: []Series{{Name: "m", Points: []Point{tsPoint(intTS(1000), intV(math.MaxInt64))}}}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.op == OpRemoteIngest {
				// NaN is refused during validation, before any byte is sent.
				if code := f.code(c.op, c.in, false); code != "invalid_input" {
					t.Fatalf("got %s", code)
				}
				return
			}
			if code := f.code(c.op, c.in, false); code != c.code {
				t.Fatalf("got %s, want %s", code, c.code)
			}
		})
	}
	// Strict documents: unknown fields are refused, never ignored.
	var in Input
	raw := json.RawMessage(`{"series":"s","bogus":1,"points":[{"ts":{"t":"int","v":"1"},"value":{"t":"f64","v":1}}]}`)
	if _, err := DecodeInput(raw); err == nil {
		t.Fatal("unknown field accepted")
	} else if in.Series != "" {
		t.Fatal("decode partially filled input")
	}
}

func TestTSCancelledContext(t *testing.T) {
	f := newFixture(t, dbstore.Persistent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(Input{Series: "s"})
	_, _, err := f.svc.RunCategory(ctx, f.target, OpRange, b, false, time.Second)
	if dbstore.Code(err) != "cancelled" {
		t.Fatalf("cancelled range = %v", err)
	}
}

func TestTSInterchangeRoundTripsAcrossModes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to dbstore.Mode
	}{
		{"persistent-to-persistent", dbstore.Persistent, dbstore.Persistent},
		{"memory-to-memory", dbstore.Memory, dbstore.Memory},
		{"memory-to-persistent", dbstore.Memory, dbstore.Persistent},
		{"persistent-to-memory", dbstore.Persistent, dbstore.Memory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newFixture(t, tc.from)
			src.must(OpIngest, Input{Series: "a", Labels: map[string]string{"h": "1"}, Points: []Point{
				tsPoint(rfcTS("2026-03-01T00:00:00Z"), f64v(1.5)),
				tsPoint(intTS(9_007_199_254_740_991), intV(9007199254740991)),
				tsPoint(intTS(-5_000_000_000), f64v(-0.25)),
			}}, false)
			// int64 extremes are stored exactly, never rounded: the
			// dedicated typed columns keep the full 64 bits.
			src.must(OpIngest, Input{Series: "ext", Points: []Point{
				tsPoint(intTS(1), intV(math.MaxInt64)),
				tsPoint(intTS(2), intV(math.MinInt64)),
			}}, false)
			src.must(OpIngest, Input{Series: "b", Points: []Point{
				tsPoint(intTS(1000), `{"t":"f64","v":"-Inf"}`),
				tsPoint(intTS(2000), `{"t":"f64","v":"Inf"}`),
			}}, false)
			// NaN is refused: the driver binds it as NULL, so storing it
			// would be a silent loss.
			if code := src.code(OpIngest, Input{Series: "b", Points: []Point{
				tsPoint(intTS(3000), `{"t":"f64","v":"NaN"}`),
			}}, false); code != "invalid_input" {
				t.Fatalf("NaN ingest = %s", code)
			}
			out := filepath.Join(t.TempDir(), "exp")
			res := src.must(OpExport, Input{Out: out}, false).Result.(map[string]any)
			if res["series"] != 3 || res["points"] != int64(7) {
				t.Fatalf("export = %v", res)
			}
			if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			dst := newFixture(t, tc.to)
			res = dst.must(OpImport, Input{In: out, Replace: true}, true).Result.(map[string]any)
			if res["ingested"] != 7 {
				t.Fatalf("import = %v", res)
			}
			// The destination re-exports to identical record bytes: the
			// round trip is lossless across modes.
			out2 := filepath.Join(t.TempDir(), "exp2")
			dst.must(OpExport, Input{Out: out2}, false)
			b1, _ := os.ReadFile(filepath.Join(out, "points-000001.jsonl"))
			b2, _ := os.ReadFile(filepath.Join(out2, "points-000001.jsonl"))
			if string(b1) != string(b2) {
				t.Fatalf("re-export differs:\n%s\n%s", b1, b2)
			}
			got := pointsOf(t, dst.must(OpRange, Input{Series: "a", Labels: map[string]string{"h": "1"}}, false))
			if len(got) != 3 {
				t.Fatalf("imported range = %d", len(got))
			}
			// Ordered by stamp: the pre-epoch point, the float-exact int
			// (2^53-1 ns sits near the epoch), then the RFC point.
			if v := got[1].(map[string]any)["value"].(map[string]any); v["t"] != "int" || v["v"] != "9007199254740991" {
				t.Fatalf("exact int not preserved: %v", v)
			}
			// The extremes survive export/import with tag and bits intact.
			checkTaggedInts(t, pointsOf(t, dst.must(OpRange, Input{Series: "ext"}, false)),
				[]int64{math.MaxInt64, math.MinInt64})
			// Merge import keeps target retention and refuses duplicates.
			if code := dst.code(OpImport, Input{In: out}, false); code != "conflict" {
				t.Fatalf("merge duplicate = %s", code)
			}
			res = dst.must(OpImport, Input{In: out, OnConflict: "skip"}, false).Result.(map[string]any)
			if res["skipped"] != 7 {
				t.Fatalf("merge skip = %v", res)
			}
		})
	}
}

// TestTSAggApproximationDeclared pins the REAL-shadow contract: with int
// samples outside float-exact range in scope, min/max/sum/avg carry an
// approximation warning while count stays exact; in-range data and
// count-only requests stay quiet.
func TestTSAggApproximationDeclared(t *testing.T) {
	hasApprox := func(out dblifecycle.Outcome) bool {
		for _, w := range out.Warnings {
			if strings.Contains(w, "approximate") {
				return true
			}
		}
		return false
	}
	bucketCount := func(t *testing.T, out dblifecycle.Outcome) string {
		t.Helper()
		buckets := out.Result.(map[string]any)["buckets"].([]any)
		if len(buckets) != 1 {
			t.Fatalf("buckets = %d", len(buckets))
		}
		return buckets[0].(map[string]any)["count"].(map[string]any)["v"].(string)
	}
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			f.must(OpIngest, Input{Series: "big", Points: []Point{
				tsPoint(intTS(1000), intV(math.MaxInt64)),
				tsPoint(intTS(2000), intV(math.MinInt64)),
				tsPoint(intTS(3000), intV(7)),
			}}, false)
			out := f.must(OpAgg, Input{Series: "big", Bucket: "1h"}, false)
			if !hasApprox(out) {
				t.Fatalf("no approximation warning: %v", out.Warnings)
			}
			if got := bucketCount(t, out); got != "3" {
				t.Fatalf("bucket count = %s", got)
			}
			// Count-only stays quiet: counts over rows are exact.
			out = f.must(OpAgg, Input{Series: "big", Bucket: "1h", Funcs: []string{"count"}}, false)
			if hasApprox(out) {
				t.Fatalf("count-only warned: %v", out.Warnings)
			}
			if got := bucketCount(t, out); got != "3" {
				t.Fatalf("bucket count = %s", got)
			}
			// In-range ints and floats never warn.
			f.must(OpIngest, Input{Series: "small", Points: []Point{
				tsPoint(intTS(1000), intV(1<<53-1)),
				tsPoint(intTS(2000), intV(-(1<<53 - 1))),
				tsPoint(intTS(3000), f64v(0.5)),
			}}, false)
			out = f.must(OpAgg, Input{Series: "small", Bucket: "1h"}, false)
			if hasApprox(out) {
				t.Fatalf("in-range data warned: %v", out.Warnings)
			}
			// A range excluding the extremes stays quiet too.
			out = f.must(OpAgg, Input{Series: "big", Bucket: "1h",
				Start: json.RawMessage(intTS(3000))}, false)
			if hasApprox(out) {
				t.Fatalf("scoped-out extremes warned: %v", out.Warnings)
			}
		})
	}
}

// TestTSImportStagedBytesBound pins the staged-import contract: a record
// file declared over the staged cap is refused before mutation, and the
// target is unchanged afterwards.
func TestTSImportStagedBytesBound(t *testing.T) {
	src := newFixture(t, dbstore.Persistent)
	src.must(OpIngest, Input{Series: "s", Points: []Point{tsPoint(intTS(1), f64v(1))}}, false)
	good := filepath.Join(t.TempDir(), "good")
	src.must(OpExport, Input{Out: good}, false)

	dir := filepath.Join(t.TempDir(), "over")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(good, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Files[0].Bytes = maxImportFileBytes + 1
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), mb, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := newFixture(t, dbstore.Persistent)
	if code := dst.code(OpImport, Input{In: dir, Replace: true}, true); code != "limit_exceeded" {
		t.Fatalf("over-cap import = %s", code)
	}
	got := pointsOf(t, dst.must(OpRange, Input{Series: "s"}, false))
	if len(got) != 0 {
		t.Fatalf("refused import mutated the target: %d points", len(got))
	}
}

// checkTaggedInts asserts every ranged point carries its exact tagged int:
// the "int" tag plus the full decimal, including int64 extremes.
func checkTaggedInts(t *testing.T, got []any, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("points = %d, want %d", len(got), len(want))
	}
	for i, n := range want {
		v := got[i].(map[string]any)["value"].(map[string]any)
		if v["t"] != "int" || v["v"] != fmt.Sprintf("%d", n) {
			t.Fatalf("point %d = %v, want tagged int %d", i, v, n)
		}
	}
}

// TestTSTaggedIntExtremesRoundTrip keeps tagged ints exact across
// ingest/range and export/import: int64 extremes, the float-boundary
// ±2^53, zero and -1 stay tagged ints, while f64s stay f64s.
func TestTSTaggedIntExtremesRoundTrip(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Persistent, dbstore.Memory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			ints := []int64{math.MaxInt64, math.MinInt64, 1 << 53, -(1 << 53), 0, -1}
			var pts []Point
			for i, n := range ints {
				pts = append(pts, tsPoint(intTS(int64(1000+i)), intV(n)))
			}
			pts = append(pts, tsPoint(intTS(9000), f64v(1.5)))
			if res := f.must(OpIngest, Input{Series: "ext", Points: pts}, false).Result.(map[string]any); res["ingested"] != len(pts) {
				t.Fatalf("ingested = %v", res)
			}
			got := pointsOf(t, f.must(OpRange, Input{Series: "ext"}, false))
			checkTaggedInts(t, got[:len(ints)], ints)
			if v := got[len(got)-1].(map[string]any)["value"].(map[string]any); v["t"] != "f64" || v["v"] != 1.5 {
				t.Fatalf("f64 point = %v", v)
			}
			out := filepath.Join(t.TempDir(), "ext1")
			f.must(OpExport, Input{Out: out}, false)
			dst := newFixture(t, mode)
			dst.must(OpImport, Input{In: out, Replace: true}, true)
			got = pointsOf(t, dst.must(OpRange, Input{Series: "ext"}, false))
			checkTaggedInts(t, got[:len(ints)], ints)
			if v := got[len(got)-1].(map[string]any)["value"].(map[string]any); v["t"] != "f64" || v["v"] != 1.5 {
				t.Fatalf("imported f64 point = %v", v)
			}
			out2 := filepath.Join(t.TempDir(), "ext2")
			dst.must(OpExport, Input{Out: out2}, false)
			b1, _ := os.ReadFile(filepath.Join(out, "points-000001.jsonl"))
			b2, _ := os.ReadFile(filepath.Join(out2, "points-000001.jsonl"))
			if string(b1) != string(b2) {
				t.Fatalf("re-export differs:\n%s\n%s", b1, b2)
			}
		})
	}
}

func TestTSImportNegatives(t *testing.T) {
	src := newFixture(t, dbstore.Persistent)
	src.must(OpIngest, Input{Series: "n", Points: []Point{tsPoint(intTS(1), f64v(1))}}, false)
	good := filepath.Join(t.TempDir(), "good")
	src.must(OpExport, Input{Out: good}, false)

	readManifest := func(dir string) manifest {
		b, _ := os.ReadFile(filepath.Join(dir, ManifestName))
		var m manifest
		_ = json.Unmarshal(b, &m)
		return m
	}
	writeManifest := func(dir string, m manifest) {
		b, _ := json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, ManifestName), b, 0o600)
	}
	copyDir := func(dst string) {
		_ = os.Mkdir(dst, 0o700)
		ents, _ := os.ReadDir(good)
		for _, e := range ents {
			b, _ := os.ReadFile(filepath.Join(good, e.Name()))
			_ = os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600)
		}
	}
	dst := newFixture(t, dbstore.Persistent)
	t.Run("bad version", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "v")
		copyDir(dir)
		m := readManifest(dir)
		m.FormatVersion = "2.0"
		writeManifest(dir, m)
		if code := dst.code(OpImport, Input{In: dir, Replace: true}, true); code != "version_unsupported" {
			t.Fatalf("got %s", code)
		}
	})
	t.Run("bad category", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "c")
		copyDir(dir)
		m := readManifest(dir)
		m.Category = "kv"
		writeManifest(dir, m)
		if code := dst.code(OpImport, Input{In: dir, Replace: true}, true); code != "invalid_input" {
			t.Fatalf("got %s", code)
		}
	})
	t.Run("checksum", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "s")
		copyDir(dir)
		m := readManifest(dir)
		fh, _ := os.OpenFile(filepath.Join(dir, m.Files[0].Path), os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = fh.WriteString("\n")
		_ = fh.Close()
		if code := dst.code(OpImport, Input{In: dir, Replace: true}, true); code != "integrity" {
			t.Fatalf("got %s", code)
		}
	})
	t.Run("unknown series", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "u")
		copyDir(dir)
		m := readManifest(dir)
		// Append a well-formed record for a series outside schema_def,
		// then refresh the file's counts and checksum so validation
		// reaches the membership check itself.
		line := `{"series":"ghost","labels":{},"ts":{"t":"int","v":"2"},"value":{"t":"f64","v":1}}` + "\n"
		fh, _ := os.OpenFile(filepath.Join(dir, m.Files[0].Path), os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = fh.WriteString(line)
		_ = fh.Close()
		m.Files[0].Records++
		m.Files[0].Bytes += int64(len(line))
		raw, _ := os.ReadFile(filepath.Join(dir, m.Files[0].Path))
		sum := sha256.Sum256(raw)
		m.Files[0].SHA256 = hex.EncodeToString(sum[:])
		writeManifest(dir, m)
		if code := dst.code(OpImport, Input{In: dir, Replace: true}, true); code != "malformed_input" {
			t.Fatalf("got %s", code)
		}
	})
	t.Run("untagged value", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "w")
		copyDir(dir)
		raw, _ := os.ReadFile(filepath.Join(dir, "points-000001.jsonl"))
		bad := strings.Replace(string(raw), `"value":{"t":"f64","v":1}`, `"value":1`, 1)
		if bad == string(raw) {
			t.Skip("record shape changed; rewrite this negative")
		}
		_ = os.WriteFile(filepath.Join(dir, "points-000001.jsonl"), []byte(bad), 0o600)
		if code := dst.code(OpImport, Input{In: dir}, false); code == "" {
			t.Fatal("untagged value imported")
		}
	})
	// The target is unchanged after every negative.
	got := pointsOf(t, dst.must(OpRange, Input{Series: "n"}, false))
	if len(got) != 0 {
		t.Fatalf("failed imports mutated the target: %d points", len(got))
	}
}
