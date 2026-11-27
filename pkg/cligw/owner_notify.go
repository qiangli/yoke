package cligw

import (
	"fmt"
	"log/slog"
	"os/user"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
)

// The owner mail path is the host message board (`bashy inbox` reads it):
// the same bus.PostMessage every other door and fleet notice uses. No new
// transport is added here, and no vendor credential travels in the post —
// only the binding, the tool and the one-time sign-in command.

// ownerInboxPost appends one post to the owner's board. A package var so
// tests can count posts instead of writing to the real board.
var ownerInboxPost = bus.PostMessage

// ownerInboxAddress resolves the board address of the human who owns this
// door: the current OS user's mailbox, the `bashy inbox human` lane. A
// package var so tests can name a fake owner.
var ownerInboxAddress = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Username)
}

// NotifyOwnerNeedsLogin is the production ServerOptions.OnNeedsLogin: it
// posts the one-time sign-in step to the owner's inbox. The Server calls
// it once per needs-login episode (on the transition into the state), so
// repeated failures in one episode post nothing further; after recovery a
// new episode posts again. A failed post is logged, never fatal — the
// binding's needs-login state and the 401 answer do not depend on it.
func NotifyOwnerNeedsLogin(nl NeedsLogin) {
	to := ownerInboxAddress()
	if to == "" {
		slog.Default().Warn("cligw: needs-login owner notice skipped: no owner address", "agent", nl.Agent, "tool", nl.Tool)
		return
	}
	body := fmt.Sprintf("cligw: binding %s (tool %s) needs-login: the vendor login is missing or expired. Sign in once with `%s`, then retry — the door serves again with no restart. No vendor credential is stored by the door; agents keep using door tokens only.",
		nl.Agent, nl.Tool, nl.LoginCommand)
	if err := ownerInboxPost(bus.Post{From: "cligw", To: to, Topic: "needs-login", Body: body}); err != nil {
		slog.Default().Warn("cligw: needs-login owner notice failed", "agent", nl.Agent, "tool", nl.Tool, "err", err)
	}
}
