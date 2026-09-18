package meet

// Relay direct messages adapt the governed `bashy chat -m` primitive to the
// web conversation surface. They are deliberately not meetings: no agenda,
// chair, secretary, decisions, or minutes. Chat remains the execution/context
// owner; this file stores only the human-visible transcript Relay must replay.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/room"
)

type relayDM struct {
	Agent   string    `json:"agent"`
	Human   string    `json:"human"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

type relayDMEvent struct {
	ID      string    `json:"id"`
	Speaker string    `json:"speaker"`
	Role    string    `json:"role"`
	Text    string    `json:"text"`
	TS      time.Time `json:"ts"`
	Status  string    `json:"status,omitempty"`
	// Kind and Retracts are carried through from the underlying record for ONE
	// reason: a withdrawal has to look like a withdrawal here too. A chat
	// projects every event onto user/assistant, and under that projection a
	// retraction arrives as an ordinary message from the human and the message
	// it withdraws still reads as live — which is the failure the whole feature
	// exists to prevent, in the surface where a person is most likely to be
	// reading.
	Kind     string `json:"kind,omitempty"`
	Retracts string `json:"retracts,omitempty"`
	// Raw carries the agent's un-normalized output when the reader asked to see
	// the transport (?raw=1). Never stored; see renderEvent.
	Raw string `json:"raw,omitempty"`
}

type relayDMDetail struct {
	State  relayDM        `json:"state"`
	Events []relayDMEvent `json:"events"`
}

var relayDMLocks sync.Map // canonical agent -> *sync.Mutex

var invokeRelayDM = chat.Invoke
var startRelayDMWork = chat.Start

// trustedBashyWorkContainment is deliberately fail-closed. A generic OCI marker
// proves only that this process is in some container; it says nothing about
// host/workspace mounts or the restricted Bashy sandbox contract. There is no
// trustworthy Bashy containment provenance available to this process today, so
// production Start work refuses until an existing trusted runner can vouch for
// it. Tests replace this seam to exercise the already-existing managed session.
var trustedBashyWorkContainment = func() (bool, string) {
	return false, "no trusted Bashy containment provenance is available"
}

func relayDMLock(agent string) *sync.Mutex {
	v, _ := relayDMLocks.LoadOrStore(agent, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// dmRoomID resolves the ONE durable room two seats share.
//
// This used to be <meetdir>/relay-dms/<agent>/ — a private store nothing else
// read. That was the bug: `bashy meet dm` (the CLI) already writes to the
// permanent dm-<a>-<b> room, which the unified inbox reads as source "meet",
// while the web DM wrote here, which it does not. So one name had two mailboxes
// and only one of them was mail: a web DM was invisible to `bashy inbox --as X`,
// to reachability, and to any spawn that reads its inbox before answering.
//
// The fix is to delete a store rather than add an inbox source. A fourth source
// would have given one name two mailboxes ON PURPOSE.
func dmRoomID(agent, human string) (string, error) {
	name, err := directMessageRoomName(human, agent)
	if err != nil {
		return "", err
	}
	return name, nil
}

// relayDMDir is the room's own store, so the observe tail keeps working
// unchanged: a meet room already keeps its transcript at
// <meetdir>/<id>/transcript.jsonl, which is exactly what this returned before.
func relayDMDir(agent string) (string, error) {
	st, err := dmRoomFor(agent, "")
	if err != nil {
		return "", err
	}
	return storeDir(st.ID)
}

// dmRoomFor loads, or creates, the durable room for this agent and human.
func dmRoomFor(agent, human string) (*State, error) {
	agent = canonAgent(agent)
	if err := routableSeat(agent); err != nil {
		return nil, err
	}
	if strings.TrimSpace(human) == "" {
		human = humanName()
	}
	name, err := dmRoomID(agent, human)
	if err != nil {
		return nil, err
	}
	return EnsurePermanentRoom(name, CreateOptions{
		Topic:        "DM with " + agent,
		Participants: []string{agent},
		Human:        human,
		Initiator:    human,
		// A DM is a conversation, not a meeting: no chair runs the floor and
		// nobody files minutes about two people talking.
		Secretary: "",
	})
}

func ensureRelayDM(agent, human string) (relayDM, error) {
	st, err := dmRoomFor(agent, human)
	if err != nil {
		return relayDM{}, err
	}
	who := strings.TrimSpace(st.Human)
	if who == "" {
		who = human
	}
	return relayDM{Agent: canonAgent(agent), Human: who, Created: st.Created, Updated: nowFn()}, nil
}

func relayDMEvents(agent string, debugRaw bool) ([]relayDMEvent, error) {
	st, err := dmRoomFor(agent, "")
	if err != nil {
		return nil, err
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		return nil, err
	}
	out := make([]relayDMEvent, 0, len(events))
	for i, e := range events {
		role := "assistant"
		if e.Kind == "human" || strings.EqualFold(e.Speaker, st.Human) {
			role = "user"
		}
		status := ""
		if e.Status != "" {
			status = e.Status
		}
		// Normalize at RENDER as well as at capture: a DM recorded before this
		// build (or by a build that did not know the tool's transport) still holds
		// the raw envelope on disk, and the record is not rewritten to fix a
		// display. See renderEvent.
		text := normalizeTurnText(e.Text)
		ev := relayDMEvent{
			ID: fmt.Sprintf("%d", i+1), Speaker: e.Speaker, Role: role,
			Text: text, TS: e.TS, Status: status,
			Kind: e.Kind, Retracts: e.Retracts,
		}
		if debugRaw && text != e.Text {
			ev.Raw = e.Text
		}
		out = append(out, ev)
	}
	return out, nil
}

// appendRelayDMEvent writes into the shared room.
//
// The addressee is set deliberately: a human's message names the AGENT, which
// is what makes it directed mail in that agent's unified inbox and what
// `meet dispatch` wakes on. The agent's reply names nobody, which is what stops
// a reply from waking anything and cascading.
func appendRelayDMEvent(agent string, event relayDMEvent) error {
	st, err := dmRoomFor(agent, "")
	if err != nil {
		return err
	}
	ts := event.TS
	if ts.IsZero() {
		ts = nowFn()
	}
	kind, to := "message", ""
	if event.Role == "user" || event.Role == "human" {
		kind, to = "human", canonAgent(agent)
	}
	return AppendEvent(st.ID, Event{
		Round: st.Round, Speaker: event.Speaker, Role: string(RoleParticipant),
		Kind: kind, To: to, Text: event.Text, Status: event.Status, TS: ts,
	})
}

// listRelayDMs enumerates the durable dm-<a>-<b> rooms rather than a private
// directory, so the web list and `bashy meet list` agree about what exists.
func listRelayDMs() ([]relayDM, error) {
	sessions, err := listSessions()
	if err != nil {
		return nil, err
	}
	out := make([]relayDM, 0)
	for _, st := range sessions {
		if !st.Permanent || !strings.HasPrefix(st.Name, "dm-") {
			continue
		}
		agent := ""
		if len(st.Participants) > 0 {
			agent = canonAgent(st.Participants[0])
		}
		if agent == "" {
			continue
		}
		out = append(out, relayDM{
			Agent: agent, Human: st.Human, Created: st.Created, Updated: st.Created,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out, nil
}

func handleRelayDMList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct{ Agent string }
		if err := decodeBody(r, &body); err != nil {
			apiErr(w, err)
			return
		}
		lock := relayDMLock(canonAgent(body.Agent))
		lock.Lock()
		st, err := ensureRelayDM(body.Agent, actorOf(r))
		lock.Unlock()
		if err != nil {
			apiErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	items, err := listRelayDMs()
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func handleRelayDMGet(w http.ResponseWriter, r *http.Request) {
	agent := canonAgent(r.PathValue("agent"))
	lock := relayDMLock(agent)
	lock.Lock()
	defer lock.Unlock()
	st, err := ensureRelayDM(agent, actorOf(r))
	if err != nil {
		apiErr(w, err)
		return
	}
	events, err := relayDMEvents(agent, truthy(r.URL.Query().Get("raw")))
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, relayDMDetail{State: st, Events: events})
}

func handleRelayDMMessage(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := canonAgent(r.PathValue("agent"))
		var body struct{ Text string }
		if err := decodeBody(r, &body); err != nil {
			apiErr(w, err)
			return
		}
		body.Text = strings.TrimSpace(body.Text)
		if body.Text == "" {
			apiErr(w, fmt.Errorf("meet: an empty message is not a contribution"))
			return
		}
		lock := relayDMLock(agent)
		lock.Lock()
		st, err := ensureRelayDM(agent, actorOf(r))
		if err != nil {
			lock.Unlock()
			apiErr(w, err)
			return
		}
		// The timestamp is chosen HERE rather than left to the append, because
		// it is the handle a recall names: events carry no id, and a caller that
		// cannot name the record it just wrote can only ask to withdraw "the
		// last one", which is a different message the moment two people type.
		stamp := nowFn()
		if err := appendRelayDMEvent(agent, relayDMEvent{
			Speaker: st.Human, Role: "human", Text: body.Text, TS: stamp,
		}); err != nil {
			lock.Unlock()
			apiErr(w, err)
			return
		}
		lock.Unlock()
		at := stamp.Format(time.RFC3339Nano)
		// A Start work session owns this canonical identity and has already bound
		// its inbox. The event above is therefore the message: its inbox relay
		// delivers it to the persistent session. Starting a second read-only
		// Invoke here would both duplicate the request and violate one identity.
		if relayDMWorkSessionLive(agent) {
			writeJSON(w, http.StatusAccepted, map[string]string{"agent": agent, "status": "queued", "ts": at})
			return
		}
		// Cancellable, and remembered by agent name, so the sender can stop the
		// turn their message started. A 1:1 has exactly one live turn by
		// construction — one identity, one conversation store — so the name is a
		// sufficient key and no job id is needed.
		turnCtx, cancel := context.WithCancel(ctx)
		turn := storeDMTurn(agent, cancel)
		go func() {
			defer clearDMTurn(agent, turn)
			defer cancel()
			runRelayDMTurn(turnCtx, st, body.Text)
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"agent": agent, "status": "working", "ts": at})
	}
}

func relayDMWorkSessionLive(agent string) bool {
	card, ok, err := room.Find(room.AgentClaimID(canonAgent(agent)))
	if err != nil || !ok || card.Mode != "meet-work" {
		return false
	}
	for _, capability := range card.Caps {
		if capability == room.CapInboxDelivery {
			return true
		}
	}
	return false
}

// handleRelayDMWork starts the existing managed Chat session for work that may
// change the workspace. The browser never supplies a sandbox or approval flag:
// this server-side provenance check is the only web path to AllowUnsafe. Once
// trusted, chat.Start still uses the ordinary registered-agent resolver,
// environment scrubber, singleton claim, control socket, and inbox relay.
//
// An uncontained web session cannot honestly be called attended. Meet has no
// transport for rendering and answering a vendor CLI's approval prompt, so it
// refuses instead of launching a process that will either stall or gain raw
// host authority.
func handleRelayDMWork(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := canonAgent(r.PathValue("agent"))
		var body struct{ Text string }
		if err := decodeBody(r, &body); err != nil {
			apiErr(w, err)
			return
		}
		body.Text = strings.TrimSpace(body.Text)
		if body.Text == "" {
			apiErr(w, fmt.Errorf("meet: Start work needs an instruction"))
			return
		}

		// A cloud request must carry the verified identity installed by the auth
		// gate. Falling back to the server's OS user here would turn an unnamed
		// remote caller into the machine owner before a write-capable launch.
		if coopauth.ArrivedViaCloud(r) {
			id, ok := coopauth.IdentityOf(r)
			if !ok || strings.TrimSpace(id.User) == "" {
				writeErr(w, http.StatusForbidden, errors.New(
					"meet: Start work requires an authenticated account"))
				return
			}
		}

		contained, reason := trustedBashyWorkContainment()
		if !contained {
			if strings.TrimSpace(reason) == "" {
				reason = "trusted Bashy containment was not proven"
			}
			writeErr(w, http.StatusForbidden, fmt.Errorf(
				"meet: Start work is unavailable: %s; use Chat for a read-only question", reason))
			return
		}

		lock := relayDMLock(agent)
		lock.Lock()
		st, err := ensureRelayDM(agent, actorOf(r))
		lock.Unlock()
		if err != nil {
			apiErr(w, err)
			return
		}

		_, err = startRelayDMWork(ctx, agent, chat.SessionOptions{
			Prompt:      body.Text,
			ReadOnly:    false,
			Attended:    false,
			AllowUnsafe: true,
			Mode:        "meet-work",
		})
		if err != nil {
			apiErr(w, fmt.Errorf("meet: could not start work with %s: %w", agent, err))
			return
		}

		// Record what the human asked without addressing it as new mail: Start
		// already delivered this opening prompt. Addressing the transcript event
		// too would make the session's inbox relay deliver the same instruction a
		// second time.
		lock.Lock()
		err = appendRelayDMWorkEvent(st, body.Text)
		lock.Unlock()
		if err != nil {
			// Work is already live. A 4xx/5xx would invite a retry and duplicate it.
			writeJSON(w, http.StatusAccepted, map[string]string{
				"agent": agent, "status": "started", "warning": "work started but its request could not be added to the transcript",
			})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"agent": agent, "status": "started"})
	}
}

func appendRelayDMWorkEvent(st relayDM, text string) error {
	room, err := dmRoomFor(st.Agent, st.Human)
	if err != nil {
		return err
	}
	return AppendEvent(room.ID, Event{
		Round: room.Round, Speaker: st.Human, Role: string(RoleParticipant),
		Kind: "human", Text: text, Status: "work-started", TS: nowFn(),
	})
}

func runRelayDMTurn(ctx context.Context, st relayDM, prompt string) {
	lock := relayDMLock(st.Agent)
	lock.Lock()
	defer lock.Unlock()
	turnCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	roomState, err := dmRoomFor(st.Agent, st.Human)
	if err != nil {
		return
	}
	// A DM uses the same ephemeral live channel as a meeting turn. The DM
	// websocket projects this verbose local view into a bounded progress stream;
	// the durable transcript still receives only the complete final answer.
	live := newLiveWriter(roomState, st.Agent, "assistant", "")
	live.activity = dmActivitySummary
	defer live.close(statusError)
	// A chat seat ACTS: this is the surface a steward drives the machine from,
	// so a turn that could only speak would make it a suggestion box. Same
	// authority as `Start work` above, which had it already. See turnAuthority.
	dmReadOnly, dmAllowUnsafe := turnAuthority()
	res, err := invokeRelayDM(turnCtx, chat.Options{
		Agent: st.Agent, Instruction: prompt, Cwd: "",
		ReadOnly: dmReadOnly, AllowUnsafe: dmAllowUnsafe,
		Stream: live, EventStream: live.eventStream(),
	}, nil)
	// Extract the assistant's words from whatever structured transport the tool
	// streamed, exactly as the room engine does at classifyTurn. A DM is the
	// surface a human is MOST likely to read directly, so storing the raw
	// envelope here was the most visible form of the same defect.
	event := relayDMEvent{Speaker: st.Agent, Role: "assistant", Text: strings.TrimSpace(normalizeTurnText(res.Output)), Status: "ok"}
	if err != nil {
		event.Status = "error"
		if event.Text == "" {
			event.Text = err.Error()
		}
	}
	if event.Text == "" {
		event.Text = "(agent returned no text)"
	}
	_ = appendRelayDMEvent(st.Agent, event)
	live.close(event.Status)
	_, _ = ensureRelayDM(st.Agent, st.Human)
}

const (
	dmProgressHeartbeat = 5 * time.Second
	dmProgressSampleMax = 360
)

// dmProgressSampler turns the local line-granular live channel into a small
// remote-safe view. It reports Fibonacci-numbered lines (1, 2, 3, 5, 8, ...)
// and a counter-only heartbeat every five seconds. A thousand-line tool log is
// therefore a few dozen short frames, while the complete final answer remains
// available through the transcript.
type dmProgressSampler struct {
	speaker    string
	started    time.Time
	lastReport time.Time
	lines      int
	bytes      int64
	fibA       int
	fibB       int
}

func (s *dmProgressSampler) reset(e LiveEvent) LiveEvent {
	s.speaker = e.Speaker
	s.started = e.TS
	if s.started.IsZero() {
		s.started = nowFn()
	}
	s.lastReport = s.started
	s.lines, s.bytes = 0, 0
	s.fibA, s.fibB = 1, 2
	return s.frame(liveSpeaking, "", "working", s.started)
}

func (s *dmProgressSampler) add(e LiveEvent) []LiveEvent {
	var out []LiveEvent
	for line := range strings.SplitSeq(e.Text, "\n") {
		s.lines++
		s.bytes += int64(len(line))
		if s.lines != s.fibA {
			continue
		}
		next := s.fibA + s.fibB
		s.fibA, s.fibB = s.fibB, next
		out = append(out, s.frame(liveLine, progressSnippet(line), "working", e.TS))
		s.lastReport = e.TS
	}
	return out
}

func (s *dmProgressSampler) heartbeat(at time.Time) (LiveEvent, bool) {
	if s.started.IsZero() || at.Sub(s.lastReport) < dmProgressHeartbeat {
		return LiveEvent{}, false
	}
	s.lastReport = at
	return s.frame(liveLine, "", "working", at), true
}

func (s *dmProgressSampler) finish(e LiveEvent) LiveEvent {
	return s.frame(liveSpoke, "", e.Status, e.TS)
}

func (s *dmProgressSampler) frame(kind, text, status string, at time.Time) LiveEvent {
	if at.IsZero() {
		at = nowFn()
	}
	elapsed := at.Sub(s.started)
	if elapsed < 0 {
		elapsed = 0
	}
	return LiveEvent{
		Kind: kind, Speaker: s.speaker, Role: "assistant", Text: text,
		Status: status, TS: at, Lines: s.lines, Bytes: s.bytes,
		ElapsedMS: elapsed.Milliseconds(),
	}
}

func progressSnippet(line string) string {
	line = strings.TrimSpace(line)
	if len(line) <= dmProgressSampleMax {
		return line
	}
	line = line[:dmProgressSampleMax]
	for !utf8.ValidString(line) {
		line = line[:len(line)-1]
	}
	return line + "…"
}

// dmActivitySummary extracts only the public shape of tool activity from the
// configured agent transports. It deliberately excludes thinking, tool input,
// command output, usage, ids, and raw JSON: the progress card should say what
// is moving without becoming a second debug console or leaking an expensive
// transport over a remote connection.
func dmActivitySummary(line string) string {
	var e struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"item"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
		Part struct {
			Type string `json:"type"`
			Tool string `json:"tool"`
		} `json:"part"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) != nil {
		return ""
	}
	if (e.Type == "item.started" || e.Type == "item.completed") && e.Item.Type == "command_execution" {
		verb := "Running"
		if e.Type == "item.completed" {
			verb = "Finished"
		}
		command := progressSnippet(e.Item.Command)
		if command == "" {
			command = "command"
		}
		return verb + ": " + command
	}
	if e.Type == "assistant" {
		for _, block := range e.Message.Content {
			if block.Type == "tool_use" && strings.TrimSpace(block.Name) != "" {
				return "Using tool: " + progressSnippet(block.Name)
			}
		}
	}
	if e.Type == "tool.call" && strings.TrimSpace(e.Data.Name) != "" {
		return "Using tool: " + progressSnippet(e.Data.Name)
	}
	if e.Part.Type == "tool" && strings.TrimSpace(e.Part.Tool) != "" {
		return "Using tool: " + progressSnippet(e.Part.Tool)
	}
	return ""
}

func handleRelayDMObserve(w http.ResponseWriter, r *http.Request) {
	agent := canonAgent(r.URL.Query().Get("agent"))
	lock := relayDMLock(agent)
	lock.Lock()
	_, ensureErr := ensureRelayDM(agent, actorOf(r))
	lock.Unlock()
	if ensureErr != nil {
		http.Error(w, ensureErr.Error(), http.StatusBadRequest)
		return
	}
	dir, _ := relayDMDir(agent)
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	debugRaw := truthy(r.URL.Query().Get("raw"))
	rec := &lineTail{path: filepath.Join(dir, "transcript.jsonl")}
	live := &lineTail{path: filepath.Join(dir, "live.jsonl")}
	write := func(kind string, data any) error { return conn.WriteJSON(wsFrame{Kind: kind, Data: data}) }
	if events, err := relayDMEvents(agent, debugRaw); err == nil {
		for _, event := range events {
			if write("dm-event", event) != nil {
				return
			}
		}
	}
	rec.skipToEnd()
	// The live view is forward-only. Replaying it would resurrect the progress
	// card for a turn whose completed answer is already in the transcript.
	live.skipToEnd()
	var progress dmProgressSampler
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(observePoll):
		}
		liveLines, err := live.next()
		if err == nil {
			for _, line := range liveLines {
				var event LiveEvent
				if json.Unmarshal(line, &event) != nil || event.Speaker != agent {
					continue
				}
				switch event.Kind {
				case liveSpeaking:
					if write("dm-progress", progress.reset(event)) != nil {
						return
					}
				case liveLine:
					for _, sample := range progress.add(event) {
						if write("dm-progress", sample) != nil {
							return
						}
					}
				case liveSpoke:
					if write("dm-progress", progress.finish(event)) != nil {
						return
					}
				}
			}
		}
		if heartbeat, ok := progress.heartbeat(nowFn()); ok {
			if write("dm-progress", heartbeat) != nil {
				return
			}
		}
		lines, err := rec.next()
		if err != nil {
			continue
		}
		for _, line := range lines {
			var event relayDMEvent
			if json.Unmarshal(line, &event) == nil {
				// Same render seam as the backlog above — a tailed line is read
				// straight off disk and has had no capture-time normalization
				// applied by THIS process.
				if text := normalizeTurnText(event.Text); text != event.Text {
					if debugRaw {
						event.Raw = event.Text
					}
					event.Text = text
				}
				if write("dm-event", event) != nil {
					return
				}
			}
		}
	}
}

// --- Recall, for the 1:1 ------------------------------------------------------

// dmTurns is the live turn per agent. A 1:1 holds one identity and one
// conversation store, so there is at most one.
var dmTurns sync.Map // agent -> *dmTurn

// dmTurn is a handle rather than a bare cancel func so that "is this still the
// turn I started?" is pointer identity. Comparing functions is not; without a
// handle, a finished turn can delete — or worse, cancel — the NEXT one, which
// is the classic stale-handle shape: the sender sends again, the previous
// goroutine returns, and the new turn dies with no explanation anywhere.
type dmTurn struct{ cancel context.CancelFunc }

func storeDMTurn(agent string, cancel context.CancelFunc) *dmTurn {
	t := &dmTurn{cancel: cancel}
	dmTurns.Store(canonAgent(agent), t)
	return t
}

func clearDMTurn(agent string, t *dmTurn) {
	name := canonAgent(agent)
	if v, ok := dmTurns.Load(name); ok && v == any(t) {
		dmTurns.Delete(name)
	}
}

// handleRelayDMRecall is "stop that message" for a chat.
//
// A chat appends the human's line INSIDE the send request — that is what makes
// the reply streamable — so by the time a recall can arrive the record exists,
// and the honest answer is a retraction rather than a cancellation. The compose
// surface is what makes an actual cancel possible here: it holds the message
// before dispatching it, so the common "wait, no" never reaches this handler at
// all.
func handleRelayDMRecall(w http.ResponseWriter, r *http.Request) {
	agent := canonAgent(r.PathValue("agent"))
	var body struct {
		TS string `json:"ts"`
	}
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	st, err := dmRoomFor(agent, "")
	if err != nil {
		apiErr(w, err)
		return
	}
	// Stop the turn first: the agent is reading the message being withdrawn, and
	// every token it produces after the sender asked to stop is work nobody
	// wants and context the conversation has to carry.
	if v, ok := dmTurns.LoadAndDelete(agent); ok {
		if turn, _ := v.(*dmTurn); turn != nil && turn.cancel != nil {
			turn.cancel()
		}
	}
	res, err := Recall(st, "", body.TS, actorOf(r))
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
