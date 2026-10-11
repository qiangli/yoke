package bus

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SprintScope is the one rule for "is this message about sprint N".
//
// A manager seat that reuses an agent identity inherits that identity's whole
// stale backlog, so an unscoped unread count says nothing about THIS sprint.
// The scope is deliberately an allow-list of exact, bounded signals — never a
// free-text search — because ids and timestamps are full of digit runs that a
// loose `419` match would swallow.
//
// A message is relevant when ANY of these holds:
//
//   - it is addressed to conductor:N (To) or its topic conductor.N (Topic);
//   - it arrived in the sprint's meet room (Room, or the contact Topic);
//   - its body carries the sprint UUID, a story id (a bounded token), or a story
//     number written as #N / "story N" / "story #N";
//   - its body says "Sprint N", "Sprint #N" or "S<N>".
//
// Title keywords are NOT a signal: they narrow within a scope (the reader's
// --search) and never select on their own.
//
// Pure: no I/O, no clock. The reader and `sprint show` build one scope from the
// sprint card and share this predicate, so they cannot disagree.
type SprintScope struct {
	// ID is the sprint number (conductor:ID, "Sprint ID", "S<ID>").
	ID int64
	// UUID is the sprint's cross-host identity.
	UUID string
	// RoomRef is the meet room ID of the sprint's contact (never the reusable
	// short room number). ContactTopic is the contact's bus topic.
	RoomRef      string
	ContactTopic string
	// StoryIDs are the ids of the sprint's stories; StoryNumbers their short
	// running numbers.
	StoryIDs     []string
	StoryNumbers []int
	// Since drops messages older than the sprint start. Zero means no bound; a
	// message whose TS does not parse is never dropped on time alone.
	Since time.Time
}

// SprintMessage is the minimal field set the rule reads, so the same predicate
// serves a pending record, an inbox item and a timeline event.
type SprintMessage struct {
	TS, To, Topic, Room, Body string
}

// Relevant reports whether m belongs to the scope.
func (sc SprintScope) Relevant(m SprintMessage) bool {
	if sc.ID <= 0 && sc.UUID == "" {
		return false
	}
	if !sc.Since.IsZero() {
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(m.TS)); err == nil && ts.Before(sc.Since) {
			return false
		}
	}
	if sc.addressed(m) {
		return true
	}
	return sc.mentions(m.Body)
}

// RelevantPending applies the rule to a pending record.
func (sc SprintScope) RelevantPending(p Pending) bool {
	return sc.Relevant(SprintMessage{TS: p.TS, To: p.To, Topic: p.Topic, Room: p.Room, Body: p.Body})
}

// RelevantItem applies the rule to an inbox item.
func (sc SprintScope) RelevantItem(it InboxItem) bool {
	return sc.Relevant(SprintMessage{TS: it.TS, To: it.To, Topic: it.Topic, Room: it.Room, Body: it.Body})
}

func (sc SprintScope) addressed(m SprintMessage) bool {
	if sc.ID > 0 {
		n := strconv.FormatInt(sc.ID, 10)
		if strings.EqualFold(strings.TrimSpace(m.To), "conductor:"+n) ||
			strings.EqualFold(strings.TrimSpace(m.Topic), "conductor."+n) {
			return true
		}
	}
	if r := strings.TrimSpace(sc.RoomRef); r != "" && strings.TrimSpace(m.Room) == r {
		return true
	}
	if t := strings.TrimSpace(sc.ContactTopic); t != "" && strings.TrimSpace(m.Topic) == t {
		return true
	}
	return false
}

var (
	sprintWordRef = regexp.MustCompile(`(?i)sprint[ \t]*#?([0-9]+)`)
	sprintShortRe = regexp.MustCompile(`(?i)s([0-9]+)`)
	storyNumRe    = regexp.MustCompile(`(?i)(story[ \t]*[:#]?[ \t]*#?|#)([0-9]+)`)
	sprintBeforeR = regexp.MustCompile(`(?i)sprint[ \t]*$`)
)

func (sc SprintScope) mentions(body string) bool {
	if body == "" {
		return false
	}
	lower := strings.ToLower(body)
	if u := strings.ToLower(strings.TrimSpace(sc.UUID)); len(u) >= 8 && strings.Contains(lower, u) {
		return true
	}
	for _, id := range sc.StoryIDs {
		if id = strings.ToLower(strings.TrimSpace(id)); len(id) >= 6 && hasToken(lower, id) {
			return true
		}
	}
	if sc.ID > 0 {
		want := strconv.FormatInt(sc.ID, 10)
		for _, re := range []*regexp.Regexp{sprintWordRef, sprintShortRe} {
			for _, m := range re.FindAllStringSubmatchIndex(body, -1) {
				if boundedNumber(body, m[0], m[1], body[m[2]:m[3]], want) {
					return true
				}
			}
		}
	}
	if len(sc.StoryNumbers) > 0 {
		for _, m := range storyNumRe.FindAllStringSubmatchIndex(body, -1) {
			// "Sprint #319" names a sprint, never story 319.
			if sprintBeforeR.MatchString(body[:m[0]]) {
				continue
			}
			digits := body[m[4]:m[5]]
			n, err := strconv.Atoi(digits)
			if err != nil || !boundedNumber(body, m[0], m[1], digits, digits) {
				continue
			}
			for _, sn := range sc.StoryNumbers {
				if sn == n {
					return true
				}
			}
		}
	}
	return false
}

// boundedNumber accepts a digit run only when it is exactly want and sits on
// token boundaries: not glued to a preceding word character and not followed by
// one, so "a419", "S4190" and "Sprint 419x" never match "419".
func boundedNumber(s string, start, end int, digits, want string) bool {
	if digits != want {
		return false
	}
	if start > 0 && isWordByte(s[start-1]) {
		return false
	}
	if end < len(s) && isWordByte(s[end]) {
		return false
	}
	return true
}

// hasToken reports whether tok appears in s delimited by non-identifier bytes
// (hyphen included, so a UUID's trailing segment is not mistaken for an id).
func hasToken(s, tok string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(tok)
		if (a == 0 || !isTokenByte(s[a-1])) && (b >= len(s) || !isTokenByte(s[b])) {
			return true
		}
		i = a + 1
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isTokenByte(c byte) bool { return isWordByte(c) || c == '-' }
