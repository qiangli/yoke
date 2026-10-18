package dbts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// validateFor checks every typed value before admission, so a dry run and
// the owner refuse bad input without touching the target. It computes no
// wall-clock time and mints no ids; those stay inside Run.
func validateFor(name string, in *Input) error {
	switch name {
	case OpIngest:
		if err := in.only(name, "series", "labels", "points", "on_conflict"); err != nil {
			return err
		}
		if err := checkSeriesName(in.Series); err != nil {
			return err
		}
		if _, err := canonicalLabels(in.Labels); err != nil {
			return err
		}
		if len(in.Points) == 0 || len(in.Points) > MaxBatchPoints {
			return fmt.Errorf("%w: ingest takes 1..%d points", dbstore.ErrInvalidInput, MaxBatchPoints)
		}
		if err := checkOnConflict(in.OnConflict); err != nil {
			return err
		}
		for i, p := range in.Points {
			if _, err := parseTS(fmt.Sprintf("points[%d].ts", i), p.Ts); err != nil {
				return err
			}
			if _, err := parseSampleValue(fmt.Sprintf("points[%d].value", i), p.Value); err != nil {
				return err
			}
		}
	case OpRange:
		if err := in.only(name, "series", "labels", "start", "end", "limit", "max_bytes", "since"); err != nil {
			return err
		}
		if err := checkSeriesName(in.Series); err != nil {
			return err
		}
		if _, err := canonicalLabels(in.Labels); err != nil {
			return err
		}
		if _, _, err := rangeBounds(in); err != nil {
			return err
		}
	case OpAgg:
		if err := in.only(name, "series", "labels", "start", "end", "bucket", "funcs", "limit", "max_bytes", "since"); err != nil {
			return err
		}
		if err := checkSeriesName(in.Series); err != nil {
			return err
		}
		if _, err := canonicalLabels(in.Labels); err != nil {
			return err
		}
		if _, _, err := rangeBounds(in); err != nil {
			return err
		}
		if _, err := parseDuration("bucket", in.Bucket); err != nil {
			return err
		}
		if _, err := checkFuncs(in.Funcs); err != nil {
			return err
		}
	case OpRetain:
		if err := in.only(name, "series", "labels", "retention"); err != nil {
			return err
		}
		if in.Series != "" {
			if err := checkSeriesName(in.Series); err != nil {
				return err
			}
		}
		if _, err := canonicalLabels(in.Labels); err != nil {
			return err
		}
		if _, err := parseRetention(in.Retention); err != nil {
			return err
		}
	case OpExport:
		if err := in.only(name, "out"); err != nil {
			return err
		}
	case OpImport:
		if err := in.only(name, "in", "on_conflict", "replace"); err != nil {
			return err
		}
		if err := checkOnConflict(in.OnConflict); err != nil {
			return err
		}
	case OpRemoteQuery:
		if err := in.only(name, "endpoint", "expr", "start", "end", "step", "limit", "timeout"); err != nil {
			return err
		}
		return validateRemoteQuery(in)
	case OpRemoteIngest:
		if err := in.only(name, "endpoint", "series_list", "timeout"); err != nil {
			return err
		}
		return validateRemoteIngest(in)
	default:
		return fmt.Errorf("%w: unknown ts operation %s", dbstore.ErrUnsupported, name)
	}
	return nil
}

func checkOnConflict(s string) error {
	if s == "" {
		return nil
	}
	switch s {
	case "error", "skip", "replace":
		return nil
	}
	return fmt.Errorf("%w: on_conflict %q (error, skip, replace)", dbstore.ErrInvalidInput, s)
}

// rangeBounds decodes the optional start/end pair; start is inclusive, end
// is exclusive, and start past end is empty, never an error.
func rangeBounds(in *Input) (start, end *int64, err error) {
	if len(in.Start) != 0 {
		n, err := parseTS("start", in.Start)
		if err != nil {
			return nil, nil, err
		}
		start = &n
	}
	if len(in.End) != 0 {
		n, err := parseTS("end", in.End)
		if err != nil {
			return nil, nil, err
		}
		end = &n
	}
	return start, end, nil
}

var aggFuncs = []string{"count", "min", "max", "sum", "avg"}

func checkFuncs(funcs []string) ([]string, error) {
	if len(funcs) == 0 {
		return aggFuncs, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range funcs {
		ok := false
		for _, a := range aggFuncs {
			if f == a {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: agg func %q (count, min, max, sum, avg)", dbstore.ErrInvalidInput, f)
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// parseRetention decodes a retention policy: "off" clears it, otherwise a
// positive bounded duration.
func parseRetention(s string) (*int64, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: retain needs retention (a duration like 30d, or off)", dbstore.ErrInvalidInput)
	}
	if s == "off" {
		return nil, nil
	}
	n, err := parseDuration("retention", s)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// selector is the canonical form a cursor binds to.
type selector struct {
	Series string            `json:"series"`
	Labels map[string]string `json:"labels,omitempty"`
	Start  *int64            `json:"start,omitempty"`
	End    *int64            `json:"end,omitempty"`
	Bucket *int64            `json:"bucket,omitempty"`
	Funcs  []string          `json:"funcs,omitempty"`
}

func (in *Input) selector() (selector, error) {
	labels := map[string]string{}
	for k, v := range in.Labels {
		labels[k] = v
	}
	s := selector{Series: in.Series, Labels: labels}
	start, end, err := rangeBounds(in)
	if err != nil {
		return s, err
	}
	s.Start, s.End = start, end
	return s, nil
}

func runIngest(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	labels, err := canonicalLabels(in.Labels)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	type sample struct {
		ts  int64
		val sampleValue
	}
	samples := make([]sample, len(in.Points))
	for i, p := range in.Points {
		ts, err := parseTS(fmt.Sprintf("points[%d].ts", i), p.Ts)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		v, err := parseSampleValue(fmt.Sprintf("points[%d].value", i), p.Value)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		samples[i] = sample{ts: ts, val: v}
	}
	onConflict := in.OnConflict
	if onConflict == "" {
		onConflict = "error"
	}
	t, err := begin(ctx, h, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.close()
	if err := t.ensureSchema(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	id, err := t.seriesID(in.Series, labels, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	ingested, skipped, replaced := 0, 0, 0
	for i, s := range samples {
		if onConflict == "replace" {
			// Probe first: insert-or-replace cannot tell an insert from
			// an overwrite by RowsAffected, so count exactly.
			var n int
			if err := t.queryRow("select count(*) from "+tablePoints+" where series_id = ? and ts = ?", id, s.ts).Scan(&n); err != nil {
				return dblifecycle.Outcome{}, t.classify(err)
			}
			if _, err := t.exec("insert or replace into "+tablePoints+"(series_id, ts, value, value_kind, value_int) values (?, ?, ?, ?, ?)", id, s.ts, s.val.fval, kindOf(s.val), intOf(s.val)); err != nil {
				return dblifecycle.Outcome{}, fmt.Errorf("points[%d]: %w", i, err)
			}
			if n == 0 {
				ingested++
			} else {
				replaced++
			}
			continue
		}
		stmt := "insert into " + tablePoints + "(series_id, ts, value, value_kind, value_int) values (?, ?, ?, ?, ?)"
		if onConflict == "skip" {
			stmt = "insert or ignore into " + tablePoints + "(series_id, ts, value, value_kind, value_int) values (?, ?, ?, ?, ?)"
		}
		r, err := t.exec(stmt, id, s.ts, s.val.fval, kindOf(s.val), intOf(s.val))
		if err != nil {
			if dbstore.Code(err) == "conflict" {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: points[%d] at ts %d already exists (on_conflict=error)", err, i, s.ts)
			}
			return dblifecycle.Outcome{}, fmt.Errorf("points[%d]: %w", i, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			skipped++
		} else {
			ingested++
		}
	}
	if err := t.commit(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	res := map[string]any{"series": in.Series, "ingested": ingested}
	switch onConflict {
	case "skip":
		res["skipped"] = skipped
	case "replace":
		res["replaced"] = replaced
	}
	return dblifecycle.Outcome{Result: res}, nil
}

func runRange(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	labels, err := canonicalLabels(in.Labels)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	sel, err := in.selector()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target := h.Identity().ID
	binding := requestHash(OpRange, sel)
	offset, err := resumeOffset(in.Since, target, binding)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.close()
	ok, err := t.hasSchema()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if !ok {
		return dblifecycle.Outcome{Result: map[string]any{"series": in.Series, "points": []any{}},
			Page: newPage(target, binding, offset, 0, limit, 0, false)}, nil
	}
	var id int64
	if err := t.queryRow("select s.id from "+tableSeries+" s where s.name = ? and s.labels = ?", in.Series, labels).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return dblifecycle.Outcome{Result: map[string]any{"series": in.Series, "points": []any{}},
				Page: newPage(target, binding, offset, 0, limit, 0, false)}, nil
		}
		return dblifecycle.Outcome{}, t.classify(err)
	}
	var sb strings.Builder
	sb.WriteString("select p.ts, p.value_kind, p.value_int, p.value from " + tablePoints + " p where p.series_id = ?")
	args := []any{id}
	if sel.Start != nil {
		sb.WriteString(" and p.ts >= ?")
		args = append(args, *sel.Start)
	}
	if sel.End != nil {
		sb.WriteString(" and p.ts < ?")
		args = append(args, *sel.End)
	}
	sb.WriteString(" order by p.ts limit ? offset ?")
	args = append(args, limit+1, offset)
	rows, err := t.query(sb.String(), args...)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer rows.Close()
	out := []any{}
	used, truncated := 0, false
	for rows.Next() {
		if len(out) == limit {
			truncated = true
			break
		}
		var ts int64
		var kind int
		var ival sql.NullInt64
		var fval float64
		if err := rows.Scan(&ts, &kind, &ival, &fval); err != nil {
			return dblifecycle.Outcome{}, t.classify(err)
		}
		v := sampleValue{fval: fval}
		if kind == valueKindInt && ival.Valid {
			v = sampleValue{isInt: true, ival: ival.Int64, fval: fval}
		}
		rec := map[string]any{"ts": encodeTS(ts), "value": encodeSampleValue(v)}
		b, _ := json.Marshal(rec)
		if used+len(b) > maxBytes {
			if len(out) == 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: point %d is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, offset+1, len(b), maxBytes)
			}
			truncated = true
			break
		}
		out = append(out, rec)
		used += len(b)
	}
	if err := rows.Err(); err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	if err := ctx.Err(); err != nil {
		return dblifecycle.Outcome{}, dbstore.FromContext(err)
	}
	res := dblifecycle.Outcome{Result: map[string]any{"series": in.Series, "points": out},
		Page: newPage(target, binding, offset, len(out), limit, used, truncated)}
	if truncated {
		res.Warnings = []string{cursorWarning}
	}
	return res, nil
}

func runAgg(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	labels, err := canonicalLabels(in.Labels)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	sel, err := in.selector()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	bucket, err := parseDuration("bucket", in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	sel.Bucket = &bucket
	funcs, err := checkFuncs(in.Funcs)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	sel.Funcs = funcs
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	target := h.Identity().ID
	binding := requestHash(OpAgg, sel)
	offset, err := resumeOffset(in.Since, target, binding)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	t, err := begin(ctx, h, false)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.close()
	ok, err := t.hasSchema()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if !ok {
		return dblifecycle.Outcome{Result: map[string]any{"series": in.Series, "buckets": []any{}},
			Page: newPage(target, binding, offset, 0, limit, 0, false)}, nil
	}
	var id int64
	if err := t.queryRow("select s.id from "+tableSeries+" s where s.name = ? and s.labels = ?", in.Series, labels).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return dblifecycle.Outcome{Result: map[string]any{"series": in.Series, "buckets": []any{}},
				Page: newPage(target, binding, offset, 0, limit, 0, false)}, nil
		}
		return dblifecycle.Outcome{}, t.classify(err)
	}
	// Floor every stamp into its bucket; the double modulo floors negatives
	// too, so pre-1970 buckets contain their stamps.
	q := `select p.ts - ((p.ts % ?) + ?) % ? as b, count(*), min(p.value), max(p.value), sum(p.value), avg(p.value)
 from ` + tablePoints + ` p where p.series_id = ?`
	args := []any{bucket, bucket, bucket, id}
	if sel.Start != nil {
		q += " and p.ts >= ?"
		args = append(args, *sel.Start)
	}
	if sel.End != nil {
		q += " and p.ts < ?"
		args = append(args, *sel.End)
	}
	q += " group by b order by b limit ? offset ?"
	args = append(args, limit+1, offset)
	rows, err := t.query(q, args...)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer rows.Close()
	want := map[string]bool{}
	for _, f := range funcs {
		want[f] = true
	}
	out := []any{}
	used, truncated := 0, false
	for rows.Next() {
		if len(out) == limit {
			truncated = true
			break
		}
		var b, count int64
		var mn, mx, sum, avg sql.NullFloat64
		if err := rows.Scan(&b, &count, &mn, &mx, &sum, &avg); err != nil {
			return dblifecycle.Outcome{}, t.classify(err)
		}
		rec := map[string]any{"bucket_start": encodeTS(b)}
		if want["count"] {
			rec["count"] = map[string]any{"t": "int", "v": fmt.Sprintf("%d", count)}
		}
		if want["min"] && mn.Valid {
			rec["min"] = encodeValue(mn.Float64)
		}
		if want["max"] && mx.Valid {
			rec["max"] = encodeValue(mx.Float64)
		}
		if want["sum"] && sum.Valid {
			rec["sum"] = encodeValue(sum.Float64)
		}
		if want["avg"] && avg.Valid {
			rec["avg"] = encodeValue(avg.Float64)
		}
		jb, _ := json.Marshal(rec)
		if used+len(jb) > maxBytes {
			if len(out) == 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket %d is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, offset+1, len(jb), maxBytes)
			}
			truncated = true
			break
		}
		out = append(out, rec)
		used += len(jb)
	}
	if err := rows.Err(); err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	if err := ctx.Err(); err != nil {
		return dblifecycle.Outcome{}, dbstore.FromContext(err)
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	res := dblifecycle.Outcome{Result: map[string]any{"series": in.Series, "bucket_ns": bucket, "funcs": keys, "buckets": out},
		Page: newPage(target, binding, offset, len(out), limit, used, truncated)}
	if truncated {
		res.Warnings = []string{cursorWarning}
	}
	return res, nil
}

func runRetain(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	ret, err := parseRetention(in.Retention)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	// Wall-clock time is read here, inside Run, never during admission.
	now := time.Now().UnixNano()
	t, err := begin(ctx, h, true)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	defer t.close()
	if err := t.ensureSchema(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	var sb strings.Builder
	sb.WriteString("select s.id, s.name, s.labels from " + tableSeries + " s where 1 = 1")
	var args []any
	if in.Series != "" {
		sb.WriteString(" and s.name = ?")
		args = append(args, in.Series)
	}
	if len(in.Labels) != 0 {
		labels, err := canonicalLabels(in.Labels)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		clause, err := labelClauses(labels, &args)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		sb.WriteString(clause)
	}
	sb.WriteString(" order by s.name, s.labels")
	rows, err := t.query(sb.String(), args...)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	type matched struct {
		id     int64
		name   string
		labels string
	}
	var series []matched
	for rows.Next() {
		var m matched
		if err := rows.Scan(&m.id, &m.name, &m.labels); err != nil {
			rows.Close()
			return dblifecycle.Outcome{}, t.classify(err)
		}
		series = append(series, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return dblifecycle.Outcome{}, t.classify(err)
	}
	var deleted int64
	names := []string{}
	for _, m := range series {
		var r sql.Result
		if ret == nil {
			r, err = t.exec("update "+tableSeries+" set retention_ns = null where id = ?", m.id)
		} else {
			r, err = t.exec("update "+tableSeries+" set retention_ns = ? where id = ?", *ret, m.id)
		}
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: series %q changed under retain", dbstore.ErrConflict, m.name)
		}
		names = append(names, m.name)
		if ret != nil {
			cutoff := now - *ret
			r, err := t.exec("delete from "+tablePoints+" where series_id = ? and ts < ?", m.id, cutoff)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			n, _ := r.RowsAffected()
			deleted += n
		}
	}
	if err := t.commit(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	res := map[string]any{"series": names, "deleted": deleted}
	if ret == nil {
		res["retention_ns"] = nil
	} else {
		res["retention_ns"] = *ret
	}
	return dblifecycle.Outcome{Result: res}, nil
}
