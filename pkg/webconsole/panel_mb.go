// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
)

// mbSchemaVersion identifies the board payload. Bump it only for a breaking
// shape change; the page reads it and can then say so.
const mbSchemaVersion = "bashy-console-mb-v1"

// mbDefaultLimit / mbMaxLimit bound one page of board. Deliberately much larger
// than the CLI's -n 5: the cap exists there because a post costs an agent
// TOKENS at a turn boundary, and a human scrolling a browser pays neither the
// tokens nor the turn. Scanning across every lane is the whole reason this
// panel exists.
const (
	mbDefaultLimit = 200
	mbMaxLimit     = 1000
)

// mbPost is one post as the page renders it.
//
// `To` is the DISPLAY audience from Post.Audiences() — the role's label rather
// than its routing topic, "all" for a broadcast, "band 4 · tool ycode" for a
// selector — so a filter matches the string the reader can actually see.
type mbPost struct {
	Seq       int64  `json:"seq"`
	At        string `json:"at"`
	From      string `json:"from"`
	To        string `json:"to"`
	Topic     string `json:"topic,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Body      string `json:"body"`
	Directed  bool   `json:"directed"`
	Broadcast bool   `json:"broadcast"`
}

type mbFacet struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type mbFacets struct {
	From  []mbFacet `json:"from"`
	To    []mbFacet `json:"to"`
	Topic []mbFacet `json:"topic"`
}

type mbList struct {
	SchemaVersion string   `json:"schema_version"`
	Reader        string   `json:"reader"`
	SeenSeq       int64    `json:"seen_seq"`
	HighSeq       int64    `json:"high_seq"`
	Total         int      `json:"total"`
	Matched       int      `json:"matched"`
	Posts         []mbPost `json:"posts"`
	Facets        mbFacets `json:"facets"`
	Concerns      []string `json:"concerns"`
	Declared      []string `json:"declared"`
	RetentionDays int      `json:"retention_days"`
}

// handleMBList is the board, filtered.
//
// IT NEVER ADVANCES A CURSOR. bus.MarkSeen is not called here and no reader
// identity is accepted from the browser: `seen_seq` is read-only and only draws
// an "unread for me" line. A web view that marked posts read would silently eat
// an agent's mail every time somebody opened a tab — and the board's whole
// delivery model rests on a cursor meaning "a turn consumed this".
//
// It also shows the LIVE board only. Posts older than the retention window are
// rotated into ~/.bashy/mb/archive/, which has no reader; the page says so
// rather than implying this is everything ever said.
func (s *server) handleMBList(w http.ResponseWriter, r *http.Request) {
	reader, _ := s.userOf(r)
	// BoardIdentity with an explicit name is pure canonicalization through the
	// fleet catalog — the same mapping the CLI's --as goes through, so the
	// cursor this page reads is the one that agent's own reads advance.
	if canon, err := bus.BoardIdentity(reader); err == nil {
		reader = canon
	}

	posts, err := bus.Posts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	fFrom := strings.TrimSpace(q.Get("from"))
	fTo := strings.TrimSpace(q.Get("to"))
	fTopic := strings.TrimSpace(q.Get("topic"))
	fText := strings.ToLower(strings.TrimSpace(q.Get("q")))
	unreadOnly := q.Get("unread") == "1"

	limit := mbDefaultLimit
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, mbMaxLimit)
	}

	seen := bus.SeenSeq(reader)

	// Facets come from the WHOLE live board, not from the filtered slice: a
	// dropdown that lists only what the current filter already matched cannot be
	// used to change the filter.
	from, to, topic := map[string]int{}, map[string]int{}, map[string]int{}

	out := make([]mbPost, 0, limit)
	high, matched := int64(0), 0
	for _, p := range posts {
		if p.Seq > high {
			high = p.Seq
		}
		aud := p.Audiences()
		from[p.From]++
		to[aud]++
		if t := strings.TrimSpace(p.Topic); t != "" {
			topic[t]++
		}

		switch {
		case p.Seq <= since,
			fFrom != "" && !strings.EqualFold(p.From, fFrom),
			fTo != "" && !strings.EqualFold(aud, fTo),
			fTopic != "" && !strings.EqualFold(p.Topic, fTopic),
			unreadOnly && p.Seq <= seen:
			continue
		}
		if fText != "" && !strings.Contains(strings.ToLower(p.Body+" "+p.From+" "+aud+" "+p.Topic), fText) {
			continue
		}
		matched++
		out = append(out, mbPost{
			Seq: p.Seq, At: p.At, From: p.From, To: aud, Topic: p.Topic, Mode: p.Mode,
			Body: p.Body, Directed: p.Directed(reader), Broadcast: p.Broadcast(),
		})
	}

	// Newest first, then trim — the newest is what a scan wants, and trimming
	// before reversing would keep the OLDEST page.
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}

	writeJSON(w, http.StatusOK, mbList{
		SchemaVersion: mbSchemaVersion,
		Reader:        reader,
		SeenSeq:       seen,
		HighSeq:       high,
		Total:         len(posts),
		Matched:       matched,
		Posts:         out,
		Facets: mbFacets{
			From: facetList(from), To: facetList(to), Topic: facetList(topic),
		},
		Concerns:      bus.WellKnownConcerns,
		Declared:      bus.DeclaredConcerns(reader),
		RetentionDays: int(bus.RetentionWindow().Hours() / 24),
	})
}

// facetList sorts a facet by count, then name — the busiest lanes first, which
// is the order somebody scanning a board wants to pick from.
func facetList(m map[string]int) []mbFacet {
	out := make([]mbFacet, 0, len(m))
	for n, c := range m {
		if n == "" {
			continue
		}
		out = append(out, mbFacet{Name: n, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// handleMBViewers answers "who has actually read this" for ONE post.
//
// It is a separate route because Viewers and ClaimHolder are a file read per
// post: on the list path that would be one stat storm per repaint, for a
// receipt almost nobody is looking at.
func (s *server) handleMBViewers(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad seq"})
		return
	}
	viewers := bus.Viewers(seq)
	if viewers == nil {
		viewers = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seq": seq, "viewers": viewers, "holder": bus.ClaimHolder(seq),
	})
}

type mbSendBody struct {
	To    string `json:"to"`
	Topic string `json:"topic"`
	Body  string `json:"body"`
}

// handleMBSend posts from the browser, through the SAME bus.Send the CLI uses —
// so the panel cannot drift into a second delivery model with its own idea of
// resolution, ordering or receipts.
//
// The sender is derived from the request, NEVER read from the body. A browser
// that could name its own `from` could sign as any agent on the host, and
// attribution is the one thing the board guarantees.
func (s *server) handleMBSend(w http.ResponseWriter, r *http.Request) {
	var in mbSendBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request"})
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a post needs a body"})
		return
	}

	user, _ := s.userOf(r)
	from, err := bus.BoardIdentity(user)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		return
	}

	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		topic = "mb"
	}
	res, err := bus.Send(bus.SendRequest{
		From: from, To: strings.TrimSpace(in.To), Topic: topic, Body: in.Body,
	})
	if err != nil {
		// An unresolvable addressee is a 400 with the board's own message,
		// near misses and all: the sender typed something, and the honest answer
		// names what would have worked.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": mbSchemaVersion,
		"from":           from,
		"result":         res,
	})
}

// handleMBPage serves the board page.
//
// Like the terminal, it is served from the LAUNCHER's root rather than from
// under a mount, so its <base href> is the launcher's: it reuses app.css and the
// header chrome verbatim, and still composes when the console is reached through
// outpost's tunnel prefix.
func (s *server) handleMBPage(w http.ResponseWriter, r *http.Request) {
	s.servePageFile(w, r, "mb.html")
}
