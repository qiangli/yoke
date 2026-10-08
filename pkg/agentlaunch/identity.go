// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package agentlaunch

// THE LAUNCH SIDE OF THE INSTANCE CONTRACT.
//
// pkg/fleet owns the records (Family, InstanceStore) and pkg/principal owns
// the ownership claim. Both were leaves with no caller: a launch still minted
// nothing, claimed nothing and stamped nothing, so every consumer of an
// instance identity read an environment that no producer ever wrote. The most
// visible symptom was pkg/weave's sprintLeaseIdentity, which reads
// BASHY_INSTANCE_UUID/BASHY_INSTANCE and therefore recorded an empty instance
// on every conductor lease — the "legacy lease" path, forever.
//
// This file is that producer, and it is deliberately the ONLY one: a launch is
// the single moment at which "which conversation is this" is decided, so
// deciding it anywhere else would give one context two answers.
//
// # What it does NOT do
//
// It does not touch BASHY_PRINCIPAL. That variable still names the family
// (dhnt:agent/<nick>) because bus.ResolveAuthoredActor resolves a BOARD NAME
// from it, and a UUID resolves to no registered agent — stamping the UUID
// there would make every legacy agent unable to author a message. Migrating
// the resolver is #1109; until then the UUID travels beside the principal, not
// instead of it, and principal.SelfInstanceUUID already reads both.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

// Instance environment names. These are the names the EXISTING consumers
// already read (pkg/weave sprintLeaseIdentity, principal.SelfInstanceUUID);
// they are not a new vocabulary.
const (
	// InstanceUUIDEnv carries the context UUID.
	InstanceUUIDEnv = "BASHY_INSTANCE_UUID"
	// InstanceEnv is the shorter alias principal.SelfInstanceUUID falls back to.
	InstanceEnv = "BASHY_INSTANCE"
	// InstanceFamilyEnv carries the FROZEN family configuration id — not the
	// family name. The ratings work in #1269 keys on what the conversation
	// actually ran as, and the catalog may have been edited since.
	InstanceFamilyEnv = "BASHY_INSTANCE_FAMILY"
	// InstanceLabelEnv carries the display label, for rendering only. It is
	// never an address: labels are released and reused.
	InstanceLabelEnv = "BASHY_INSTANCE_LABEL"
	// SessionClaimEnv carries the session digest that proves continuity across
	// the child commands of one harness session.
	SessionClaimEnv = "BASHY_SESSION_CLAIM"
)

// ContextRequest is what a launcher knows at the moment it decides which
// conversation it is starting or continuing.
type ContextRequest struct {
	// Store is the instance store. nil means the host-global default, which is
	// what makes the one-live-owner rule hold across repositories.
	Store *fleet.InstanceStore

	// Resume names a specific context to continue. It is the deliberate
	// recovery path: same UUID, same mailbox watermark, same pending mail.
	Resume string
	// Fresh forces a NEW context even when this session already has one. It is
	// how two parallel contexts on one family are started.
	Fresh bool

	// Label and Handle are passed to a fresh open only. Empty Label derives the
	// next free one from the family's display name.
	Label  string
	Handle string
	// MailFrom is the bus sequence a fresh mailbox begins at — the caller's
	// current timeline head. Passed in so fleet stays a leaf.
	MailFrom int64

	// Binding requests one specific tool:model for this launch. Empty means
	// the family's declared START — bindings[0], which for a cascade is the
	// base rung.
	//
	// A non-empty value must be INSIDE the frozen set. Inside is a selection
	// and keeps the UUID, the mail and the ownership; outside is a
	// reconfiguration and gets fleet.ErrBindingImmutable. This is the only
	// place a launch may ask for a model by name, which is why the refusal
	// lives here rather than being derived from the resolved argv: the Launch's
	// own binding comes from the same catalog entry that produced the family
	// (and for a cascade entry it is the ladder's own row, not one of its
	// rungs), so comparing against it would refuse correct launches and prove
	// nothing about intent.
	Binding string

	// Session is the hashed stable session digest (bus.HashSessionClaim). It is
	// REQUIRED: without it there is nothing to tell a child command of this
	// session apart from a competing driver, and defaulting it would make every
	// unattributed session the same session.
	Session string
	// OwnerPID is the long-lived harness process. Child commands are its
	// descendants, which is how ownership survives a per-turn command exiting.
	OwnerPID int
	// Cwd, Mode, Role and Task are recorded on the ownership card.
	Cwd  string
	Mode string
	Role string
	Task string
}

// InstanceContext is a launch bound to one owned conversation.
type InstanceContext struct {
	Instance fleet.Instance
	// Family is the configuration as DECLARED NOW. For a resumed context it is
	// compared against the instance's frozen copy and may not differ; keeping
	// both makes that comparison auditable rather than implicit.
	Family fleet.Family
	// Binding is the selection for this launch, resolved inside the frozen set.
	Binding string
	// Resumed reports a continued context (same UUID) rather than a fresh one.
	Resumed bool
	// Reused reports an idle active instance that was taken over rather than
	// minted. It is still a resume — same UUID, same mail — but it was found
	// rather than named, so a caller that wants to report "continuing Esme"
	// can tell the two apart.
	Reused bool
}

// ErrNoSessionClaim is returned for a context request with no session digest.
var ErrNoSessionClaim = room.ErrNoSessionClaim

// ErrFamilyUnknown reports a launch whose family this catalog cannot derive.
var ErrFamilyUnknown = errors.New("agentlaunch: launch resolves to no family configuration")

// reconfigured reports an attempt to continue a context under a family
// configuration that is no longer the one it froze.
type reconfigured struct {
	UUID  string
	Was   string
	Now   string
	Label string
}

func (e *reconfigured) Error() string {
	who := strings.TrimSpace(e.Label)
	if who == "" {
		who = e.UUID
	}
	return fmt.Sprintf(
		"%s (instance %s) was opened on family %s and the declared configuration is now %s: %v",
		who, e.UUID, e.Was, e.Now, fleet.ErrBindingImmutable)
}

func (e *reconfigured) Unwrap() error { return fleet.ErrBindingImmutable }

// FamilyForLaunch derives the family configuration a launch runs as.
//
// A NAMED agent's family comes from the catalog, so a cascade entry yields the
// predefined composite with its declared ladder order. A raw tool:model launch
// is its own single family — "claude:opus5 displayed as Esme; claude:sonnet5 is
// a different family" is true whether or not anybody gave it a nickname.
func FamilyForLaunch(l Launch, newCatalog CatalogFunc) (fleet.Family, error) {
	if l.Named() {
		if newCatalog == nil {
			newCatalog = NewCatalog
		}
		f, ok, err := newCatalog().FamilyOf(l.Nick)
		if err != nil {
			return fleet.Family{}, err
		}
		if ok {
			return f, nil
		}
		// A named launch the catalog does not have is not silently downgraded
		// to a raw binding: the name is what the operator asked for, and
		// inventing a family for it would freeze a configuration nobody
		// declared.
		return fleet.Family{}, fmt.Errorf("%w: %s", ErrFamilyUnknown, l.Nick)
	}
	binding := l.Binding()
	if strings.TrimSpace(binding) == "" {
		return fleet.Family{}, fmt.Errorf("%w: %s declares no tool:model binding", ErrFamilyUnknown, l.Tool)
	}
	return fleet.Family{
		Name:     binding,
		Display:  l.ToolName,
		Policy:   fleet.PolicySingle,
		Bindings: []string{binding},
	}, nil
}

// OpenContext binds a launch to the conversation it is going to drive, and
// takes the single live ownership of it.
//
// The order is resume → reuse → mint, and that order is the contract:
//
//   - A named Resume is the deliberate recovery path. Same UUID, same pending
//     mail, same ownership continuity, even from another repository.
//   - This session's own instance (from the environment) continues
//     implicitly. That is what makes a per-turn child command free of a
//     repeated --as.
//   - An IDLE active instance of the family is reused before a new one is
//     minted, so an ordinary relaunch does not burn a cap slot per start. It
//     is found by configuration, never by label — a label is reusable, so
//     matching on one would let a new context inherit a retired context's
//     mailbox, which is the exact thing the acceptance criteria forbid.
//   - Only then is a fresh UUID minted, with an empty mailbox.
//
// Fresh skips the two resume paths entirely: two Fresh opens in one family are
// two parallel contexts with distinct UUIDs and distinct mailboxes.
func OpenContext(l Launch, req ContextRequest) (InstanceContext, error) {
	if strings.TrimSpace(req.Session) == "" {
		return InstanceContext{}, ErrNoSessionClaim
	}
	return openContextWithCatalog(l, req, NewCatalog)
}

func openContextWithCatalog(l Launch, req ContextRequest, newCatalog CatalogFunc) (InstanceContext, error) {
	if strings.TrimSpace(req.Session) == "" {
		return InstanceContext{}, ErrNoSessionClaim
	}
	family, err := FamilyForLaunch(l, newCatalog)
	if err != nil {
		return InstanceContext{}, err
	}
	// An explicitly requested binding must be INSIDE the frozen set; empty
	// resolves to the family's declared start. See ContextRequest.Binding.
	binding, err := family.Select(req.Binding)
	if err != nil {
		return InstanceContext{}, err
	}

	store := req.Store
	if store == nil {
		store = fleet.NewInstanceStore("")
	}
	claim := principal.InstanceClaim{
		Session:  req.Session,
		OwnerPID: req.OwnerPID,
		Cwd:      req.Cwd,
		Mode:     req.Mode,
		Role:     req.Role,
		Task:     req.Task,
	}

	// 1. An explicitly named context.
	if id := strings.TrimSpace(req.Resume); id != "" {
		inst, err := store.Resume(id)
		if err != nil {
			return InstanceContext{}, err
		}
		if err := sameConfiguration(inst, family); err != nil {
			return InstanceContext{}, err
		}
		if err := principal.ClaimInstance(inst, claim); err != nil {
			return InstanceContext{}, err
		}
		return InstanceContext{Instance: inst, Family: family, Binding: binding, Resumed: true}, nil
	}

	if !req.Fresh {
		// 2. The context this session already established. Only its own
		// family's: an environment naming an instance of some OTHER family is
		// a different conversation, not an error, so it falls through to a
		// fresh open. The SAME family name under a different configuration
		// digest is the reconfiguration, and that is refused.
		if id, ok := principal.SelfInstanceUUID(); ok {
			inst, err := store.Resume(id)
			switch {
			case err != nil:
				// A stale or retired BASHY_INSTANCE must not strand a launch.
				// The session simply has no continuable context and gets a
				// new one below.
			case !strings.EqualFold(inst.Family, family.Name):
			default:
				if err := sameConfiguration(inst, family); err != nil {
					return InstanceContext{}, err
				}
				if err := principal.ClaimInstance(inst, claim); err != nil {
					return InstanceContext{}, err
				}
				return InstanceContext{Instance: inst, Family: family, Binding: binding, Resumed: true}, nil
			}
		}

		// 3. An idle active instance of this exact configuration.
		live, err := store.ActiveInstances(family.ID())
		if err != nil {
			return InstanceContext{}, err
		}
		for _, inst := range live {
			if _, owned := principal.InstanceOwner(inst); owned {
				continue
			}
			if err := principal.ClaimInstance(inst, claim); err != nil {
				var busy *room.ErrLive
				if errors.As(err, &busy) {
					// Somebody claimed it between the read and the write.
					// Reading is the reconciliation: try the next candidate.
					continue
				}
				return InstanceContext{}, err
			}
			return InstanceContext{Instance: inst, Family: family, Binding: binding, Resumed: true, Reused: true}, nil
		}
	}

	// 4. A fresh context: new UUID, empty mailbox.
	inst, err := store.Open(family, fleet.OpenOptions{
		Label:    req.Label,
		Handle:   req.Handle,
		MailFrom: req.MailFrom,
	})
	if err != nil {
		return InstanceContext{}, err
	}
	if err := principal.ClaimInstance(inst, claim); err != nil {
		return InstanceContext{}, err
	}
	return InstanceContext{Instance: inst, Family: family, Binding: binding}, nil
}

// sameConfiguration refuses to continue a context whose family configuration
// has moved underneath it.
//
// The comparison is on the frozen FamilyID, which is a digest of the binding
// set, the policy, the declared order and the version. Selecting a different
// model INSIDE that set does not change the id and is therefore allowed here;
// changing the set, the order or the policy does, and needs a new instance plus
// a handoff.
func sameConfiguration(inst fleet.Instance, f fleet.Family) error {
	want := f.ID()
	if inst.FamilyID == want {
		return nil
	}
	return &reconfigured{UUID: inst.UUID, Was: inst.FamilyID, Now: want, Label: inst.Label}
}

// ContextEnv stamps a child environment with the conversation it is driving.
//
// It REPLACES any inherited instance variables rather than appending beside
// them. A spawned worker inherits its launcher's environment, so an unscrubbed
// append would leave the parent's UUID in front of (or behind, depending on
// the consumer's scan direction) the child's, and the two would disagree about
// whose mailbox the child reads.
//
// BASHY_PRINCIPAL is left exactly as PrincipalEnv set it — see the file
// comment for why the UUID travels beside it rather than inside it.
func ContextEnv(base []string, c InstanceContext) []string {
	if strings.TrimSpace(c.Instance.UUID) == "" {
		return base
	}
	drop := []string{
		InstanceUUIDEnv + "=",
		InstanceEnv + "=",
		InstanceFamilyEnv + "=",
		InstanceLabelEnv + "=",
		SessionClaimEnv + "=",
	}
	out := make([]string, 0, len(base)+5)
	for _, kv := range base {
		keep := true
		for _, p := range drop {
			if strings.HasPrefix(kv, p) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	out = append(out,
		InstanceUUIDEnv+"="+c.Instance.UUID,
		InstanceEnv+"="+c.Instance.UUID,
		InstanceFamilyEnv+"="+c.Instance.FamilyID,
	)
	if label := strings.TrimSpace(c.Instance.Label); label != "" {
		out = append(out, InstanceLabelEnv+"="+label)
	}
	return out
}

// SessionEnv stamps the session digest a child inherits so its own claims
// compare equal to its parent's.
//
// Separate from ContextEnv because the digest is not part of the instance: a
// takeover hands the same instance to a DIFFERENT session, and bundling the
// two would have the new owner stamp the old owner's digest.
func SessionEnv(base []string, session string) []string {
	session = strings.TrimSpace(session)
	if session == "" {
		return base
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, SessionClaimEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, SessionClaimEnv+"="+session)
}

// ReleaseContext gives up this session's ownership of a context. The instance
// and its mail are untouched: releasing is "I am done driving", not
// retirement.
func ReleaseContext(c InstanceContext, session string) {
	principal.ReleaseInstance(c.Instance, session)
}

// RetireContext ends a conversation: its mail is archived under its own UUID,
// its label is released for a LATER instance to reuse, and it stops counting
// against the family's active cap.
//
// mail must be a real source — see fleet.ErrMailSourceRequired. The ownership
// claim is released after the archive succeeds, so a failed retirement leaves
// the instance owned by the session that was driving it rather than ownerless
// and half-retired.
func RetireContext(store *fleet.InstanceStore, id string, mail fleet.MailSource, session string) (string, error) {
	if store == nil {
		store = fleet.NewInstanceStore("")
	}
	inst, err := store.Get(id)
	if err != nil {
		return "", err
	}
	archive, err := store.Retire(id, mail)
	if err != nil {
		return "", err
	}
	principal.ReleaseInstance(inst, session)
	return archive, nil
}

// CurrentSessionClaim is the hashed session digest of THIS process, read from
// the environment a launcher stamped.
//
// It is a read, not a derivation: the digest has to be identical across a
// harness and every child command it spawns, so exactly one place may compute
// it (the host, via bus.HashSessionClaim) and everybody else inherits it.
func CurrentSessionClaim() string {
	return strings.TrimSpace(os.Getenv(SessionClaimEnv))
}
