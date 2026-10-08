// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package principal

// Instance identity for a session: claim it once, then stop saying who you
// are.
//
// This file is where the two halves of the contract meet. pkg/fleet owns the
// instance RECORD (UUID, frozen family, label, mailbox watermark) and stays a
// leaf; pkg/room owns the one-live-owning-session CLAIM. Neither may import
// the other's concerns, and pkg/principal already imports both — it is the
// package whose whole job is "who is this process" — so the glue belongs here
// and nowhere else. No new store and no new transport: a claim is a room card
// and an identity is a URN, exactly as they already were for agents.
//
// # Why "once" is the point
//
// An external harness — an operator's Claude Code window, a Codex run — is not
// launched by bashy, so nothing stamps its identity into its environment for
// it. Before this, every single command it ran had to re-assert `--as NAME`,
// and a command that forgot was either refused or, worse, attributed to the
// OS login. Establishing the instance once and exporting its URN makes the
// identity ambient for the rest of the session, which is what the acceptance
// criterion "external identity works without repeated --as" asks for.
//
// # Why child commands are not competitors
//
// A session is a conversation, not a process. The harness shells out a fresh
// short-lived `bashy` per turn, so pids differ constantly while the owner does
// not. The claim is therefore decided on the session digest (see
// room.ClaimSession): same digest, same owner, however many pids. A DIFFERENT
// digest against a live incumbent is a second driver for one context, and that
// is refused — across repositories, because the room is host-global and the
// claim does not live in a checkout (#1245).

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// InstanceEnv is the fallback by which a session names its instance when it
// has not exported a full URN. BASHY_PRINCIPAL is still preferred: it is one
// variable that says both the kind and the name, so it cannot disagree with
// itself.
const InstanceEnv = "BASHY_INSTANCE"

// ErrNoInstance reports that this process is not running as an instance.
//
// It is distinct from "the instance is unknown". Not having claimed one is the
// ordinary state of a plain host command and is nobody's mistake; naming a
// UUID that has no record is a real error, and conflating them would tell
// every `bashy ls` that its identity was broken.
var ErrNoInstance = errors.New("principal: this session has not claimed an instance")

// InstanceClaim is what a session presents to take or keep ownership.
type InstanceClaim struct {
	// Session is the one-way digest of the stable tool-session identifier
	// (bus.HashSessionClaim). It is the thing compared; the raw vendor session
	// id must never reach a room card.
	Session string
	// OwnerPID is the long-lived harness process, when known. Short-lived child
	// commands are its descendants.
	OwnerPID int
	// Cwd is the checkout this session is working in. RECORDED, never
	// compared: it is what makes a cross-repo refusal legible ("already owned
	// by a session in <other repo>") without ever becoming part of the identity
	// — the moment the claim depended on the directory, two repos would each
	// think they owned the conversation.
	Cwd  string
	Mode string
	Role string
	Task string
}

// InstanceURN is the canonical identifier of an instance.
func InstanceURN(uuid string) string {
	return URN(KindInstance, strings.TrimSpace(uuid), LocalOwner)
}

// SelfInstanceUUID reports the instance UUID this process is running as,
// reading BASHY_PRINCIPAL first and BASHY_INSTANCE second.
func SelfInstanceUUID() (string, bool) {
	if urn := strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")); urn != "" {
		if kind, name, _, err := ParseURN(urn); err == nil && kind == KindInstance {
			if name = strings.TrimSpace(name); name != "" {
				return name, true
			}
		}
	}
	if id := strings.TrimSpace(os.Getenv(InstanceEnv)); id != "" {
		return id, true
	}
	return "", false
}

// SelfInstance resolves the instance this session established.
//
// This is the "no repeated --as" path: a surface that needs to know who is
// speaking calls this instead of requiring a flag. A session that never
// claimed one gets ErrNoInstance and the caller falls back to its existing
// agent/person resolution — nothing is broken for the legacy identities, which
// is a requirement of this migration, not a courtesy.
func SelfInstance(store *fleet.InstanceStore) (fleet.Instance, error) {
	id, ok := SelfInstanceUUID()
	if !ok {
		return fleet.Instance{}, ErrNoInstance
	}
	if store == nil {
		store = fleet.NewInstanceStore("")
	}
	return store.Resume(id)
}

// ClaimInstance takes or keeps the single live ownership of an instance.
//
// Granted when the instance is unowned, when its previous owner's process is
// gone, or when the CALLER IS THE SAME SESSION — which is how a per-turn child
// command keeps working without re-taking anything. Refused with
// *room.ErrLive when another live session owns it.
func ClaimInstance(inst fleet.Instance, claim InstanceClaim) error {
	if strings.TrimSpace(inst.UUID) == "" {
		return fmt.Errorf("principal: an instance claim requires a UUID")
	}
	if strings.TrimSpace(claim.Session) == "" {
		return room.ErrNoSessionClaim
	}
	if !inst.Active() {
		return fmt.Errorf("principal: instance %s is retired and cannot be claimed", inst.UUID)
	}
	binding := ""
	if len(inst.Bindings) > 0 {
		binding = inst.Bindings[0]
	}
	tool, model, _ := strings.Cut(binding, ":")
	card := room.Card{
		ID:           inst.ClaimID(),
		Principal:    InstanceURN(inst.UUID),
		Tool:         tool,
		Model:        model,
		Binding:      binding,
		Nick:         inst.Label,
		Mode:         claim.Mode,
		Role:         claim.Role,
		Task:         claim.Task,
		SessionClaim: claim.Session,
		OwnerPID:     claim.OwnerPID,
		PID:          claim.OwnerPID,
		Cwd:          claim.Cwd,
	}
	if card.PID == 0 {
		card.PID = os.Getpid()
	}
	if err := room.ClaimSession(card); err != nil {
		var live *room.ErrLive
		if errors.As(err, &live) {
			return fmt.Errorf("%w: %s", err, competingOwnerDetail(inst))
		}
		return err
	}
	return nil
}

// ReleaseInstance gives up ownership held by this session. Releasing one you
// do not hold is a no-op.
func ReleaseInstance(inst fleet.Instance, sessionClaim string) {
	room.ReleaseSession(inst.ClaimID(), sessionClaim)
}

// InstanceOwner reports the live owning session of an instance, if any.
func InstanceOwner(inst fleet.Instance) (room.Card, bool) {
	return room.SessionOwner(inst.ClaimID())
}

// competingOwnerDetail says WHERE the incumbent is working, because the
// commonest form of this refusal is one operator driving the same instance
// from two checkouts and the only useful next question is "which other one".
func competingOwnerDetail(inst fleet.Instance) string {
	owner, ok := room.SessionOwner(inst.ClaimID())
	if !ok {
		return "another session owns it"
	}
	where := strings.TrimSpace(owner.Cwd)
	if where == "" {
		where = "an unreported directory"
	}
	label := strings.TrimSpace(inst.Label)
	if label == "" {
		label = inst.UUID
	}
	return fmt.Sprintf("%s is already driven by a live session in %s; hand off or open a new instance", label, where)
}
