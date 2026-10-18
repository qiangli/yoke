// Package dbkv is the Sprint 453 key-value category (story 10fbbb26510d) over
// the bbolt adapter and its in-memory supplement: `db kv ...` and the KV
// logical interchange behind `db export|import`.
//
// Every operation registers with dblifecycle, so CLI, MCP and owner sessions
// share one claim guard, owner admission, request-id replay and epoch check.
// Buckets are one level deep with UTF-8 names; keys and values are arbitrary
// bytes carried as {"text":...} or {"b64":...}. Nothing is created
// implicitly: a bucket must exist before put/delete/cas/batch touch it.
// There is no TTL or expiry and no Redis compatibility; a ttl request is
// refused as unsupported.
package dbkv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Operation names, as registered with dblifecycle.
const (
	OpBucketCreate = "kv.bucket.create"
	OpBucketDrop   = "kv.bucket.drop"
	OpBucketList   = "kv.bucket.list"
	OpGet          = "kv.get"
	OpPut          = "kv.put"
	OpDelete       = "kv.delete"
	OpScan         = "kv.scan"
	OpBatch        = "kv.batch"
	OpCAS          = "kv.cas"
	OpExport       = "kv.export"
	OpImport       = "kv.import"
)

// Bounds declared for the kv category.
const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10
	MaxMaxBytes     = 8 << 20
	MaxBatchOps     = 1000
	MaxBucketName   = 255
)

// Value is a byte string: exactly one of Text (valid UTF-8) or B64.
type Value struct {
	Text *string `json:"text,omitempty"`
	B64  *string `json:"b64,omitempty"`
}

// Bytes decodes the value.
func (v *Value) Bytes() ([]byte, error) {
	switch {
	case v == nil || (v.Text == nil && v.B64 == nil):
		return nil, fmt.Errorf("%w: value needs text or b64", dbstore.ErrInvalidInput)
	case v.Text != nil && v.B64 != nil:
		return nil, fmt.Errorf("%w: value gives both text and b64", dbstore.ErrInvalidInput)
	case v.Text != nil:
		return []byte(*v.Text), nil
	}
	b, err := base64.StdEncoding.DecodeString(*v.B64)
	if err != nil {
		return nil, fmt.Errorf("%w: b64 value: %v", dbstore.ErrInvalidInput, err)
	}
	return b, nil
}

// Encode renders bytes as text when valid UTF-8, else base64. Both decode to
// identical bytes, so the choice is presentation only.
func Encode(b []byte) Value {
	if utf8.Valid(b) {
		s := string(b)
		return Value{Text: &s}
	}
	s := base64.StdEncoding.EncodeToString(b)
	return Value{B64: &s}
}

// BatchOp is one entry of an atomic batch. "check" asserts a key's current
// value (or absence) so a batch is a multi-key compare-and-set.
type BatchOp struct {
	Op           string `json:"op"`
	Bucket       string `json:"bucket,omitempty"`
	Key          *Value `json:"key,omitempty"`
	Value        *Value `json:"value,omitempty"`
	Expect       *Value `json:"expect,omitempty"`
	ExpectAbsent bool   `json:"expect_absent,omitempty"`
}

// Input is the typed input every kv operation accepts.
type Input struct {
	Bucket       string    `json:"bucket,omitempty"`
	Key          *Value    `json:"key,omitempty"`
	Value        *Value    `json:"value,omitempty"`
	Expect       *Value    `json:"expect,omitempty"`
	ExpectAbsent bool      `json:"expect_absent,omitempty"`
	Start        *Value    `json:"start,omitempty"`
	Prefix       *Value    `json:"prefix,omitempty"`
	Ops          []BatchOp `json:"ops,omitempty"`
	TTL          string    `json:"ttl,omitempty"`
	Limit        int       `json:"limit,omitempty"`
	MaxBytes     int       `json:"max_bytes,omitempty"`
	Since        string    `json:"since,omitempty"`
	Out          string    `json:"out,omitempty"`
	In           string    `json:"in,omitempty"`
	OnConflict   string    `json:"on_conflict,omitempty"`
	Replace      bool      `json:"replace,omitempty"`
}

func decodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: kv input: %v", dbstore.ErrInvalidInput, err)
	}
	if in.TTL != "" {
		return in, fmt.Errorf("%w: the bbolt kv category has no TTL or expiry", dbstore.ErrUnsupported)
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

func bucketName(s string) ([]byte, error) {
	if s == "" || len(s) > MaxBucketName || !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: bucket name must be 1..%d bytes of UTF-8", dbstore.ErrInvalidInput, MaxBucketName)
	}
	return []byte(s), nil
}

func handleKV(h dbstore.Handle) (dbstore.KV, error) {
	kv, ok := h.(dbstore.KV)
	if !ok {
		return nil, fmt.Errorf("%w: kv category needs the bbolt engine", dbstore.ErrUnsupported)
	}
	return kv, nil
}

func init() {
	rd := func(Input) dblifecycle.Access { return dblifecycle.AccessRead }
	wr := func(Input) dblifecycle.Access { return dblifecycle.AccessWrite }
	register(OpBucketCreate, wr, runBucketCreate)
	register(OpBucketDrop, func(Input) dblifecycle.Access { return dblifecycle.AccessExclusive }, runBucketDrop)
	register(OpBucketList, rd, runBucketList)
	register(OpGet, rd, runGet)
	register(OpPut, wr, runPut)
	register(OpDelete, wr, runDelete)
	register(OpScan, rd, runScan)
	register(OpBatch, wr, runBatch)
	register(OpCAS, wr, runCAS)
	register(OpExport, rd, runExport)
	register(OpImport, func(in Input) dblifecycle.Access {
		if in.Replace {
			return dblifecycle.AccessExclusive
		}
		return dblifecycle.AccessWrite
	}, runImport)
}

type runFn func(context.Context, dbstore.KV, dbstore.Handle, Input) (dblifecycle.Outcome, error)

func register(name string, access func(Input) dblifecycle.Access, run runFn) {
	dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			in, err := decodeInput(raw)
			if err != nil {
				return 0, err
			}
			return access(in), nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return Spec(name, mode, a) },
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := decodeInput(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if a != access(in) {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: %s admitted with the wrong access", dbstore.ErrInvalidInput, name)
			}
			kv, err := handleKV(h)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			return run(ctx, kv, h, in)
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
	}
	return s
}

// cursor is the shared bashy-cursor-v1 envelope; the token is the next key
// to return, so a resume is exact on keys even across writes.
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

type page struct {
	Returned  int     `json:"returned"`
	Limit     int     `json:"limit"`
	Truncated bool    `json:"truncated"`
	Bytes     int     `json:"bytes"`
	Cursor    *cursor `json:"cursor"`
}

func scanBinding(in Input) string {
	var p, s string
	if in.Prefix != nil {
		b, _ := in.Prefix.Bytes()
		p = base64.StdEncoding.EncodeToString(b)
	}
	if in.Start != nil {
		b, _ := in.Start.Bytes()
		s = base64.StdEncoding.EncodeToString(b)
	}
	return in.Bucket + "\x00" + p + "\x00" + s
}

func resumeKey(since, target, query string) ([]byte, error) {
	var c cursor
	dec := json.NewDecoder(strings.NewReader(since))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.SchemaVersion != "bashy-cursor-v1" || c.Kind != "pagination" {
		return nil, fmt.Errorf("%w: --since wants a bashy-cursor-v1 pagination envelope", dbstore.ErrInvalidInput)
	}
	if c.Binding.Provider != "db" || c.Binding.Target != target || c.Binding.Query != query {
		return nil, fmt.Errorf("%w: cursor is bound to another target or scan; restart without --since", dbstore.ErrInvalidInput)
	}
	tok, ok := strings.CutPrefix(c.Token, "k:")
	if !ok {
		return nil, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	k, err := base64.StdEncoding.DecodeString(tok)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor token", dbstore.ErrInvalidInput)
	}
	return k, nil
}

const cursorWarning = "a cursor resumes at the next key; pages are separate snapshots, so writes between pages can add or remove keys"

func runBucketCreate(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.Key != nil || in.Value != nil || len(in.Ops) != 0 {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: bucket create takes only --bucket", dbstore.ErrInvalidInput)
	}
	if err := kv.Update(ctx, func(tx dbstore.KVTx) error { return tx.CreateBucket(name) }); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"bucket": in.Bucket, "created": true}}, nil
}

func runBucketDrop(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := kv.Update(ctx, func(tx dbstore.KVTx) error { return tx.DeleteBucket(name) }); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"bucket": in.Bucket, "dropped": true}}, nil
}

func runBucketList(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	var names []string
	err := kv.View(ctx, func(tx dbstore.KVTx) error {
		bs, err := tx.Buckets()
		for _, b := range bs {
			names = append(names, string(b))
		}
		return err
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if names == nil {
		names = []string{}
	}
	return dblifecycle.Outcome{Result: map[string]any{"buckets": names}}, nil
}

func keyOf(in Input) ([]byte, error) {
	k, err := in.Key.Bytes()
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	if len(k) == 0 {
		return nil, fmt.Errorf("%w: empty key", dbstore.ErrInvalidInput)
	}
	return k, nil
}

func runGet(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	key, err := keyOf(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var v []byte
	var found bool
	if err := kv.View(ctx, func(tx dbstore.KVTx) (e error) { v, found, e = tx.Get(name, key); return }); err != nil {
		return dblifecycle.Outcome{}, err
	}
	res := map[string]any{"bucket": in.Bucket, "key": Encode(key), "found": found}
	if found {
		res["value"] = Encode(v)
	}
	return dblifecycle.Outcome{Result: res}, nil
}

func runPut(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	key, err := keyOf(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	val, err := in.Value.Bytes()
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("value: %w", err)
	}
	var existed bool
	err = kv.Update(ctx, func(tx dbstore.KVTx) (e error) {
		if _, existed, e = tx.Get(name, key); e != nil {
			return
		}
		return tx.Put(name, key, val)
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"bucket": in.Bucket, "key": Encode(key), "created": !existed, "replaced": existed}}, nil
}

func runDelete(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	key, err := keyOf(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var existed bool
	err = kv.Update(ctx, func(tx dbstore.KVTx) (e error) {
		if _, existed, e = tx.Get(name, key); e != nil {
			return
		}
		return tx.Delete(name, key)
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"bucket": in.Bucket, "key": Encode(key), "deleted": existed}}, nil
}

// check asserts a key's current state against an expectation.
func check(tx dbstore.KVTx, bucket, key []byte, expect *Value, absent bool) error {
	if absent == (expect != nil) {
		return fmt.Errorf("%w: give exactly one of expect or expect_absent", dbstore.ErrInvalidInput)
	}
	var want []byte
	if expect != nil {
		var err error
		if want, err = expect.Bytes(); err != nil {
			return fmt.Errorf("expect: %w", err)
		}
	}
	cur, found, err := tx.Get(bucket, key)
	if err != nil {
		return err
	}
	if absent && found {
		return fmt.Errorf("%w: key %s exists, expected absent", dbstore.ErrConflict, show(key))
	}
	if !absent && (!found || !bytes.Equal(cur, want)) {
		if !found {
			return fmt.Errorf("%w: key %s is absent, expected a value", dbstore.ErrConflict, show(key))
		}
		return fmt.Errorf("%w: key %s holds a different value than expected", dbstore.ErrConflict, show(key))
	}
	return nil
}

func show(k []byte) string {
	v := Encode(k)
	if v.Text != nil {
		return strconv.Quote(*v.Text)
	}
	return "b64:" + *v.B64
}

func runCAS(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	key, err := keyOf(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	val, err := in.Value.Bytes()
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("value: %w", err)
	}
	err = kv.Update(ctx, func(tx dbstore.KVTx) error {
		if err := check(tx, name, key, in.Expect, in.ExpectAbsent); err != nil {
			return err
		}
		return tx.Put(name, key, val)
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"bucket": in.Bucket, "key": Encode(key), "swapped": true}}, nil
}

func runBatch(ctx context.Context, kv dbstore.KV, _ dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	if len(in.Ops) == 0 || len(in.Ops) > MaxBatchOps {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: a batch takes 1..%d ops", dbstore.ErrInvalidInput, MaxBatchOps)
	}
	type step struct {
		op      BatchOp
		bucket  []byte
		key, vl []byte
	}
	steps := make([]step, len(in.Ops))
	for i, op := range in.Ops {
		s := step{op: op}
		fail := func(err error) error { return fmt.Errorf("op %d: %w", i+1, err) }
		var err error
		if s.bucket, err = bucketName(op.Bucket); err != nil {
			return dblifecycle.Outcome{}, fail(err)
		}
		switch op.Op {
		case "create_bucket":
		case "put", "delete", "check":
			if s.key, err = keyOf(Input{Key: op.Key}); err != nil {
				return dblifecycle.Outcome{}, fail(err)
			}
			if op.Op == "put" {
				if s.vl, err = op.Value.Bytes(); err != nil {
					return dblifecycle.Outcome{}, fail(err)
				}
			}
		default:
			return dblifecycle.Outcome{}, fail(fmt.Errorf("%w: op %q (want create_bucket, put, delete or check)", dbstore.ErrInvalidInput, op.Op))
		}
		steps[i] = s
	}
	err := kv.Update(ctx, func(tx dbstore.KVTx) error {
		for i, s := range steps {
			var err error
			switch s.op.Op {
			case "create_bucket":
				err = tx.CreateBucket(s.bucket)
			case "put":
				err = tx.Put(s.bucket, s.key, s.vl)
			case "delete":
				err = tx.Delete(s.bucket, s.key)
			case "check":
				err = check(tx, s.bucket, s.key, s.op.Expect, s.op.ExpectAbsent)
			}
			if err != nil {
				return fmt.Errorf("op %d: %w", i+1, err)
			}
		}
		return nil
	})
	if err != nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w (rolled back; nothing was applied)", err)
	}
	return dblifecycle.Outcome{Result: map[string]any{"committed": true, "ops": len(steps)}}, nil
}

type entry struct {
	Key   Value `json:"key"`
	Value Value `json:"value"`
}

func runScan(ctx context.Context, kv dbstore.KV, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	name, err := bucketName(in.Bucket)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var start, prefix []byte
	if in.Start != nil {
		if start, err = in.Start.Bytes(); err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("start: %w", err)
		}
	}
	if in.Prefix != nil {
		if prefix, err = in.Prefix.Bytes(); err != nil {
			return dblifecycle.Outcome{}, fmt.Errorf("prefix: %w", err)
		}
		if bytes.Compare(prefix, start) > 0 {
			start = prefix
		}
	}
	target, binding := h.Identity().ID, scanBinding(in)
	if in.Since != "" {
		k, err := resumeKey(in.Since, target, binding)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if bytes.Compare(k, start) > 0 {
			start = k
		}
	}
	out := []entry{}
	used := 0
	var next []byte
	err = kv.View(ctx, func(tx dbstore.KVTx) error {
		return tx.Scan(name, start, func(k, v []byte) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, dbstore.FromContext(err)
			}
			if len(prefix) > 0 && !bytes.HasPrefix(k, prefix) {
				return false, nil
			}
			e := entry{Key: Encode(k), Value: Encode(v)}
			b, _ := json.Marshal(e)
			if len(out) == limit || used+len(b) > maxBytes {
				if len(out) == 0 {
					return false, fmt.Errorf("%w: entry %s is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, show(k), len(b), maxBytes)
				}
				next = append([]byte(nil), k...)
				return false, nil
			}
			out = append(out, e)
			used += len(b)
			return true, nil
		})
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	p := page{Returned: len(out), Limit: limit, Truncated: next != nil, Bytes: used}
	res := dblifecycle.Outcome{Result: map[string]any{"bucket": in.Bucket, "entries": out}}
	if next != nil {
		p.Cursor = &cursor{SchemaVersion: "bashy-cursor-v1", Kind: "pagination", Token: "k:" + base64.StdEncoding.EncodeToString(next),
			Binding: cursorBinding{Provider: "db", Target: target, Query: binding}}
		res.Warnings = []string{cursorWarning}
	}
	res.Page = p
	return res, nil
}
