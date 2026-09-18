package meet

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/fleet"
)

// RoomSecretaryStartRequest asks the embedding host to choose one concrete
// secretary from its fleet. Exclude names the room roles that separation of
// powers forbids the secretary from also holding.
type RoomSecretaryStartRequest struct {
	Room    string
	Agent   string
	Band    int
	Exclude []string
}

// StartRoomSecretary is supplied by bashy. It selects and validates a fleet
// binding; Meet owns transcript persistence and invokes the selected model only
// when synthesis work exists, so an idle room does not consume an LLM session.
var StartRoomSecretary func(context.Context, RoomSecretaryStartRequest) (string, error)

// ValidateRoomSecretary lets an embedding host tighten the generic routability
// rule. Bashy uses it to require a named fleet agent rather than a bare tool.
var ValidateRoomSecretary func(string) error

func ensureRoomSecretary(ctx context.Context, st *State) error {
	// A board keeps no minutes and spawns no secretary. This is belt-and-braces:
	// the create paths already refuse to arm SecretaryPending for a board, but if
	// one ever slipped through it must not spawn an agent here.
	if st == nil || !st.SecretaryPending || st.board() {
		return nil
	}
	base, err := baseDir()
	if err != nil {
		return err
	}
	l, err := lockfile.AcquireWithin(filepath.Join(base, "secretary-start.lock"), 30*time.Second,
		lockfile.Holder{Intent: "activate meet secretary"})
	if err != nil {
		return fmt.Errorf("meet: lock secretary activation: %w", err)
	}
	defer l.Release()

	fresh, err := loadState(st.ID)
	if err != nil {
		return err
	}
	if !fresh.SecretaryPending {
		st.Secretary, st.SecretaryPending = fresh.Secretary, false
		return nil
	}
	if StartRoomSecretary == nil {
		return fmt.Errorf("meet: %s needs a secretary but this host cannot select agents", st.ID)
	}
	excluded := append(append([]string{}, fresh.Participants...), fresh.Chair)
	name, err := StartRoomSecretary(ctx, RoomSecretaryStartRequest{
		Room: fresh.ID, Band: fresh.SecretaryBand, Exclude: excluded,
	})
	if err != nil {
		return fmt.Errorf("meet: activate secretary: %w", err)
	}
	name = canonAgent(strings.TrimSpace(name))
	if _, ok := fleet.New().Agent(name); !ok {
		return fmt.Errorf("meet: secretary %q is not a named agent in `bashy agent list`", name)
	}
	for _, other := range excluded {
		if strings.EqualFold(canonAgent(other), name) {
			return fmt.Errorf("meet: %s cannot be both secretary and another room role", name)
		}
	}
	fresh.Secretary, fresh.SecretaryPending = name, false
	if err := fresh.Validate(); err != nil {
		return err
	}
	if err := fresh.save(); err != nil {
		return err
	}
	st.Secretary, st.SecretaryPending = name, false
	_, err = record(fresh, "invite", procedural(fresh), string(RoleSecretary),
		"activated secretary "+seatLabel(name))
	return err
}
