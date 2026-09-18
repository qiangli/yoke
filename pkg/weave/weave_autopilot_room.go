package weave

// THE PER-REPO CONDUCTOR'S ROOM.
//
// `bashy sprint` gave the plan-layer conductor a room; this gives the same
// thing to the one actually driving a repo. They are different seats and both
// need reaching: a sprint spans repos and its conductor decides WHAT gets done,
// while the autopilot lease-holder is who is executing HERE, right now, and is
// the one an agent about to touch this repo needs to talk to.
//
// The rule is the same one pkg/role states: acquiring the lease opens a room,
// releasing it closes one. So is the reason it cannot be skipped — an open room
// with a dead holder is a lie, and a closed room with a live campaign is a dead
// letterbox.
//
// # Directly imported here, unlike in steward, and that is deliberate
//
// pkg/steward reaches meetroom through a hook because it sits in the cross-OS
// build canary that meet's transitive shell interpreter cannot satisfy. No such
// constraint applies to pkg/weave, which already links the interpreter to run
// agents at all. Adding a hook here would be indirection with nothing behind
// it, and an injection seam nobody needs is a seam somebody later has to
// understand.
//
// # The address lives beside the lease, not in the queue
//
// The queue is the durable record of WORK. A room address is a live pointer
// that is meaningless the moment the room closes, so putting it in the queue
// would mean a campaign's history accumulated dead addresses — and a reader
// replaying it would have to know which ones to ignore.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/yoke/pkg/role"
	"github.com/qiangli/yoke/pkg/role/meetroom"
)

// autopilotRoomPath is where a repo's campaign room address is recorded.
func autopilotRoomPath(dir string) string {
	return filepath.Join(dir, "orchestrator-room.json")
}

// repoAssignment names this repo's campaign as a role assignment.
//
// The Ref is the queue TAG rather than the repo path, for the same containment
// reason the tag exists: the bus topic is visible to every subscriber, and a
// path would hand each of them the origin location.
func autopilotAssignment(dir string) role.Assignment {
	return role.Assignment{
		Kind:  role.Conductor,
		Ref:   filepath.Base(dir),
		Title: "repo campaign",
	}
}

// openAutopilotRoom opens the campaign room when the lease is acquired.
//
// Failure is REPORTED to the caller, never fatal. An autopilot that cannot open
// a room still drives the queue, and stopping a campaign because the intercom
// is down would trade the work for the ability to discuss it.
func openAutopilotRoom(dir, holder string) (*role.Contact, error) {
	c, err := meetroom.Assume(autopilotAssignment(dir), holder)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	if err := weaveWriteFile(autopilotRoomPath(dir), b, 0o600); err != nil {
		return c, err
	}
	return c, nil
}

// loadAutopilotRoom reads a repo's campaign room address, if one is open.
func loadAutopilotRoom(dir string) (*role.Contact, error) {
	b, err := os.ReadFile(autopilotRoomPath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var c role.Contact
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// closeAutopilotRoom closes the campaign room and forgets its address.
//
// Called on release, and on a TAKEOVER of an expired lease — the holder that
// let it expire is by definition not going to close its own room, and a
// successor inheriting a channel addressed to a dead predecessor is the exact
// state the sweep exists to prevent.
func closeAutopilotRoom(dir, actor string) error {
	c, err := loadAutopilotRoom(dir)
	if err != nil || c == nil {
		return err
	}
	rerr := meetroom.Release(c, actor)
	if err := os.Remove(autopilotRoomPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return rerr
}

// autopilotRoomLine renders what to tell the operator after an acquire.
func autopilotRoomLine(dir, holder string) string {
	c, err := openAutopilotRoom(dir, holder)
	switch {
	case err != nil && c == nil:
		return fmt.Sprintf("  no room (%v) — reachable on bus %s only",
			err, autopilotAssignment(dir).Topic())
	case err != nil:
		return fmt.Sprintf("  room %s open but not recorded (%v)", c.String(), err)
	default:
		return "  reachable at " + c.String()
	}
}
