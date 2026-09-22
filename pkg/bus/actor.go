package bus

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

const identityRefusalTopic = "identity.refusal"

// CurrentSessionClaim is injected by the Bashy host. It returns the raw stable
// tool-session identifier inherited by this process for the named fleet agent.
// The value is hashed before comparison and is never persisted or rendered.
var CurrentSessionClaim func(agentName string) string

// HashSessionClaim returns the one-way representation stored in a room card.
// Hosts use this helper when registering a watcher or interactive session.
func HashSessionClaim(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ResolveAuthoredActor resolves the accountable sender of a manually authored
// coordination message. Unlike BoardIdentity (which also supports public board
// reads), this path treats a launcher-stamped agent principal as authoritative
// and requires an unattributed external harness to prove its live session claim.
func ResolveAuthoredActor(explicit string) (string, error) {
	requested := strings.TrimSpace(explicit)
	principal := strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL"))
	if self, ok := agentNameFromPrincipal(principal); ok {
		if requested != "" {
			claimed := resolveBoardName(requested)
			if !strings.EqualFold(self, claimed) {
				return "", authoredActorRefusal(claimed, self,
					fmt.Sprintf("authenticated agent %q cannot author as %q", self, claimed))
			}
		}
		return resolveRegisteredActorClaim(self, self)
	}

	if DetectHarness != nil {
		if tool, detected := DetectHarness(); detected {
			if requested == "" {
				return "", fmt.Errorf("authored communication: %w: running under %s with no claimed agent identity", ErrUnattributed, tool)
			}
			claimed, registered := resolveAgentName(requested)
			if !registered {
				// A HUMAN speaking as themselves from inside an agent session is
				// the case BoardIdentity already documents and tells people to
				// use — "a human meaning to speak as themselves here passes
				// --as <name>" — and it was refused here, so the two halves of
				// one rule disagreed.
				//
				// The contract this guard implements is about AGENT names: an
				// agent harness must not author as a different registered agent.
				// A person is not an agent identity to impersonate, and for
				// non-agent names the OS account remains the trust boundary, as
				// the plain-host branch below already states. So a registered
				// person is allowed through and canonicalized.
				if addr, kind, ok := resolvePrincipalTarget(requested); ok && kind == TargetPerson {
					return addr, nil
				}
				return "", fmt.Errorf("authored communication: --as %q names no registered agent or person", requested)
			}
			return resolveRegisteredActorClaim(claimed, tool)
		}
	}

	// Public host-human behavior is unchanged. The OS login remains the trust
	// boundary for non-agent names. A registered agent name is still protected
	// by its live claim, even when the caller itself is a plain host process.
	actor, err := BoardIdentity(requested)
	if err != nil {
		return "", err
	}
	if registered, ok := resolveAgentName(actor); ok {
		return resolveRegisteredActorClaim(registered, loginName())
	}
	return actor, nil
}

func resolveRegisteredActorClaim(actor, claimant string) (string, error) {
	canonical, registered := resolveAgentName(actor)
	if !registered {
		return "", fmt.Errorf("authored communication: actor %q is not a registered Bashy agent", actor)
	}
	claimID := room.AgentClaimID(canonical)
	card, live, err := room.Find(claimID)
	// A pre-contract inbox watcher used the raw fleet name as its room ID.
	// Keep that already-live claim authoritative until the process exits; every
	// new singleton card uses claimID, so the fallback naturally disappears.
	if err == nil && !live && claimID != canonical {
		card, live, err = room.Find(canonical)
	}
	if err != nil {
		return "", fmt.Errorf("authored communication: inspect session claim for %q: %w", canonical, err)
	}
	if live && card.SessionClaim != "" && CurrentSessionClaim != nil {
		current := HashSessionClaim(CurrentSessionClaim(canonical))
		if current != "" && subtle.ConstantTimeCompare([]byte(card.SessionClaim), []byte(current)) == 1 {
			return canonical, nil
		}
	}
	anchor := 0
	if live {
		anchor = card.OwnerPID
		if anchor <= 0 {
			anchor = card.PID
		}
	}
	if !live {
		// Nobody holds the seat. A process that DECLARES this identity the
		// way the session roster already trusts it (BASHY_AGENT and its
		// aliases) takes the seat now — the same first-come rule an inbox
		// watcher's claim follows — instead of being told to go start a
		// watcher first. A bare codex/claude in a checkout could otherwise
		// join a repo session as <name>@<host>, receive mail, and still be
		// refused when it answered. The guard is unchanged for a HELD seat:
		// a live claim by another session still wins below.
		if declared, ok := declaredAgentIdentity(); ok && strings.EqualFold(declared, canonical) {
			if err := takeAuthoredSeat(canonical, claimant); err != nil {
				return "", fmt.Errorf("authored communication: take seat %q: %w", canonical, err)
			}
			return canonical, nil
		}
		reason := fmt.Sprintf("%s has no matching live session claim for %q, and nobody holds that seat — export BASHY_AGENT=%s in the agent's environment (the seat is then taken on its first message), or keep `bashy inbox --as %s --watch` running in that session",
			claimant, canonical, canonical, canonical)
		return "", authoredActorRefusal(canonical, claimant, reason)
	}
	if anchor <= 0 || !processHasAncestor(anchor) {
		reason := fmt.Sprintf("%s has no matching live session claim for %q", claimant, canonical)
		return "", authoredActorRefusal(canonical, claimant, reason)
	}
	return canonical, nil
}

// declaredAgentIdentity is the process-local identity a bare tool declares
// for itself — the same ladder weave reads to seat a participant on a repo
// session (BASHY_PRINCIPAL is handled by the caller; it is attribution with
// its own rule). Empty when nothing is declared.
func declaredAgentIdentity() (string, bool) {
	for _, env := range []string{"BASHY_AGENT", "WEAVE_AGENT", "BASHY_AGENT_ID", "WEAVE_CONDUCTOR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			name := resolveBoardName(v)
			return name, name != ""
		}
	}
	return "", false
}

// takeAuthoredSeat registers the live claim card for an unclaimed identity on
// behalf of the calling tool session. The card is anchored to this command's
// PARENT — the harness, or the shell it spawned — so it outlives the command
// when the parent does and simply expires with it otherwise (the next message
// takes the seat again; a watcher started meanwhile wins). It carries the
// tool-session claim, so later commands from the same session match by claim
// even when their process lineage differs.
func takeAuthoredSeat(canonical, tool string) error {
	anchor := os.Getppid()
	if anchor <= 1 {
		anchor = os.Getpid()
	}
	claim := ""
	if CurrentSessionClaim != nil {
		claim = HashSessionClaim(CurrentSessionClaim(canonical))
	}
	cwd, _ := os.Getwd()
	return room.Join(room.Card{
		ID:           room.AgentClaimID(canonical),
		Nick:         canonical,
		Principal:    canonical,
		SessionClaim: claim,
		Tool:         tool,
		Mode:         "authored",
		Task:         "seat taken on first authored message",
		PID:          anchor,
		OwnerPID:     anchor,
		Cwd:          cwd,
	})
}

func agentNameFromPrincipal(principal string) (string, bool) {
	before, nick, ok := strings.Cut(principal, "agent/")
	if !ok || strings.TrimSpace(before) == "" || strings.TrimSpace(nick) == "" {
		return "", false
	}
	name := resolveBoardName(nick)
	return name, name != ""
}

// authoredActorRefusal warns the registered identity whose name was claimed.
// The rejected authored body is deliberately not accepted as an argument, so
// it cannot accidentally enter the warning timeline.
func authoredActorRefusal(claimed, actual, reason string) error {
	if owner, ok := resolveAgentName(claimed); ok {
		warning := fmt.Sprintf("Bashy refused an authored-message identity claim for %s by %s", owner, strings.TrimSpace(actual))
		if err := Publish(Notification{
			Principal: "bashy-identity-guard",
			Topic:     identityRefusalTopic,
			To:        owner,
			Body:      warning,
		}); err != nil {
			return fmt.Errorf("authored communication: %s; warning %q failed: %v", reason, owner, err)
		}
	}
	return fmt.Errorf("authored communication: %s", reason)
}
