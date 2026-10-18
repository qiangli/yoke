package adminclients

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// Bounds. Exhaustion is an explicit output-limit error, never truncation.
const (
	MaxOutputBytes = 4 << 20
	DefaultTimeout = 60 * time.Second
)

// Run executes an admitted, validated invocation once with bounded stdout
// (enforced while reading) and a deadline. stderr and errors are redacted
// against the given secrets. It never retries: an ambiguous write is not
// repeated.
func Run(ctx context.Context, bin string, argv, env []string, secrets ...string) ([]byte, *gws.Error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, gws.NewError(gws.CodeProviderFailure, "stdout pipe: "+err.Error(), nil)
	}
	if err := cmd.Start(); err != nil {
		return nil, gws.NewError(gws.CodeProviderFailure, gws.RedactText(err.Error(), secrets...), nil)
	}
	data, rerr := io.ReadAll(io.LimitReader(out, MaxOutputBytes+1))
	over := len(data) > MaxOutputBytes
	if over {
		_ = cmd.Process.Kill()
	}
	werr := cmd.Wait()
	op := filepathBase(bin)
	switch {
	case over:
		return nil, gws.NewError(gws.CodeOutputLimit, fmt.Sprintf("%s: output exceeds %d bytes; narrow the query", op, MaxOutputBytes), nil)
	case errors.Is(ctx.Err(), context.Canceled):
		return nil, gws.NewError(gws.CodeCancelled, op+" cancelled; remote outcome unknown", nil)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, gws.NewError(gws.CodeTimeout, op+" timed out; remote outcome unknown", nil)
	case rerr != nil:
		return nil, gws.NewError(gws.CodeProviderFailure, op+": read failed", nil)
	case werr != nil:
		msg := strings.TrimSpace(gws.RedactText(stderr.String(), secrets...))
		return nil, gws.NewError(classifyStderr(msg), fmt.Sprintf("%s failed: %s", op, msg), nil)
	}
	return data, nil
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".exe")
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// classifyStderr maps the documented failure phrases of both CLIs onto the
// shared typed codes; anything unrecognized is a provider failure.
func classifyStderr(msg string) gws.Code {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "insufficient privileges"), strings.Contains(l, "authorization_requestdenied"), strings.Contains(l, "not authorized"), strings.Contains(l, "forbidden"):
		return gws.CodePermissionDenied
	case strings.Contains(l, "expired"), strings.Contains(l, "invalid_grant"), strings.Contains(l, "unauthenticated"):
		return gws.CodeExpiredCredentials
	case strings.Contains(l, "does not exist"), strings.Contains(l, "not found"), strings.Contains(l, "resource not found"):
		return gws.CodeNotFound
	case strings.Contains(l, "too many requests"), strings.Contains(l, "rate limit"), strings.Contains(l, "quota"):
		return gws.CodeRateLimit
	}
	return gws.CodeProviderFailure
}

// ParseRecords turns CLI stdout into verbatim JSON records: m365 prints a
// JSON array (or a single object); gam prints CSV with a header row.
func ParseRecords(client string, data []byte) ([]json.RawMessage, *gws.Error) {
	bad := func(m string) *gws.Error {
		return gws.NewError(gws.CodeProviderFailure, client+": unparseable output: "+m, nil)
	}
	switch client {
	case "m365":
		t := bytes.TrimSpace(data)
		if len(t) == 0 {
			return []json.RawMessage{}, nil
		}
		if t[0] == '{' {
			return []json.RawMessage{json.RawMessage(t)}, nil
		}
		var recs []json.RawMessage
		if err := json.Unmarshal(t, &recs); err != nil {
			return nil, bad("not a JSON array")
		}
		return recs, nil
	case "gam":
		r := csv.NewReader(bytes.NewReader(data))
		rows, err := r.ReadAll()
		if err != nil {
			return nil, bad("not CSV")
		}
		if len(rows) == 0 {
			return []json.RawMessage{}, nil
		}
		hdr := rows[0]
		recs := make([]json.RawMessage, 0, len(rows)-1)
		for _, row := range rows[1:] {
			m := make(map[string]string, len(hdr))
			for i, h := range hdr {
				if i < len(row) {
					m[h] = row[i]
				}
			}
			b, _ := json.Marshal(m)
			recs = append(recs, b)
		}
		return recs, nil
	}
	return nil, bad("unknown client")
}

// ListPage parses a listing and returns one bounded page. The token binds to
// (target, op, query); reuse across a different binding is cursor-mismatch.
// Offset tokens assume a stable directory: mutable listings may duplicate or
// omit records between pages (no provider snapshot exists).
func ListPage(client string, s Scope, op, query string, data []byte, token string, pageSize int) (gws.Page, *gws.Error) {
	recs, e := ParseRecords(client, data)
	if e != nil {
		return gws.Page{}, e
	}
	b := gws.Binding{Target: s.Target, Query: op + " " + query}
	off, e := gws.ResumeOffset(token, b)
	if e != nil {
		return gws.Page{}, e
	}
	return gws.Paginate(recs, off, pageSize, b)
}
