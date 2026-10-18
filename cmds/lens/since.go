package lenscmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// File deltas are change streams, not graph streams. --since=start reads from
// byte zero; --since=@FILE reads a reader-owned envelope. Without --cursor-out,
// JSONL ends with a {t:cursor,cursor:...} record. With it stdout is raw input
// lines and the envelope is atomically saved only after stdout succeeds.
// Readers must checkpoint downstream ingestion themselves for exactly-once use.
// Rotation/truncation expires a cursor: explicitly restart with --since=start.
// The snapshot boundary is the opened file's initial size, excluding a partial
// final line. Concurrent in-place rewriting is not a supported log operation.
type filePosition struct {
	Offset int64  `json:"offset"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Anchor string `json:"anchor"`
}

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	// Stat_t differs across targets; unsupported filesystems fail explicitly.
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		d, i := v.FieldByName("Dev"), v.FieldByName("Ino")
		number := func(v reflect.Value) uint64 {
			if v.CanUint() {
				return v.Uint()
			}
			if v.CanInt() {
				return uint64(v.Int())
			}
			return 0
		}
		if d.IsValid() && i.IsValid() {
			return number(d), number(i), nil
		}
	}
	return 0, 0, fmt.Errorf("file identity unavailable on this filesystem")
}

func fileAnchor(f *os.File, offset int64) (string, error) {
	start := max(int64(0), offset-256)
	b := make([]byte, offset-start)
	if _, err := f.ReadAt(b, start); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func sinceFile(rc *tool.RunContext, operand, format, since, cursorOut string) int {
	fail := func(err error) int { fmt.Fprintf(rc.Err, "lens: --since: %v\n", err); return 1 }
	path := rc.Path(operand)
	path, err := filepath.Abs(path)
	if err != nil {
		return fail(err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("requires a regular file"))
	}
	dev, ino, err := fileIdentity(info)
	if err != nil {
		return fail(err)
	}
	if cursorOut != "" {
		dst := rc.Path(cursorOut)
		abs, err := filepath.Abs(dst)
		if err != nil {
			return fail(err)
		}
		other, statErr := os.Stat(dst)
		if abs == path || statErr == nil && os.SameFile(info, other) {
			return fail(fmt.Errorf("cursor output must differ from input"))
		}
	}
	query := "complete-lines:" + format
	pos := filePosition{Device: dev, Inode: ino}
	if since != "start" {
		data := []byte(since)
		if len(since) > 0 && since[0] == '@' {
			data, err = os.ReadFile(rc.Path(since[1:]))
			if err != nil {
				return fail(err)
			}
		}
		c, err := stream.ParseCursor(data)
		if err != nil {
			return fail(err)
		}
		if err = c.Check("lens-file", path, query); err != nil {
			return fail(err)
		}
		if c.Kind != stream.KindChangeStream {
			return fail(fmt.Errorf("requires a change-stream cursor"))
		}
		if c.Expiry != "" {
			expiry, _ := time.Parse(time.RFC3339, c.Expiry)
			if !time.Now().Before(expiry) {
				return fail(fmt.Errorf("cursor expired; restart with --since=start"))
			}
		}
		token, err := base64.RawURLEncoding.DecodeString(c.Token)
		if err != nil {
			return fail(fmt.Errorf("invalid file cursor token"))
		}
		dec := json.NewDecoder(bytes.NewReader(token))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&pos); err != nil {
			return fail(err)
		}
		if _, err = dec.Token(); err != io.EOF {
			return fail(fmt.Errorf("invalid file cursor token suffix"))
		}
		if pos.Offset < 0 || pos.Device != dev || pos.Inode != ino || pos.Offset > info.Size() {
			return fail(fmt.Errorf("file rotated or truncated; restart with --since=start"))
		}
		anchor, err := fileAnchor(f, pos.Offset)
		if err != nil {
			return fail(err)
		}
		if anchor != pos.Anchor {
			return fail(fmt.Errorf("file rewritten or truncated; restart with --since=start"))
		}
	}
	reader := bufio.NewReader(io.NewSectionReader(f, pos.Offset, info.Size()-pos.Offset))
	var delta bytes.Buffer
	for {
		if rc.Ctx != nil {
			if err := rc.Ctx.Err(); err != nil {
				return fail(err)
			}
		}
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		} // Never checkpoint an incomplete record.
		if err != nil {
			return fail(err)
		}
		if format == "jsonl" && !json.Valid(bytes.TrimSpace(line)) {
			return fail(fmt.Errorf("invalid JSONL at byte %d", pos.Offset))
		}
		delta.Write(line)
		pos.Offset += int64(len(line))
	}
	pos.Anchor, err = fileAnchor(f, pos.Offset)
	if err != nil {
		return fail(err)
	}
	token, _ := json.Marshal(pos)
	c := stream.Cursor{SchemaVersion: stream.CursorSchema, Kind: stream.KindChangeStream, Token: base64.RawURLEncoding.EncodeToString(token), Binding: stream.Binding{Provider: "lens-file", Target: path, Query: query}}
	return emitDelta(rc, &delta, c, cursorOut, fail)
}

func emitDelta(rc *tool.RunContext, delta *bytes.Buffer, c stream.Cursor, cursorOut string, fail func(error) int) int {
	envelope, _ := json.Marshal(c)
	if cursorOut == "" {
		trailer, _ := json.Marshal(struct {
			T      string        `json:"t"`
			Cursor stream.Cursor `json:"cursor"`
		}{"cursor", c})
		delta.Write(trailer)
		delta.WriteByte('\n')
	}
	if _, err := io.Copy(rc.Out, delta); err != nil {
		return fail(err)
	}
	if cursorOut != "" {
		dst := rc.Path(cursorOut)
		tmp, err := os.CreateTemp(filepath.Dir(dst), ".lens-cursor-*")
		if err != nil {
			return fail(err)
		}
		defer os.Remove(tmp.Name())
		_, err = tmp.Write(append(envelope, '\n'))
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), dst)
		}
		if err != nil {
			return fail(err)
		}
	}
	return 0
}

// ErrDeltaExpired is returned by a DeltaSource whose position can no longer be
// resumed; the caller reports restart guidance.
var ErrDeltaExpired = errors.New("delta position expired")

// DeltaSource is an explicitly registered, read-only change feed for one
// agent-world scheme. Delta returns complete newline-terminated JSONL records
// after token ("" = from start) and the opaque token to resume from. Token and
// expiry are the source's; lens binds them to scheme/target and never invents
// records for a scheme with no source.
type DeltaSource interface {
	Delta(ctx context.Context, loc ref.Locator, token string) (records []byte, next string, expiry string, err error)
}

// TimeDeltaSource is an optional interface for a DeltaSource that can translate
// an absolute cutoff timestamp or duration into its native resume token.
type TimeDeltaSource interface {
	TranslateTime(ctx context.Context, loc ref.Locator, t time.Time) (token string, err error)
}

// SeqDeltaSource is an optional interface for a DeltaSource that can translate or
// validate a sequence shorthand into its native resume token.
type SeqDeltaSource interface {
	TranslateSeq(ctx context.Context, loc ref.Locator, seq int64) (token string, err error)
}

var deltaSources = map[string]DeltaSource{}

// RegisterDeltaSource wires a read-only delta source for an agent-world scheme.
func RegisterDeltaSource(scheme string, src DeltaSource) error {
	if !slices.Contains(provider.AgentWorldSchemes, scheme) {
		return fmt.Errorf("lens: %q is not an agent-world scheme", scheme)
	}
	if src == nil {
		return fmt.Errorf("lens: nil delta source for %q", scheme)
	}
	deltaSources[scheme] = src
	return nil
}

func isAgentWorldScheme(scheme string) bool {
	return slices.Contains(provider.AgentWorldSchemes, scheme)
}

func sinceAgentWorld(rc *tool.RunContext, loc ref.Locator, format, since, cursorOut string) int {
	fail := func(err error) int { fmt.Fprintf(rc.Err, "lens: --since: %v\n", err); return 1 }
	src, ok := deltaSources[loc.Scheme]
	if !ok {
		return fail(fmt.Errorf("no delta source registered for provider %q (no live deltas are fabricated)", loc.Scheme))
	}
	target := loc.String()
	query := "delta:" + format
	restart := fmt.Errorf("restart with --since=start")
	spec, err := ParseSinceCursor(since, loc, query, time.Now(), func(p string) ([]byte, error) {
		return os.ReadFile(rc.Path(p))
	})
	if err != nil {
		return fail(err)
	}

	var token string
	switch spec.Mode {
	case SinceModeStart:
		token = ""
	case SinceModeCursor:
		token = spec.Token
	case SinceModeSeq:
		ssrc, ok := src.(SeqDeltaSource)
		if !ok {
			return fail(fmt.Errorf("provider %q does not support sequence in --since", loc.Scheme))
		}
		var err error
		token, err = ssrc.TranslateSeq(rc.Ctx, loc, spec.Seq)
		if errors.Is(err, ErrDeltaExpired) {
			return fail(fmt.Errorf("%w; %v", err, restart))
		}
		if err != nil {
			return fail(err)
		}
	case SinceModeTime, SinceModeDuration:
		tsrc, ok := src.(TimeDeltaSource)
		if !ok {
			return fail(fmt.Errorf("provider %q does not support time/duration in --since", loc.Scheme))
		}
		var err error
		token, err = tsrc.TranslateTime(rc.Ctx, loc, spec.Time)
		if errors.Is(err, ErrDeltaExpired) {
			return fail(fmt.Errorf("%w; %v", err, restart))
		}
		if err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unsupported --since mode %q", spec.Mode))
	}

	records, next, expiry, err := src.Delta(rc.Ctx, loc, token)
	if errors.Is(err, ErrDeltaExpired) {
		return fail(fmt.Errorf("%v; %v", err, restart))
	}
	if err != nil {
		return fail(err)
	}
	if next == "" {
		return fail(fmt.Errorf("delta source returned no resume token"))
	}
	if len(records) > 0 && records[len(records)-1] != '\n' {
		return fail(fmt.Errorf("delta source returned an incomplete record"))
	}
	if format == "jsonl" {
		for _, line := range bytes.Split(bytes.TrimSuffix(records, []byte("\n")), []byte("\n")) {
			if len(records) > 0 && !json.Valid(bytes.TrimSpace(line)) {
				return fail(fmt.Errorf("delta source returned invalid JSONL"))
			}
		}
	}
	c := stream.Cursor{SchemaVersion: stream.CursorSchema, Kind: stream.KindChangeStream, Token: next, Expiry: expiry, Binding: stream.Binding{Provider: loc.Scheme, Target: target, Query: query}}
	if err := c.Validate(); err != nil {
		return fail(err)
	}
	if cursorOut != "" {
		if _, err := os.Stat(filepath.Dir(rc.Path(cursorOut))); err != nil {
			return fail(err)
		}
	}
	return emitDelta(rc, bytes.NewBuffer(append([]byte(nil), records...)), c, cursorOut, fail)
}
