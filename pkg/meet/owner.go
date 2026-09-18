// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package meet

import (
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/role"
)

// roomView is the room state AS A READER SEES IT — the stored State plus the
// two things a reader cannot derive for itself.
//
// It is a PROJECTION and never a stored shape. State is persisted with
// json.MarshalIndent, so a resolved holder written onto State would be written
// to disk, and a copied holder is exactly what State.DefaultTo exists to
// prevent: the address is the seat, the holder is decided every time somebody
// reads it, and a handover re-targets mail already in flight. Resolving here —
// at the moment the state is sent — keeps that property while still letting a
// browser say who is accountable right now.
type roomView struct {
	*State
	// Owner is the seat accountable for this room, resolved to a name.
	Owner string `json:"owner,omitempty"`
	// OwnerTitle is what that owner is CALLED here: "project manager" in a
	// sprint's room, "facilitator" in an ordinary meeting. The words come from
	// role.Title so meet, sprint and todo cannot drift on how an owner is named.
	OwnerTitle string `json:"owner_title,omitempty"`
}

// viewOf projects a room for a reader. A nil state projects to nil so a caller
// can hand it straight to writeJSON.
func viewOf(st *State) any {
	if st == nil {
		return nil
	}
	owner, title := ownerOf(st)
	return roomView{State: st, Owner: owner, OwnerTitle: title}
}

// ownerOf answers "who is accountable for this room, and what are they called".
//
// Two sources, in precedence order, and the order is the point:
//
//   - DefaultTo names the SEAT this room advertises for — `conductor:99` for a
//     sprint's room. That seat outranks the facilitator because it is what the
//     room is FOR: unaddressed mail already lands there, so a reader who is
//     shown anyone else is being pointed at the wrong person.
//   - Chair is the meeting's facilitator, which is what `--owner` sets on an
//     ordinary meeting ("ONE FLAG, DOMAIN TITLES").
//
// A seat with no holder returns an empty name and a non-empty title: the room
// still HAS a project manager's seat, it is just vacant, and saying so is more
// honest than naming the facilitator instead.
func ownerOf(st *State) (name, title string) {
	if label := strings.TrimSpace(st.DefaultTo); label != "" {
		holder, _ := bus.RoleHolderFor(label)
		return canonAgent(strings.TrimSpace(holder)), titleForRoleLabel(label)
	}
	if chair := strings.TrimSpace(st.Chair); chair != "" {
		return chair, string(role.Facilitator)
	}
	return "", ""
}

// titleForRoleLabel names the seat a role label addresses.
//
// A conductor label is a SPRINT's seat, and the domain's word for that owner is
// "project manager" — not "conductor", which is the internal kind. Anything
// else keeps its own label ("steward"), because inventing a title for a seat
// this package does not own would be a guess presented as a fact.
func titleForRoleLabel(label string) string {
	kind := label
	if i := strings.IndexByte(label, ':'); i > 0 {
		kind = label[:i]
	}
	if strings.EqualFold(kind, string(role.Conductor)) {
		return string(role.ProjectManager)
	}
	return strings.ToLower(strings.TrimSpace(kind))
}
