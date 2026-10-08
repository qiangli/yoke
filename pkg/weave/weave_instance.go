// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package weave

// THE INSTANCE IDENTITY OF A WEAVE RUN.
//
// A weave run IS a conversation: the orchestrator spawns a worker CLI, that
// worker has one transcript, and it ends when the run ends. So the run is
// exactly the unit that owns an instance UUID, and the run record is where the
// UUID and the FROZEN family configuration belong.
//
// Recorded, not re-derived. The ratings work (#1269) has to ask "what did this
// run actually execute as", and the catalog may have been re-pointed since —
// `bashy agent set` edits a row in place, so resolving the family name again
// at read time would attribute a run to a configuration it never ran. The
// snapshot on the item is the answer; the catalog is only its source at claim
// time.
//
// # Why the session digest is minted here
//
// room.ClaimSession decides one-live-owner on a session digest, and that digest
// has to be identical across the worker harness and every `bashy` command the
// worker shells out per turn. The worker cannot supply it — it does not exist
// until the worker starts, and a value the worker invented would differ between
// its own turns. The orchestrator mints it once per run instead and the worker
// inherits it through the environment, which is the same inheritance the
// contract already relies on ("the OS boundary remains the trust boundary").
//
// It is STORED rather than recomputed so that `weave start --resume` continues
// the same ownership. A digest derived from the clock or the pid would make a
// resumed run look like a competing session against its own instance.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// weaveRunSessionClaim mints the stable session digest for a run.
//
// It is one-way (the room never stores a raw session identifier) and it is a
// pure function of the run, so a resume of the same run recomputes the same
// value even if the stored one was lost.
func weaveRunSessionClaim(id int64, branch string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("weave-run/%d/%s", id, strings.TrimSpace(branch))))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// weaveOpenRunInstance binds a run to its conversation, recording the UUID and
// the frozen family id on the item.
//
// A run that ALREADY has an instance resumes it: `weave start --resume` is the
// recovery path the contract names, and it must keep the UUID, the pending mail
// and the ownership. A run with none opens a fresh context.
//
// MOST FAILURES ARE NOT FATAL. A worker that cannot take an instance identity
// is the pre-instance worker, which is what every live run is today; refusing
// to launch it would make this story an outage rather than a feature. A raw
// `weave start claude` or an agent this host's catalog does not have resolves
// to no family at all, and that must keep working exactly as before — it
// simply records no conversation.
//
// Two failures DO block the run, because starting anyway would do the specific
// damage this story exists to prevent:
//
//   - a competing owning session (*room.ErrLive): two drivers of one
//     conversation is how a context gets interleaved by two models at once;
//   - a reconfiguration (fleet.ErrBindingImmutable): continuing a recorded
//     conversation under a family configuration it never agreed to makes every
//     earlier turn unattributable to the model that produced it.
//
// Cap exhaustion is deliberately NOT fatal here. A run's instance is retired
// only when its record is deleted, so active instances accumulate across a
// fleet's lifetime; letting the default cap refuse the ninth `weave start` of
// an agent would turn an identity ceiling into a fleet outage. The cap still
// governs interactive opens, where it is the operator's own choice. Retiring a
// context when its run reaches a terminal state is the follow-up that makes
// the cap meaningful for weave too.
func weaveOpenRunInstance(it *weaveItem, l *weaveAgentLaunch, branch string) (agentlaunch.InstanceContext, error) {
	if it == nil || l == nil {
		return agentlaunch.InstanceContext{}, nil
	}
	session := strings.TrimSpace(it.SessionClaim)
	if session == "" {
		session = weaveRunSessionClaim(it.ID, branch)
	}
	req := agentlaunch.ContextRequest{
		Resume:   strings.TrimSpace(it.Instance),
		Session:  session,
		OwnerPID: os.Getpid(),
		Cwd:      it.Workspace,
		Mode:     "weave",
		Role:     it.Owner,
		Task:     fmt.Sprintf("weave #%d: %s", it.ID, it.Title),
	}
	// A run with no recorded instance wants a NEW conversation, not whichever
	// idle one the family happens to have: two concurrent runs of one agent
	// are two separate pieces of work, and reusing an idle context would give
	// the second run the first one's transcript identity and mailbox.
	if req.Resume == "" {
		req.Fresh = true
	}
	ctx, err := agentlaunch.OpenContext(agentlaunch.Launch(*l), req)
	if err != nil {
		if weaveInstanceBlocksRun(err) {
			return agentlaunch.InstanceContext{}, err
		}
		return agentlaunch.InstanceContext{}, nil
	}
	it.Instance = ctx.Instance.UUID
	it.InstanceFamily = ctx.Instance.FamilyID
	it.InstanceLabel = ctx.Instance.Label
	it.SessionClaim = session
	return ctx, nil
}

// weaveInstanceBlocksRun reports the two refusals a run must not start
// through. See weaveOpenRunInstance for why everything else is tolerated.
func weaveInstanceBlocksRun(err error) bool {
	var live *room.ErrLive
	return errors.As(err, &live) || errors.Is(err, fleet.ErrBindingImmutable)
}

// weaveInstanceEnv stamps a run's recorded identity onto the child
// environment.
//
// It is a pure function of the ITEM, not of a live store lookup, so the worker
// is stamped with the identity the run recorded — the same value the ratings
// reader will see — and so weaveChildEnv stays testable without a room.
func weaveInstanceEnv(env []string, it *weaveItem) []string {
	if it == nil || strings.TrimSpace(it.Instance) == "" {
		return env
	}
	env = agentlaunch.ContextEnv(env, agentlaunch.InstanceContext{
		Instance: fleet.Instance{
			UUID:     it.Instance,
			FamilyID: it.InstanceFamily,
			Label:    it.InstanceLabel,
		},
	})
	return agentlaunch.SessionEnv(env, it.SessionClaim)
}

// weaveRetireRunInstance ends a run's conversation: its mail is archived under
// its own UUID and its label is released for a later instance to reuse.
//
// Called when a run REACHES A TERMINAL STATE, not when its wrapper merely
// exits — a killed run that will be resumed still owns its context, and
// retiring it would archive the mail its next attempt has not read yet.
//
// The mail source is bus.InstanceMail — the real one. fleet.Retire refuses a
// nil source rather than writing an empty archive that claims to have
// preserved something, so passing the real reader is what makes "retiring
// archives mail without deleting it" a fact rather than a field.
func weaveRetireRunInstance(it *weaveItem) (string, error) {
	if it == nil || strings.TrimSpace(it.Instance) == "" {
		return "", nil
	}
	return agentlaunch.RetireContext(nil, it.Instance, bus.InstanceMail, it.SessionClaim)
}
