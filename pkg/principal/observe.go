package principal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The catalogs say who is DECLARED to exist; the coordination stores say who
// is actually ACTING. An agent that polls the message board, sits in a meet
// roster, or holds a bus subscription left a trace on disk, and a resolver
// that reads only the catalogs answers "names nothing on this host" about a
// live seat with a hundred posts behind it. Measured on this host 2026-08-25:
// 62 distinct names in use vs ~44 in the fleet catalog, with every weave
// -w<issue> worker invisible.
//
// So observation is a source alongside the catalogs — but a subordinate one.
// A trace is evidence of EXISTENCE, not identity: the name in a cursor file
// was written by whatever process advanced it. Two rules keep the claims
// honest:
//
//   - An observed match is marked Source=observed with Inferred confidence,
//     below any catalog entry, so a caller can tell a declared identity from
//     an inferred one.
//   - Observation answers only when the catalogs name NOTHING, so a declared
//     name never changes meaning because something posted under it.
//
// The stores are read with deliberately minimal parsers, the same way
// readOutpostAgentName reads the daemon's config: this package must not
// depend on bus/meet's schema packages (several of them import it back),
// and a malformed record is simply not evidence.

// liveWindow is how recently a trace must have moved for the name behind it
// to be treated as live. A board cursor advances on every poll, so a day of
// silence means the seat is gone or wedged — either way, not something to
// rank as reachable-in-place.
const liveWindow = 24 * time.Hour

// boardSafeName mirrors how the board sanitizes a reader name into a cursor
// filename (bus's safeName + leading-dot trim). The two must agree or a
// cursor written by mb is invisible to whois.
var boardSafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// traces is what the coordination stores say about one name. The same name
// can leave agent-kind traces (cursors, posts, rosters, subscriptions) and
// person-kind traces (a meet's human seat); both are collected so ambiguity
// surfaces as ambiguity, per the exit-3 rule.
type traces struct {
	agent  [][2]string // evidence rows for an agent-kind claim
	person [][2]string // evidence rows for a person-kind claim
	last   time.Time   // newest trace, for liveness
}

func (t *traces) bump(at time.Time) {
	if at.After(t.last) {
		t.last = at
	}
}

// live reports whether the newest trace is fresh enough to call the
// principal behind it running right now.
func (t traces) live(now time.Time) bool {
	return !t.last.IsZero() && now.Sub(t.last) <= liveWindow
}

// observe reads every observation store for one name. Best-effort and
// read-only throughout: a missing store, an unreadable file, or a malformed
// record is an empty answer, never an error — resolving must work on a host
// that has never run a board or a meet.
func observe(env Env, name string) traces {
	var t traces
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return t
	}
	observeBoard(env.BoardDir, name, &t)
	observeMeets(env.MeetDir, name, &t)
	observeSubs(env.RoomDir, name, &t)
	return t
}

// observeBoard reads the message board: a seen cursor means the name POLLS —
// the strongest liveness signal there is — and an authored post means it
// spoke.
func observeBoard(dir, name string, t *traces) {
	if dir == "" {
		return
	}
	cursor := strings.TrimLeft(boardSafeName.ReplaceAllString(strings.TrimSpace(name), "_"), ".")
	if cursor != "" {
		if fi, err := os.Stat(filepath.Join(dir, "seen", cursor)); err == nil && !fi.IsDir() {
			t.agent = append(t.agent, [2]string{"mb cursor", "last poll " + fi.ModTime().UTC().Format(time.RFC3339)})
			t.bump(fi.ModTime())
		}
	}
	if n, last := boardPostsBy(filepath.Join(dir, "posts.jsonl"), name); n > 0 {
		v := strconv.Itoa(n) + " authored"
		if !last.IsZero() {
			v += ", last " + last.UTC().Format(time.RFC3339)
		}
		t.agent = append(t.agent, [2]string{"mb posts", v})
		t.bump(last)
	}
}

// boardPostsBy counts the posts one author wrote and when the newest landed.
func boardPostsBy(path, name string) (int, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}
	}
	defer f.Close()

	var n int
	var last time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		// Only the two fields that matter — not the board schema.
		var p struct {
			From string    `json:"from"`
			At   time.Time `json:"at"`
		}
		if json.Unmarshal(sc.Bytes(), &p) != nil || !strings.EqualFold(p.From, name) {
			continue
		}
		n++
		if p.At.After(last) {
			last = p.At
		}
	}
	return n, last
}

// observeMeets walks the meet store's state files. A participant or
// secretary seat is agent evidence; the human seat is person evidence —
// meets are the one store that says which kind a name acted as.
func observeMeets(dir, name string, t *traces) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var agentN, personN int
	var agentLast, personLast string // the id of the newest matching meet
	var agentAt, personAt time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "state.json")
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			ID           string   `json:"id"`
			Participants []string `json:"participants"`
			Secretary    string   `json:"secretary"`
			Human        string   `json:"human"`
		}
		if json.Unmarshal(data, &st) != nil {
			continue
		}
		id := st.ID
		if id == "" {
			id = e.Name()
		}
		var at time.Time
		if fi, err := os.Stat(p); err == nil {
			at = fi.ModTime()
		}
		seated := strings.EqualFold(st.Secretary, name)
		for _, part := range st.Participants {
			if strings.EqualFold(part, name) {
				seated = true
				break
			}
		}
		if seated {
			agentN++
			if at.After(agentAt) || agentLast == "" {
				agentAt, agentLast = at, id
			}
			t.bump(at)
		}
		if strings.EqualFold(st.Human, name) {
			personN++
			if at.After(personAt) || personLast == "" {
				personAt, personLast = at, id
			}
			t.bump(at)
		}
	}
	if agentN > 0 {
		t.agent = append(t.agent, [2]string{"meet roster", strconv.Itoa(agentN) + " meet(s), latest " + agentLast})
	}
	if personN > 0 {
		t.person = append(t.person, [2]string{"meet human", strconv.Itoa(personN) + " meet(s), latest " + personLast})
	}
}

// observeSubs checks for a standing bus subscription held under the name.
func observeSubs(dir, name string, t *traces) {
	if dir == "" {
		return
	}
	if fi, err := os.Stat(filepath.Join(dir, "subs", name+".json")); err == nil && !fi.IsDir() {
		t.agent = append(t.agent, [2]string{"bus subscription", "since " + fi.ModTime().UTC().Format(time.RFC3339)})
		t.bump(fi.ModTime())
	}
}

// --- the observed fallback rungs -----------------------------------------

// observedAgent resolves a name the catalogs do not know from its agent-kind
// traces. The one contact is the board: a directed post reaches the seat the
// next time it polls, needs no binding, and is the very channel the evidence
// came from.
func (r *Resolver) observedAgent(name string, t traces) (Resolution, bool) {
	if len(t.agent) == 0 {
		return Resolution{}, false
	}
	res := Resolution{
		URN: URN(KindAgent, name, r.owner), Kind: KindAgent, Name: name,
		Owner: r.owner, Source: SourceObserved, Confidence: Inferred,
		Summary: "observed on this host, not in the fleet catalog",
		Facts:   t.agent,
	}
	c := Contact{
		Method: "mb", Address: "bashy mb send " + name + " <message>",
		Source: SourceObserved, Confidence: Observed, Live: true, Cost: 15,
	}
	if !t.live(time.Now()) {
		c.Confidence = Inferred
		c.Why = "no trace inside " + liveWindow.String() + " — the seat may no longer be polling"
	}
	res.Contacts = []Contact{c}
	return res, true
}

// observedPerson resolves a name only a meet's human seat vouches for.
func (r *Resolver) observedPerson(name string, t traces) (Resolution, bool) {
	if len(t.person) == 0 {
		return Resolution{}, false
	}
	return Resolution{
		URN: URN(KindPerson, name, r.owner), Kind: KindPerson, Name: name,
		Owner: r.owner, Source: SourceObserved, Confidence: Inferred,
		Summary: "observed on this host, not in the people catalog",
		Facts:   t.person,
		Contacts: []Contact{{
			Method: "mention", Address: "@" + name,
			Source: SourceObserved, Confidence: Inferred, Live: true, Cost: 1,
		}},
	}, true
}
