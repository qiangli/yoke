package meet

import (
	"fmt"
	"strings"

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
	Question   ResponsibilityDelivery   `json:"question"`
	Observers  []ResponsibilityDelivery `json:"observers,omitempty"`
	Escalation *ResponsibilityDelivery  `json:"escalation,omitempty"`
	Fallback   string                   `json:"fallback,omitempty"`
}

// RouteResponsibility sends one question to the accountable address and a
// source-labelled digest to each distinct observer. It never broadcasts or
// launches an agent. A role remains a late-bound role address in either store.
func RouteResponsibility(req ResponsibilityRequest) (ResponsibilityReceipt, error) {
	var out ResponsibilityReceipt
	req.From = strings.TrimSpace(req.From)
	req.Accountable = strings.TrimSpace(req.Accountable)
	req.EscalateTo = strings.TrimSpace(req.EscalateTo)
	if req.From == "" || req.Accountable == "" || req.EscalateTo == "" || strings.TrimSpace(req.Question) == "" {
		return out, fmt.Errorf("meet route: --as, --accountable, --question and --escalate-to are required")
	}
	if isAllSeats(req.Accountable) || isAllSeats(req.EscalateTo) {
		return out, fmt.Errorf("meet route: accountable and escalation must name one seat each")
	}
	if strings.EqualFold(req.Accountable, req.EscalateTo) {
		return out, fmt.Errorf("meet route: escalation must name a different seat")
	}
	digest, err := renderResponsibilityDigest(req.Digest)
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
	var st *State
	if req.Room != "" {
		st, err = roomOf(req.Room)
		if err != nil {
			return out, err
		}
	}
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
	send := func(to, body string) (ResponsibilityDelivery, error) {
		if req.Room != "" {
			ev, err := PostAs(req.Room, req.From, to, body)
			if err != nil {
				return ResponsibilityDelivery{}, err
			}
			events, err := readRoomTranscript(st.ID)
			if err != nil {
				return ResponsibilityDelivery{To: to, Channel: "meet", State: "unverified", Ref: st.ID,
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
	out.Question, err = send(req.Accountable, "Question for accountable seat: "+strings.TrimSpace(req.Question))
	if err != nil {
		out.Fallback = "question delivery is uncertain; inspect the room or board, then contact " + req.EscalateTo + " directly"
		return out, err
	}
	for _, to := range observers {
		d, sendErr := send(to, digest)
		if sendErr != nil {
			out.Fallback = "observer " + to + " delivery is uncertain; inspect the room or board before retrying"
			return out, sendErr
		}
		out.Observers = append(out.Observers, d)
	}
	if out.Question.State != "delivered" && out.Question.State != "read" {
		out.Fallback = "accountable seat has not confirmed receipt; escalation sent to " + req.EscalateTo
		notice := fmt.Sprintf("Escalation: %s has not confirmed receipt of a question. Source %s. Please follow up with that seat; this notice grants no decision authority.", req.Accountable, out.Question.Ref)
		d, sendErr := send(req.EscalateTo, notice)
		if sendErr != nil {
			out.Fallback = "accountable seat has not confirmed receipt; escalation failed; contact " + req.EscalateTo + " directly"
			return out, sendErr
		}
		out.Escalation = &d
	}
	return out, nil
}

func renderResponsibilityDigest(items []DigestItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Observer digest (source authority retained; no action requested):")
	for _, item := range items {
		switch item.Authority {
		case "human-decision", "verified-result", "proposal", "unverified":
		default:
			return "", fmt.Errorf("meet route: unknown digest authority %q", item.Authority)
		}
		if strings.TrimSpace(item.Source) == "" || strings.TrimSpace(item.Text) == "" {
			return "", fmt.Errorf("meet route: each digest item needs a source and text")
		}
		fmt.Fprintf(&b, "\n[%s] %s: %s", item.Authority, strings.TrimSpace(item.Source), strings.TrimSpace(item.Text))
	}
	return b.String(), nil
}
