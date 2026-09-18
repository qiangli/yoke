package weave

// REACHING THE CONDUCTOR — a room to talk in, and a bus to make someone notice.
//
// A sprint says who is accountable. It did not say how to reach them, and the
// gap matters as soon as more than one thing is running: the answer to "who
// owns this repo" is useless if the next question is "…and how do I ask them".
//
// Two mechanisms, because they answer different questions and neither
// substitutes for the other:
//
//	meet room   a PLACE to have the conversation, with a durable transcript
//	bus         the INTERRUPT that makes someone look at it
//
// The room is opened by `sprint start`, not by a conductor remembering to. An
// optional room is empty exactly when it is needed — at the moment somebody
// urgent arrives and the conductor is mid-turn on something else.
//
// # Why a file was rejected as the primary
//
// "A file the conductor checks periodically" assumes a liveness the lease
// already tells us is often false: conductors die by SIGKILL and token
// exhaustion, and a file nobody polls is indistinguishable from a file with
// nothing in it. The bus solves the half that is actually hard — its sidecar
// holds a subscription off the agent's critical path and leaves a pre-resolved
// buffer to read at a turn boundary, because an agent mid-turn cannot decide to
// go and check somewhere. Its demote-never-drop rule then guarantees an
// undeliverable interrupt becomes a queued notification with a recorded reason
// rather than silence.
//
// A file is fine BEHIND that as a durable drop-box. It is not a contact method.
//
// # The address is stored, not the mechanism
//
// Contact carries a Kind and a Ref rather than a room number, so the mechanism
// can change without a schema change. And the Ref is meet's room ID, never its
// short room NUMBER: that number is a pointer, released and REUSED when a room
// closes, so a sprint holding one would eventually name somebody else's meeting
// while looking perfectly valid.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/role"
	"github.com/qiangli/yoke/pkg/role/meetroom"
)

// sprintContact is how to reach the conductor responsible for a sprint.
//
// It is pkg/role's Contact: the steward holds a host the same way a conductor
// holds a sprint, and one shape for both is what lets the sweep treat them
// identically. The alias keeps the existing JSON and call sites unchanged.
type sprintContact = role.Contact

// sprintTopic is the bus topic for one sprint's conductor.
func sprintTopic(id int64) string { return sprintAssignment(id, "").Topic() }

// sprintAssignment is a sprint expressed as a role assignment.
func sprintAssignment(id int64, title string) role.Assignment {
	return role.Assignment{Kind: role.Conductor, Ref: strconv.FormatInt(id, 10), Title: title}
}

// openSprintRoom convenes the room a running sprint is reachable in.
//
// Failure is NOT fatal to starting the sprint. A conductor who cannot open a
// room still has a box to run, and refusing to start work because the intercom
// is down would be the wrong trade — the sprint simply records no contact, and
// the surfaces that show contact say so rather than implying one exists.
func openSprintRoom(s *weaveStory, conductor string) (*sprintContact, error) {
	return meetroom.Assume(sprintAssignment(s.ID, s.Title), conductor)
}

// closeSprintRoom releases a sprint's room when the SPRINT ends.
//
// It closes as the CONVENER, not as whoever happens to be running the command.
// Only the organizer may close a meet room, and the room now outlives the
// conductor who opened it (see weave_story_reach.go) — so by the time a sprint
// stops, the actor is routinely somebody who was not there when it opened.
// Passing the current actor would make the close silently fail and leak the
// room, which is the failure this whole area exists to prevent.
func closeSprintRoom(s *weaveStory, actor string) error {
	if s.Contact == nil {
		return nil
	}
	closer := strings.TrimSpace(s.Contact.Holder)
	if closer == "" {
		closer = actor
	}
	err := meetroom.Release(s.Contact, closer)
	s.Contact = nil
	return err
}

// ensureSprintRoom opens the room only when the sprint has none.
//
// Reusing a live room across a take is deliberate: the transcript IS the
// handover context, and closing plus reopening on every conductor change threw
// it away and filed a set of meet minutes for each hop of one continuous
// conversation.
func ensureSprintRoom(s *weaveStory, conductor string) string {
	if s.Contact != nil {
		// The room exists. Make sure it still declares the SEAT as its default
		// addressee: rooms opened before DefaultTo existed carry none, so
		// unaddressed mail in them resolves to nobody — the very failure the
		// field was added to close. Best-effort; a room without one is no worse
		// off than it was.
		_ = meetroom.EnsureDefaultTo(s.Contact, sprintAssignment(s.ID, s.Title))
		// Same migration rule for the human-facing room name. Rename metadata in
		// place; replacing the room would discard the session history this room
		// exists to preserve.
		_ = meetroom.EnsureName(s.Contact, sprintAssignment(s.ID, s.Title))
		return ""
	}
	c, err := openSprintRoom(s, conductor)
	if err != nil {
		// Reported, never swallowed: a contact that silently failed to open
		// reads identically to one nobody has tried to use yet.
		return fmt.Sprintf("; no room (%v)", err)
	}
	s.Contact = c
	return "; " + c.String()
}

// pingSprintConductor publishes an interrupt to whoever is conducting.
//
// It addresses the sprint's TOPIC rather than the conductor's name on purpose:
// the holder changes across a handoff or a take, and a message addressed to a
// person who has since handed off would be delivered to somebody with no
// context — or to nobody. The topic follows the responsibility.
func pingSprintConductor(s *weaveStory, from, body, priority string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("nothing to send")
	}
	holder, _, free := weaveStoryLeaseState(s)
	if free {
		return fmt.Errorf("sprint #%d has no conductor to ping — `sprint take %d` to become one", s.ID, s.ID)
	}
	n := bus.Notification{
		Principal: from,
		Topic:     sprintTopic(s.ID),
		To:        holder,
		Body:      body,
		Priority:  priority,
	}
	if s.Contact != nil {
		n.Room = s.Contact.Ref
	}
	return bus.Publish(n)
}

// currentActor is who is running the command, for authorship on a ping.
func currentActor() string {
	if u := strings.TrimSpace(os.Getenv("WEAVE_CONDUCTOR")); u != "" {
		return u
	}
	return weaveConductorName("")
}

// newSprintPingCmd interrupts the conductor responsible for a sprint.
func newSprintPingCmd() *cobra.Command {
	var flags weaveOutputFlags
	var body, priority string
	cmd := &cobra.Command{
		Use:   "ping <sprint>",
		Short: "Interrupt the conductor responsible for a sprint",
		Long: "ping reaches the conductor who owns a sprint's delivery.\n\n" +
			"It goes to the sprint's TOPIC, not to a person's name. The holder changes\n" +
			"across a handoff or a take, and a message addressed to whoever held it an\n" +
			"hour ago would land on somebody with no context — or on nobody. The topic\n" +
			"follows the responsibility.\n\n" +
			"Delivery is the bus's problem, and it is the half that is actually hard: an\n" +
			"agent mid-turn cannot decide to go and look somewhere, so the sidecar holds\n" +
			"the subscription off its critical path and leaves a buffer to read at a turn\n" +
			"boundary. An interrupt that cannot be delivered is DEMOTED to a queued\n" +
			"notification with a reason, never dropped.\n\n" +
			"The conversation itself belongs in the sprint's meet room, which `sprint\n" +
			"status` prints beside the conductor's name.",
		Example: "  bashy sprint ping 3 --body \"stopping you for the incident — park at a clean gate\"\n" +
			"  bashy sprint ping 3 --body \"need coreutils free\" --priority high",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			mode := flags.mode()
			dir, derr := weaveStoryDir(cmd, mode, "sprint ping")
			if derr != nil {
				return derr
			}
			q, lerr := loadWeaveQueue(dir)
			if lerr != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "sprint ping", weavecli.ExitGenericFail, lerr))
			}
			s := findWeaveStory(q, id)
			if s == nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "sprint ping", weavecli.ExitInvalidArg,
					fmt.Errorf("sprint #%d not found", id)))
			}
			if err := pingSprintConductor(s, currentActor(), body, priority); err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "sprint ping", weavecli.ExitGenericFail, err))
			}
			holder, stale, _ := weaveStoryLeaseState(s)
			where := ""
			if c := s.Contact.String(); c != "" {
				where = " — reply in " + c
			}
			line := fmt.Sprintf("pinged %s on %s%s", holder, sprintTopic(id), where)
			if stale {
				// Sent, but say so: a stale lease means the conductor stopped
				// heartbeating, so the ping may be addressed to nobody. The bus
				// will hold it, and the sender deserves to know it may wait.
				line += "\n  note: that conductor's lease is STALE — the ping is queued, but may go unread until someone takes the sprint"
			}
			if mode != weavecli.OutputJSON {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "sprint ping", map[string]any{
				"topic": sprintTopic(id), "to": holder, "lease_stale": stale, "room": s.Contact.RefID(),
			}))
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "what to say")
	cmd.Flags().StringVar(&priority, "priority", "", "urgency hint for the sidecar's governance rules")
	flags.attach(cmd)
	return cmd
}

// sweepDeadRooms closes the rooms of sprints whose conductor is no longer live.
//
// Liveness is decided HERE, by the lease, and handed to role.Sweep as a fact.
// The role package deliberately holds no opinion about it: the lease is weave's
// authority, and a second opinion living one package away could disagree with
// it invisibly.
//
// A sprint that is not running is not swept even if its lease went stale —
// there is nothing in flight for anyone to reach in about, and closing a room
// on a parked sprint would remove the channel a conductor uses when they pick
// it back up.
func sweepDeadRooms(stories []*weaveStory, now time.Time, actor string) []string {
	var open []role.Occupied
	for _, s := range stories {
		if s == nil || s.Contact == nil || s.currentBox() == nil {
			continue
		}
		_, stale, free := weaveStoryLeaseState(s)
		open = append(open, role.Occupied{
			Label:   fmt.Sprintf("#%d", s.ID),
			Contact: s.Contact,
			Live:    !stale && !free,
		})
	}
	if len(open) == 0 {
		return nil
	}
	res := meetroom.Sweep(open, actor)
	// Drop the contact from any sprint whose room was closed, so the board
	// stops advertising it. A swept room that still shows in `status` is the
	// same lie the sweep exists to remove.
	closed := map[string]bool{}
	for _, l := range res.Closed {
		closed[l] = true
	}
	for _, s := range stories {
		if s != nil && closed[fmt.Sprintf("#%d", s.ID)] {
			s.Contact = nil
		}
	}
	return res.Closed
}
