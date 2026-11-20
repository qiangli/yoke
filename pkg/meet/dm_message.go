package meet

import (
	"fmt"
	"strings"
)

// DMReceipt names where a direct message was recorded.
type DMReceipt struct {
	Agent string // the canonical seat the mail is addressed to
	Human string // the sender it is recorded under
	Room  string // id of the durable dm-<human>-<agent> room
}

// DirectMessage records one message from a human in the durable
// dm-<human>-<agent> room: the same event the Meet app's 1:1 and the
// `meet dm` front door write, so it is directed mail in the agent's unified
// inbox (`bashy inbox`, source "meet") and nothing new is stored anywhere.
//
// It is for a caller that is not the Meet app — the browser hub's Shape
// button. It records and returns: it starts no agent turn and steers nothing,
// because a one-shot spawned by a button press would answer as an identity
// that has none of the addressed seat's context (see live_seat.go). A DM room
// has no body cap, so a long or multi-line text is kept verbatim.
func DirectMessage(agent, human, text string) (DMReceipt, error) {
	agent = canonAgent(strings.TrimPrefix(strings.TrimSpace(agent), "@"))
	if agent == "" {
		return DMReceipt{}, fmt.Errorf("meet: a direct message needs an agent")
	}
	if strings.TrimSpace(text) == "" {
		return DMReceipt{}, fmt.Errorf("meet: an empty message is not a contribution")
	}
	if err := routableSeat(agent); err != nil {
		return DMReceipt{}, err
	}
	if human = strings.TrimSpace(human); human == "" {
		human = humanName()
	}
	if err := appendDMFrontDoorEvent(agent, human, relayDMEvent{
		Speaker: human, Role: "human", Text: text, TS: nowFn(),
	}); err != nil {
		return DMReceipt{}, err
	}
	st, err := dmRoomFor(agent, human)
	if err != nil {
		return DMReceipt{}, err
	}
	return DMReceipt{Agent: agent, Human: human, Room: st.ID}, nil
}
