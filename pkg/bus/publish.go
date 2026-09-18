package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// PublishEnvelope is the structured response in --json mode.
type PublishEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	Principal     string `json:"principal,omitempty"`
	Topic         string `json:"topic,omitempty"`
	Room          string `json:"room,omitempty"`
	To            string `json:"to,omitempty"`
	Message       string `json:"message,omitempty"`
	Error         string `json:"error,omitempty"`
}

func newPublishCmd() *cobra.Command {
	var topic, to, roomID, as, priority string
	var jsonOut bool

	cmd := &cobra.Command{
		Use:     "publish [flags] <message>",
		Aliases: []string{"pub", "send"},
		Short:   "publish a change notification to the bus",
		Long: `publish appends a notification to the host room's timeline, where any
'bus watch' subscriber will see it.

Addressing — at least one of these is REQUIRED, because a notification nobody is
addressed by cannot be delivered to anyone:
  --topic <t>    broadcast to a named topic
  --to <id>      1:1 delivery to a session or role
  --room <id>    room-scoped publish

Every publish must carry an identity (who sent it). Supply --as, set
$BASHY_PRINCIPAL, or $USER is used as the default. A publish with no identity is
rejected — this is the REPORT/AUTHOR invariant: a notification nobody can be
attributed to is not a notification.

One authored quick-coordination body is limited to 1024 UTF-8 bytes and is
never truncated or auto-split. Prefer a stable repo-relative commit/issue/room/
artifact reference. If none exists, manually send numbered <=1024-byte parts
with one correlation token and END; the receiver waits for END and reports
missing parts.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := strings.Join(args, " ")
			if err := ValidateCoordinationBody(msg); err != nil {
				return publishErr(cmd, jsonOut, err.Error(), topic, roomID, to, msg)
			}

			// Preserve publish's pre-existing no-login refusal. BoardIdentity also
			// supports anonymous/public readers, but an authored Bus event has
			// always required an asserted principal.
			if strings.TrimSpace(resolvePrincipal(as)) == "" {
				return publishErr(cmd, jsonOut,
					"identity is required (REPORT/AUTHOR invariant): set --as or $BASHY_PRINCIPAL",
					topic, roomID, to, msg)
			}
			who, actorErr := ResolveAuthoredActor(as)
			if actorErr != nil {
				return publishErr(cmd, jsonOut, actorErr.Error(), topic, roomID, to, msg)
			}
			if who == "" {
				return publishErr(cmd, jsonOut,
					"identity is required (REPORT/AUTHOR invariant): set --as or $BASHY_PRINCIPAL",
					topic, roomID, to, msg)
			}
			// Addressing is enforced, not merely documented. An unaddressed
			// publish has no topic to match, no recipient to route to and no room
			// to scope it: it lands on the timeline carrying no more information
			// than silence, while reporting success. Refusing costs the caller one
			// flag and makes the exit code mean something.
			if topic == "" && to == "" && roomID == "" {
				return publishErr(cmd, jsonOut,
					"addressing is required: pass at least one of --topic, --to, or --room (an unaddressed notification reaches nobody)",
					topic, roomID, to, msg)
			}

			// One enforcement path: the CLI and every programmatic publisher go
			// through Publish, so the principal/addressing rules cannot hold for a
			// human and quietly not hold for the coach.
			if err := Publish(Notification{
				Principal: who,
				Topic:     topic,
				To:        to,
				Room:      roomID,
				Body:      msg,
				Priority:  priority,
			}); err != nil {
				return publishErr(cmd, jsonOut, err.Error(), topic, roomID, to, msg)
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(PublishEnvelope{
					SchemaVersion: SchemaVersion,
					Status:        "ok",
					Principal:     who,
					Topic:         topic,
					Room:          roomID,
					To:            to,
					Message:       msg,
				})
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&topic, "topic", "", "topic to broadcast to")
	f.StringVar(&to, "to", "", "recipient session or role (1:1)")
	f.StringVar(&roomID, "room", "", "room ID for room-scoped publish")
	f.StringVar(&as, "as", "", "sender identity (who is publishing)")
	f.StringVar(&priority, "priority", DeliveryQueued,
		"delivery tier to REQUEST: queued (read at a turn boundary) | interrupt (break into a running turn). "+
			"A subscriber decides whose interrupts it honours, so this is a request, not a guarantee")
	f.BoolVar(&jsonOut, "json", false, "emit a "+SchemaVersion+" JSON envelope")
	return cmd
}

// publishErr reports a refusal in the caller's chosen form and returns an error
// that carries a non-zero exit.
//
// The plain path deliberately returns a bare error for the front end to print;
// only the --json path marks itself reported, because it has already written the
// envelope. Getting this wrong is not cosmetic: the previous implementation
// returned `enc.Encode(env)` — nil whenever the encode succeeded — so every
// --json refusal printed `"status":"error"` and then EXITED 0. The envelope and
// the exit code must agree, and the exit code is the one an agent checks.
func publishErr(cmd *cobra.Command, jsonOut bool, msg, topic, roomID, to, body string) error {
	if !jsonOut {
		return fmt.Errorf("%s", msg)
	}
	enc := json.NewEncoder(cmd.ErrOrStderr())
	enc.SetIndent("", "  ")
	_ = enc.Encode(PublishEnvelope{
		SchemaVersion: SchemaVersion,
		Status:        "error",
		Topic:         topic,
		Room:          roomID,
		To:            to,
		Message:       body,
		Error:         msg,
	})
	return reported("%s", msg)
}

func resolvePrincipal(flag string) string {
	if strings.TrimSpace(flag) != "" {
		return strings.TrimSpace(flag)
	}
	if p := strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("USER"))
}
