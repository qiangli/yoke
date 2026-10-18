// Bounded JSON/NDJSON pagination for gws reads (Sprint 457 story f25d7aa4437c).
//
// Reads emit versioned typed JSON for finite results and versioned NDJSON
// for streams through the shared native envelope loom-v2 (the single
// `schema_version` discriminator). Page tokens are the provider's opaque
// continuation strings, carried verbatim: this package defines NO cursor
// envelope of its own. The concrete cursor serialization is bashy-cursor-v1,
// owned by 446 S19; until it lands here, tokens are bound to their
// (target, query, revision) by Binding.Validate, which rejects cross-query
// reuse with a typed cursor-mismatch error and an explicit restart/resync
// choice — never silent replay or checkpoint reset. Timestamps, durations,
// and sequence numbers are request conveniences, not cursor tokens, and are
// never converted across providers.
//
// Bounds are enforced while reading upstream output, not after buffering:
// MaxPageSize caps one page, MaxTotal caps a full walk, and exhaustion is
// explicit (partial status + resume token) rather than silent truncation.
package gws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// encodeOffsetToken mints an opaque resume token for this binding. The
// format is package-internal and never parsed by consumers: treat it as an
// opaque string. A binding digest is embedded so a token presented for a
// different (target, query, revision) fails closed at decode time.
func encodeOffsetToken(b Binding, offset int) string {
	sum := sha256.Sum256([]byte(b.Target + "\x00" + b.Query + "\x00" + b.Revision))
	return "gws1." + strconv.Itoa(offset) + "." + hex.EncodeToString(sum[:])[:16]
}

// offsetFromToken decodes a package-issued token, rejecting foreign shapes
// and cross-binding reuse with a typed cursor-mismatch.
func offsetFromToken(token string, b Binding) (int, *Error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "gws1" {
		return 0, NewError(CodeCursorMismatch, "gws: unrecognized page token — restart the listing explicitly", nil)
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil || offset < 0 {
		return 0, NewError(CodeCursorMismatch, "gws: malformed page token — restart the listing explicitly", nil)
	}
	if parts[2] != bindingDigest(b) {
		return 0, NewError(CodeCursorMismatch, fmt.Sprintf("gws: page token is not bound to target=%q query=%q revision=%q — restart the listing explicitly", b.Target, b.Query, b.Revision), nil)
	}
	return offset, nil
}

func bindingDigest(b Binding) string {
	sum := sha256.Sum256([]byte(b.Target + "\x00" + b.Query + "\x00" + b.Revision))
	return hex.EncodeToString(sum[:])[:16]
}

// Pagination bounds. Finite defaults per operation; large artifacts use
// locator references, not unbounded inline text.
const (
	// DefaultPageSize is the default records per page.
	DefaultPageSize = 50
	// MaxPageSize caps a single page.
	MaxPageSize = 200
	// MaxTotal caps a full paginated walk.
	MaxTotal = 2000
)

// Binding ties an opaque provider token to the explicit target, normalized
// query/options, and revision/snapshot where available.
type Binding struct {
	Target   string
	Query    string
	Revision string
}

// Validate rejects reuse of a token outside the binding it was issued for.
// An empty token starts a walk; a token presented with a different binding
// is a typed cursor-mismatch (restart/resync explicitly, never silently).
func (b Binding) Validate(token string, issuedFor Binding) *Error {
	if token == "" {
		return nil
	}
	if issuedFor != b {
		return NewError(CodeCursorMismatch, fmt.Sprintf("gws: page token was issued for target=%q query=%q revision=%q and cannot resume target=%q query=%q revision=%q — restart the listing explicitly", issuedFor.Target, issuedFor.Query, issuedFor.Revision, b.Target, b.Query, b.Revision), nil)
	}
	return nil
}

// Page is one bounded result page. Items are provider records preserved
// verbatim (precision, nulls, encodings); NextToken is the opaque provider
// continuation ("": the listing is complete).
type Page struct {
	Items     []json.RawMessage
	NextToken string
	Complete  bool
	Total     int
}

// Paginate slices records into one bounded page: at most pageSize items
// from offset, with an opaque next token encoding only the resume offset
// for THIS (target, query) walk. Callers persist the Binding alongside the
// token and re-validate before reuse. Offset tokens assume a stable
// listing: on mutable APIs without a revision/snapshot, pages may duplicate
// or omit records — state that risk at the call site, never claim stable
// snapshots.
func Paginate(records []json.RawMessage, offset, pageSize int, b Binding) (Page, *Error) {
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		return Page{}, NewError(CodeOutputLimit, fmt.Sprintf("gws: page size %d exceeds max %d", pageSize, MaxPageSize), nil)
	}
	if offset < 0 || offset > len(records) {
		return Page{}, NewError(CodeCursorMismatch, fmt.Sprintf("gws: resume offset %d out of range (0..%d) — restart the listing explicitly", offset, len(records)), nil)
	}
	if len(records) > MaxTotal {
		return Page{}, NewError(CodeOutputLimit, fmt.Sprintf("gws: listing of %d records exceeds max total %d — narrow the query", len(records), MaxTotal), nil)
	}
	end := offset + pageSize
	if end > len(records) {
		end = len(records)
	}
	p := Page{Items: records[offset:end], Total: len(records), Complete: end == len(records)}
	if !p.Complete {
		p.NextToken = encodeOffsetToken(b, end)
	}
	if p.Items == nil {
		p.Items = []json.RawMessage{}
	}
	return p, nil
}

// ResumeOffset decodes a token issued by Paginate for this binding.
// Empty starts a walk; foreign or cross-binding tokens fail typed.
func ResumeOffset(token string, b Binding) (int, *Error) {
	if token == "" {
		return 0, nil
	}
	return offsetFromToken(token, b)
}

// EmitPage writes one page as a loom-v2 ok envelope (`schema_version:
// loom-v2`, status ok/partial). An incomplete walk reports status partial
// with the resume token; a complete one reports ok.
func EmitPage(w io.Writer, command string, p Page) int {
	status := "ok"
	if !p.Complete {
		status = "partial"
	}
	out, err := json.Marshal(struct {
		SchemaVersion string            `json:"schema_version"`
		Command       string            `json:"command"`
		Status        string            `json:"status"`
		Items         []json.RawMessage `json:"items"`
		Total         int               `json:"total"`
		NextToken     string            `json:"next_page_token,omitempty"`
	}{
		SchemaVersion: weavecli.SchemaVersion,
		Command:       command,
		Status:        status,
		Items:         p.Items,
		Total:         p.Total,
		NextToken:     p.NextToken,
	})
	if err != nil {
		return weavecli.EmitError(w, weavecli.OutputJSON, command, weavecli.ExitGenericFail, err)
	}
	_, _ = fmt.Fprintln(w, string(out))
	return weavecli.ExitOK
}

// EmitNDJSON writes items as versioned NDJSON: one loom-v2 envelope per
// line, each carrying the same `schema_version` discriminator. A missing
// terminal/completion indication must never be read as success — consumers
// check the final Complete marker line.
func EmitNDJSON(w io.Writer, command string, items []json.RawMessage, complete bool) int {
	for _, it := range items {
		out, err := json.Marshal(struct {
			SchemaVersion string          `json:"schema_version"`
			Command       string          `json:"command"`
			Status        string          `json:"status"`
			Result        json.RawMessage `json:"result"`
		}{
			SchemaVersion: weavecli.SchemaVersion,
			Command:       command,
			Status:        "ok",
			Result:        it,
		})
		if err != nil {
			return weavecli.EmitError(w, weavecli.OutputJSON, command, weavecli.ExitGenericFail, err)
		}
		if _, err := fmt.Fprintln(w, string(out)); err != nil {
			return weavecli.EmitError(w, weavecli.OutputJSON, command, weavecli.ExitGenericFail, err)
		}
	}
	marker, err := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Complete      bool   `json:"complete"`
	}{
		SchemaVersion: weavecli.SchemaVersion,
		Command:       command,
		Status:        "ok",
		Complete:      complete,
	})
	if err != nil {
		return weavecli.EmitError(w, weavecli.OutputJSON, command, weavecli.ExitGenericFail, err)
	}
	if _, err := fmt.Fprintln(w, string(marker)); err != nil {
		return weavecli.EmitError(w, weavecli.OutputJSON, command, weavecli.ExitGenericFail, err)
	}
	return weavecli.ExitOK
}
