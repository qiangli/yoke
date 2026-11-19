package meet

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/bus"
)

// DigestItem preserves the source's authority instead of upgrading a summary
// into a decision. Source is a durable room, story, commit, or artifact ref.
type DigestItem struct {
	Authority string `json:"authority"` // human-decision|verified-result|proposal|unverified
	Source    string `json:"source"`
	Text      string `json:"text"`
}

type ResponsibilityRequest struct {
	Room        string       `json:"room,omitempty"`
	OperationID string       `json:"operation_id"`
	From        string       `json:"from"`
	Accountable string       `json:"accountable"`
	Question    string       `json:"question"`
	Observers   []string     `json:"observers,omitempty"`
	Digest      []DigestItem `json:"digest,omitempty"`
	EscalateTo  string       `json:"escalate_to"`
}

type ResponsibilityDelivery struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	State   string `json:"state"`
	Ref     string `json:"ref,omitempty"`
	Warning string `json:"warning,omitempty"`
}

type ResponsibilityReceipt struct {
	OperationID string                   `json:"operation_id"`
	Question    ResponsibilityDelivery   `json:"question"`
	Observers   []ResponsibilityDelivery `json:"observers,omitempty"`
	Escalation  *ResponsibilityDelivery  `json:"escalation,omitempty"`
	Fallback    string                   `json:"fallback,omitempty"`
}

// RouteResponsibility sends one question to the accountable address and a
// source-labelled digest to each distinct observer. It never broadcasts or
// launches an agent. A role remains a late-bound role address in either store.
func RouteResponsibility(req ResponsibilityRequest) (ResponsibilityReceipt, error) {
	var out ResponsibilityReceipt
	if !operationIDPattern.MatchString(req.OperationID) {
		return out, fmt.Errorf("meet route: --id must be 8-80 letters, digits, dots, underscores or hyphens")
	}
	out.OperationID = req.OperationID
	req.From = strings.TrimSpace(req.From)
	req.Accountable = strings.TrimSpace(req.Accountable)
	req.EscalateTo = strings.TrimSpace(req.EscalateTo)
	if req.Accountable == "" || req.EscalateTo == "" || strings.TrimSpace(req.Question) == "" {
		return out, fmt.Errorf("meet route: --accountable, --question and --escalate-to are required")
	}
	actor, err := bus.ResolveAuthoredActor(req.From)
	if err != nil {
		return out, err
	}
	if bus.DetectHarness != nil {
		if _, detected := bus.DetectHarness(); detected && !registeredRouteAgent(actor) {
			return out, fmt.Errorf("meet route: an agent session cannot route as a human")
		}
	}
	if !registeredRouteAgent(actor) {
		self := strings.TrimSpace(os.Getenv("USER"))
		if self == "" {
			self = strings.TrimSpace(os.Getenv("LOGNAME"))
		}
		if !strings.EqualFold(actor, self) {
			return out, fmt.Errorf("meet route: a human may route only under the current principal")
		}
	}
	req.From = actor
	if isAllSeats(req.Accountable) || isAllSeats(req.EscalateTo) {
		return out, fmt.Errorf("meet route: accountable and escalation must name one seat each")
	}
	if strings.EqualFold(req.Accountable, req.EscalateTo) {
		return out, fmt.Errorf("meet route: escalation must name a different seat")
	}
	var st *State
	if req.Room != "" {
		st, err = roomOf(req.Room)
		if err != nil {
			return out, err
		}
		if st.board() && !participantSeat(st, actor) {
			return out, fmt.Errorf("meet route: %s has no seat in board %s", actor, st.ID)
		}
		if !participantSeat(st, actor) && requireOrganizer(st, actor) != nil {
			return out, fmt.Errorf("meet route: %s has no authority in room %s", actor, st.ID)
		}
	} else if len(req.Digest) != 0 {
		return out, fmt.Errorf("meet route: sourced observer digests require a room")
	}
	if len(req.Observers) > 0 && st == nil {
		return out, fmt.Errorf("meet route: observer digests require a room")
	}
	digest, err := renderResponsibilityDigest(st, req.Digest, "")
	if err != nil {
		return out, err
	}
	if err := bus.ValidateCoordinationBody("Question for accountable seat: " + strings.TrimSpace(req.Question)); err != nil {
		return out, fmt.Errorf("meet route: %w", err)
	}
	if digest != "" {
		if err := bus.ValidateCoordinationBody(digest); err != nil {
			return out, fmt.Errorf("meet route: %w", err)
		}
	}
	// Validate all addresses before writing the first message. A bad observer
	// cannot leave an apparently complete routing operation half posted.
	seen := map[string]bool{strings.ToLower(strings.TrimPrefix(req.Accountable, "@")): true}
	observers := make([]string, 0, len(req.Observers))
	for _, raw := range req.Observers {
		to := strings.TrimSpace(raw)
		if to == "" || isAllSeats(to) {
			return out, fmt.Errorf("meet route: observers must be named seats, never all")
		}
		key := strings.ToLower(strings.TrimPrefix(to, "@"))
		if seen[key] {
			continue
		}
		seen[key] = true
		observers = append(observers, to)
	}
	if len(observers) > 0 && digest == "" {
		return out, fmt.Errorf("meet route: observer delivery needs a sourced digest")
	}
	// One routing lock covers read-check-post for all recipients of this operation.
	// The marker lives in the existing transcript or board, not a second store.
	lockDir := bus.BoardDir()
	if st != nil {
		lockDir, err = storeDir(st.ID)
		if err != nil {
			return out, err
		}
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return out, err
	}
	lock, err := lockfile.Acquire(filepath.Join(lockDir, "responsibility.lock"), lockfile.Holder{Name: "meet-route", PID: os.Getpid(), Intent: "route responsibility", Since: nowFn()})
	if err != nil {
		return out, err
	}
	defer lock.Release()
	for _, to := range append([]string{req.Accountable, req.EscalateTo}, observers...) {
		if req.Room == "" {
			if _, err := bus.ResolveRecipient(to); err != nil {
				return out, err
			}
			continue
		}
		target, err := resolveSeatTarget(to)
		if err != nil {
			return out, err
		}
		seat := target.Seat
		if target.Role {
			seat, _ = bus.RoleHolderFor(seat)
		}
		if seat != "" && !participantSeat(st, seat) {
			return out, fmt.Errorf("meet route: %s has no seat in room %s", to, st.ID)
		}
	}
	send := func(to, phase, body string) (ResponsibilityDelivery, error) {
		body = fmt.Sprintf("[route:%s:%s] %s", req.OperationID, phase, body)
		if req.Room != "" {
			if found, ok, err := existingRoomRoute(st.ID, req.From, to, body); err != nil {
				return ResponsibilityDelivery{}, err
			} else if ok {
				return found, nil
			}
			ev, err := PostAs(req.Room, req.From, to, body)
			if err != nil {
				return ResponsibilityDelivery{To: to, Channel: "meet", State: "unverified",
					Ref:     fmt.Sprintf("room:%s#route:%s:%s", st.ID, req.OperationID, phase),
					Warning: "post status uncertain; inspect this route marker before retrying"}, err
			}
			events, err := readRoomTranscript(st.ID)
			if err != nil {
				return ResponsibilityDelivery{To: to, Channel: "meet", State: "unverified", Ref: fmt.Sprintf("room:%s#route:%s:%s@%s", st.ID, req.OperationID, phase, ev.TS.Format("20060102T150405.000000000Z0700")),
					Warning: "message posted, but delivery status could not be read: " + err.Error()}, nil
			}
			state := "queued"
			reader := ev.To
			if holder, ok := bus.RoleHolderFor(reader); ok {
				reader = holder
			}
			if reader != "" && !isRoleAddress(reader) {
				state = boardDeliveryState(st.ID, reader, int64(len(events)))
			}
			warning := ev.Warning
			// The transcript is durable before the push. An existing live seat
			// receives this exact message; no one-shot or second agent is started.
			if card, live := liveSeat(reader); live {
				push := deliverToLiveSeat(card, reader, fmt.Sprintf("Meet room %s: %s", st.ID, body))
				if push.Steered {
					state = "delivered"
				} else if warning == "" {
					warning = push.Reason
				}
			}
			return ResponsibilityDelivery{To: to, Channel: "meet", State: state,
				Ref: fmt.Sprintf("room:%s#%d", st.ID, len(events)), Warning: warning}, nil
		}
		if found, ok, err := existingBusRoute(req.From, to, body); err != nil {
			return ResponsibilityDelivery{}, err
		} else if ok {
			return found, nil
		}
		res, err := bus.Send(bus.SendRequest{From: req.From, To: to, Topic: "responsibility", Body: body})
		if err != nil {
			return ResponsibilityDelivery{}, err
		}
		d := ResponsibilityDelivery{To: to, Channel: "bus", State: "unverified", Ref: fmt.Sprintf("mb:%d", res.Seq)}
		if len(res.Deliveries) > 0 {
			d.State, d.Warning = res.Deliveries[0].State, res.Deliveries[0].Warning
		}
		return d, nil
	}
	out.Question, err = send(req.Accountable, "question", "Question for accountable seat: "+strings.TrimSpace(req.Question))
	if err != nil {
		out.Fallback = "question delivery is uncertain; inspect the room or board, then contact " + req.EscalateTo + " directly"
		return out, err
	}
	for _, to := range observers {
		visibleDigest, digestErr := renderResponsibilityDigest(st, req.Digest, to)
		if digestErr != nil {
			return out, digestErr
		}
		if visibleDigest == "" {
			continue
		}
		d, sendErr := send(to, "digest:"+strings.ToLower(strings.TrimPrefix(to, "@")), visibleDigest)
		if sendErr != nil {
			out.Fallback = "observer " + to + " delivery is uncertain; inspect the room or board before retrying"
			return out, sendErr
		}
		out.Observers = append(out.Observers, d)
	}
	if out.Question.State != "delivered" && out.Question.State != "read" {
		out.Fallback = "accountable seat has not confirmed receipt; escalation sent to " + req.EscalateTo
		notice := fmt.Sprintf("Escalation: %s has not confirmed receipt of a question. Source %s. Please follow up with that seat; this notice grants no decision authority.", req.Accountable, out.Question.Ref)
		d, sendErr := send(req.EscalateTo, "escalation", notice)
		if sendErr != nil {
			out.Fallback = "accountable seat has not confirmed receipt; escalation failed; contact " + req.EscalateTo + " directly"
			return out, sendErr
		}
		out.Escalation = &d
	}
	return out, nil
}

var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,80}$`)
var roomSourcePattern = regexp.MustCompile(`^room:([^#]+)#([1-9][0-9]*)$`)

func registeredRouteAgent(actor string) bool {
	_, ok := registeredAgentFn(actor)
	return ok
}

func existingRoomRoute(id, from, to, body string) (ResponsibilityDelivery, bool, error) {
	events, err := readRoomTranscript(id)
	if err != nil {
		return ResponsibilityDelivery{}, false, err
	}
	marker := body[:strings.Index(body, "]")+1]
	for i, ev := range events {
		if !strings.HasPrefix(ev.Text, marker) {
			continue
		}
		if ev.Speaker != from || !strings.EqualFold(ev.To, to) || ev.Text != body {
			return ResponsibilityDelivery{}, false, fmt.Errorf("meet route: operation ID already belongs to different content")
		}
		reader := ev.To
		if holder, ok := bus.RoleHolderFor(reader); ok {
			reader = holder
		}
		state := "queued"
		if reader != "" && !isRoleAddress(reader) {
			state = boardDeliveryState(id, reader, int64(i+1))
		}
		return ResponsibilityDelivery{To: to, Channel: "meet", State: state, Ref: fmt.Sprintf("room:%s#%d", id, i+1)}, true, nil
	}
	return ResponsibilityDelivery{}, false, nil
}

func existingBusRoute(from, to, body string) (ResponsibilityDelivery, bool, error) {
	posts, err := bus.Posts()
	if err != nil {
		return ResponsibilityDelivery{}, false, err
	}
	marker := body[:strings.Index(body, "]")+1]
	rec, err := bus.ResolveRecipient(to)
	if err != nil {
		return ResponsibilityDelivery{}, false, err
	}
	for _, post := range posts {
		if !strings.HasPrefix(post.Body, marker) {
			continue
		}
		if post.From != from || post.To != rec.Addr || post.Body != body {
			return ResponsibilityDelivery{}, false, fmt.Errorf("meet route: operation ID already belongs to different content")
		}
		return ResponsibilityDelivery{To: to, Channel: "bus", State: "queued", Ref: fmt.Sprintf("mb:%d", post.Seq)}, true, nil
	}
	return ResponsibilityDelivery{}, false, nil
}

func renderResponsibilityDigest(st *State, items []DigestItem, observer string) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	if st == nil {
		return "", fmt.Errorf("meet route: sourced digest requires a room")
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Observer digest (source authority retained; no action requested):")
	count := 0
	for _, item := range items {
		switch item.Authority {
		case "human-decision", "proposal", "unverified":
		case "verified-result":
			return "", fmt.Errorf("meet route: verified-result requires a verification record; room prose is insufficient")
		default:
			return "", fmt.Errorf("meet route: unknown digest authority %q", item.Authority)
		}
		match := roomSourcePattern.FindStringSubmatch(strings.TrimSpace(item.Source))
		if match == nil || match[1] != st.ID {
			return "", fmt.Errorf("meet route: digest source must be a numbered record in this room")
		}
		seq, _ := strconv.Atoi(match[2])
		if seq > len(events) {
			return "", fmt.Errorf("meet route: digest source %s does not exist", item.Source)
		}
		ev := events[seq-1]
		if strings.TrimSpace(item.Text) == "" || strings.TrimSpace(item.Text) != strings.TrimSpace(ev.Text) {
			return "", fmt.Errorf("meet route: digest text must match its source record")
		}
		if item.Authority == "human-decision" && (ev.Kind != "decision" || !strings.EqualFold(ev.Speaker, st.Human)) {
			return "", fmt.Errorf("meet route: human-decision needs a decision authored by the room human")
		}
		if observer != "" && addressedEvent(ev) && !directedEvent(ev, observer) {
			continue
		}
		fmt.Fprintf(&b, "\n[%s] %s: %s", item.Authority, strings.TrimSpace(item.Source), strings.TrimSpace(item.Text))
		count++
	}
	if count == 0 {
		return "", nil
	}
	return b.String(), nil
}
