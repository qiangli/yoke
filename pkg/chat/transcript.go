package chat

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/secrets"
)

// This file owns the chat: ref kind (Sprint 284 M1): the session transcript
// store behind `kb show chat:<id>`.
//
// A transcript is the recorded session text at
// `<TranscriptRoot>/<session-id>.md`, written by the session layer and read
// here through the existing secret gate before anyone else sees it. Showing
// renders through this reader; nothing is copied into any kb ring. The
// embedding shell (bashy) wires RegisterRefs into its registry; kb show
// renders through ReadTranscript.

// DefaultTranscriptRetention mirrors the room timeline default: a transcript
// older than this reads `expired`, never a current fact. A session id the
// store never held reads `not-found`.
const DefaultTranscriptRetention = 7 * 24 * time.Hour

// TranscriptRetention reads $BASHY_CHAT_RETENTION (a Go duration string, e.g.
// "168h"), falling back to DefaultTranscriptRetention.
func TranscriptRetention() time.Duration {
	if v := strings.TrimSpace(os.Getenv("BASHY_CHAT_RETENTION")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTranscriptRetention
}

// TranscriptRoot resolves the transcript store:
//
//	$BASHY_CHAT_TRANSCRIPTS  the specific override, most precise
//	$BASHY_HOME/sessions/transcripts  the whole bashy home relocated
//	~/.bashy/sessions/transcripts     the default
func TranscriptRoot() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_CHAT_TRANSCRIPTS")); d != "" {
		return d
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "sessions", "transcripts")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(os.TempDir(), "bashy-sessions-transcripts")
	}
	return filepath.Join(home, ".bashy", "sessions", "transcripts")
}

// ErrTranscriptExpired marks a transcript the store holds but retention has
// passed: resolved but stale, shown as `expired` with no content — never a
// current fact, and never "not found".
var ErrTranscriptExpired = errors.New("chat: transcript past retention")

// Transcript is one session transcript after the redaction gate.
type Transcript struct {
	// Content is the post-redaction bytes. Empty when Withheld.
	Content string
	// Redacted lists the masked spans, one entry per span
	// ("secret span: lines 12-14"), in transcript order.
	Redacted []string
	// Withheld reports a fully-withheld transcript: every byte was secret
	// material, so the caller shows `redacted` with no content.
	Withheld bool
}

// transcriptPath maps a session id onto its transcript file. An id that
// cannot name a file inside the store (empty, traversal) is definitionally
// absent from it.
func transcriptPath(root, id string, allowDefault bool) (string, error) {
	if allowDefault && strings.TrimSpace(root) == "" {
		root = TranscriptRoot()
	}
	safe := transcriptSafeName(strings.TrimSpace(id))
	if safe == "" {
		return "", fmt.Errorf("chat: transcript %q: %w", id, ref.ErrNotFound)
	}
	return filepath.Join(root, safe+".md"), nil
}

// transcriptSafeName flattens a session id into a single path component so a
// caller-supplied id can never escape the transcripts slot.
func transcriptSafeName(id string) string {
	if id == "" || id == "." || id == ".." {
		return ""
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "\x00") {
		return ""
	}
	return id
}

// ReadTranscript renders one session transcript through the secret gate. root
// "" resolves through TranscriptRoot; r nil behaves as an empty redactor (no
// values to mask). A never-held id is ref.ErrNotFound; a past-retention id is
// ErrTranscriptExpired; an unreadable store is its own error.
func ReadTranscript(root, id string, r *secrets.Redactor) (Transcript, error) {
	path, err := transcriptPath(root, id, true)
	if err != nil {
		return Transcript{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Transcript{}, fmt.Errorf("chat: transcript %q: %w", id, ref.ErrNotFound)
		}
		return Transcript{}, err
	}
	if age := time.Since(fi.ModTime()); age > TranscriptRetention() {
		return Transcript{}, fmt.Errorf("chat: transcript %q is %s old (retention %s): %w",
			id, age.Round(time.Second), TranscriptRetention(), ErrTranscriptExpired)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Transcript{}, err
	}
	if r == nil {
		r = secrets.NewRedactor()
	}
	redacted := r.Redact(raw)
	spans := redactedSpans(redacted)
	if len(spans) == 0 {
		return Transcript{Content: string(redacted)}, nil
	}
	if len(bytes.TrimSpace(redactMarker.ReplaceAll(redacted, nil))) == 0 {
		return Transcript{Redacted: spans, Withheld: true}, nil
	}
	return Transcript{Content: string(redacted), Redacted: spans}, nil
}

// redactMarker matches the placeholders the secret gate emits
// ([redacted:<name>]); secret names cannot contain ']', so the match cannot
// run past one placeholder.
var redactMarker = regexp.MustCompile(`\[redacted:[^\]]*\]`)

// redactedSpans lists the masked spans in transcript order as
// "secret span: lines X-Y" (one line: "secret span: line X"), the entries kb
// show carries in its `redacted` field.
func redactedSpans(redacted []byte) []string {
	locs := redactMarker.FindAllIndex(redacted, -1)
	if len(locs) == 0 {
		return nil
	}
	lineOf := func(off int) int { return bytes.Count(redacted[:off], []byte("\n")) + 1 }
	var out []string
	start, prev := lineOf(locs[0][0]), lineOf(locs[0][0])
	flush := func() {
		if start == prev {
			out = append(out, fmt.Sprintf("secret span: line %d", start))
		} else {
			out = append(out, fmt.Sprintf("secret span: lines %d-%d", start, prev))
		}
	}
	for _, loc := range locs[1:] {
		line := lineOf(loc[0])
		if line == prev || line == prev+1 {
			prev = line
			continue
		}
		flush()
		start, prev = line, line
	}
	flush()
	return out
}

// RegisterRefs installs the chat-session resolver into the uniform-ref
// registry, over the transcript store at root ("" resolves through
// TranscriptRoot — the same ladder every reader uses). The id is the chat
// session id. A past-retention id still resolves here: the node names the
// record, and the read reports it expired.
func RegisterRefs(g *ref.Registry, root string) {
	g.Register(ref.Chat, ref.ResolverFunc(func(id string) (ref.Node, error) {
		return resolveChat(root, id)
	}))
}

func resolveChat(root, id string) (ref.Node, error) {
	path, err := transcriptPath(root, id, true)
	if err != nil {
		return ref.Node{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ref.Node{}, fmt.Errorf("chat: transcript %q: %w", id, ref.ErrNotFound)
		}
		return ref.Node{}, err
	}
	n := ref.NewNode(ref.Chat, strings.TrimSpace(id))
	n.Title = fmt.Sprintf("%d bytes, %s", fi.Size(), fi.ModTime().UTC().Format(time.RFC3339))
	n.Status = "transcript"
	n.Where = path
	n.Open = "bashy kb show chat:" + strings.TrimSpace(id)
	return n, nil
}
