package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	whodb "github.com/qiangli/coreutils/pkg/who"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/room"
)

// The live-instance registry moved to pkg/room — it is the host room's membership,
// shared by every launch path (chat/weave/foreman/meet), not a chat-private board.
// What stays here is how an agent names itself, and what that name keys.
//
// AN AGENT IS A SINGLETON IDENTITY ON A HOST.
//
// This file used to key everything on `<nick>-<pid>`, which made an agent's
// identity a property of a PROCESS. Two launches of one agent then produced two
// members sharing a name, an API key, a kb and a bus cursor — but not a
// conversation. They were "the same agent" only by spelling, which is exactly
// what made them not the same. Mixing their contexts is a correctness hazard,
// not an untidiness: an agent handed two tasks' history answers confidently
// about the wrong one.
//
// So the id is the agent's NAME, and everything the agent owns hangs off it:
//
//	~/.bashy/ctl/<h>.sock              the control socket (one live session)
//	~/.bashy/events/<h>.ndjson         the structured event stream
//	~/.bashy/sessions/logs/<h>.log     the capture
//	<state>/agent-data/ycode-<id>      the conversation store
//
// Note what is ABSENT from the key: the pid, and the working directory. Dropping
// the pid is what lets room.Join enforce the singleton. Dropping the cwd is the
// same decision seen from the other side — one identity means one memory, so an
// agent recognises you in a second repo instead of starting over. An agent that
// SHOULD start over is a different agent: `bashy agent clone`.

// agentID is the agent's identity on this host, and the id a human types:
// `bashy chat steer elif "..."`.
//
// A named agent is its name. An unnamed `tool:model` launch has no identity of
// its own, so it becomes its sanitized binding — still one-per-host, because it
// still resolves to one store and one key. Naming it is how you get a second.
func agentID(l Launch) string {
	label := strings.TrimSpace(l.Nick)
	if label == "" {
		label = l.Binding()
	}
	return room.AgentClaimID(label)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// bashyPath returns ~/.bashy/<parts...> WITHOUT creating it; bashyDir is the
// creating form. Split so a read-only reader (the resource map) can name the
// location without a side effect.
func bashyPath(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home, ".bashy"}, parts...)...), nil
}

// SessionsRoot is where chat sessions and their capture logs live
// (~/.bashy/sessions). It creates nothing.
func SessionsRoot() (string, error) { return bashyPath("sessions") }

// bashyDir returns ~/.bashy/<parts...>, created.
func bashyDir(parts ...string) (string, error) {
	dir, err := bashyPath(parts...)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// sessionSock is short by construction. A unix socket address caps at ~104 bytes,
// and an agent name under a long home path can blow straight past it — at which
// point agentpty degrades to a polling file channel and steering silently gets
// worse. Hashing the id keeps the path a fixed 12 hex chars however long the
// name is.
func sessionSock(id string) (string, error) {
	dir, err := bashyDir("ctl")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, shortHash(id)+".sock"), nil
}

// sessionEventsPath is where a tool streams its structured events. It is
// ADVERTISED on the member's card as EventsPath, so no reader has to recompute
// this hash — see cardEventsFile.
func sessionEventsPath(id string) (string, error) {
	dir, err := bashyDir("events")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, shortHash(id)+".ndjson"), nil
}

// sessionLog opens this agent's capture. Truncating is deliberate: the capture
// follows the LIVE session and `chat attach` tails it from the top, so replaying
// a previous session's transcript as though it were this one would be the same
// class of lie as mixing two tasks' context.
func sessionLog(id string) (path string, sink io.Writer, closeFn func()) {
	dir, err := bashyDir("sessions", "logs")
	if err != nil {
		return "", nil, nil
	}
	path = filepath.Join(dir, shortHash(id)+".log")
	f, err := os.Create(path)
	if err != nil {
		return "", nil, nil
	}
	return path, f, func() { _ = f.Close() }
}

// publishSessionTTY gives an agent session a stable bashy-owned terminal line.
// The real PTY slave is still the terminal; the file under the pty dir is only
// the ut_line namespace write(1) resolves under its agent devDir seam.
func publishSessionTTY(id, realTTY string) (line, link string, err error) {
	line = strings.TrimSpace(id)
	if line == "" {
		line = "agent"
	}
	dir := whodb.PTYDirForEnv(os.Environ())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	link = filepath.Join(dir, line)
	_ = os.Remove(link)
	if err := os.Symlink(realTTY, link); err != nil {
		return "", "", err
	}
	return line, link, nil
}

// refuseIfSessionLive reports the agent's own live SESSION as an error, for
// launch paths that would otherwise collide with it inside the tool.
//
// Only for tools that lock their conversation store — today that is ycode, and
// the gate is the same constant agentlaunch uses to decide who gets a per-agent
// store at all, so the two cannot drift into disagreeing about which tools have
// one. Everything else can run a turn beside a session perfectly well, and
// refusing those would invent a restriction the model does not need: the
// singleton is about one identity's CONTEXT, not about rationing a name.
func refuseIfSessionLive(l Launch) error {
	if l.ToolName != agentlaunch.YcodeToolName {
		return nil
	}
	id := agentID(l)
	card, found, err := room.Find(id)
	if err != nil || !found || card.ID != id {
		return nil
	}
	if card.PID == os.Getpid() {
		return nil // our own session; the tool is not racing itself
	}
	return errAgentLive(id, card.PID, card.Cwd)
}

// errAgentLive is what a launcher returns instead of starting a second session
// for one identity. It names BOTH ways forward, because a refusal with no next
// step is how an operator learns to reach for --force.
func errAgentLive(id string, pid int, cwd string) error {
	where := ""
	if strings.TrimSpace(cwd) != "" {
		where = ", " + cwd
	}
	return fmt.Errorf(
		"chat: agent %s is already live (pid %d%s) — an agent is one identity, so it is not started twice\n"+
			"  bashy chat --agent %s --attach   watch and steer the live session\n"+
			"  bashy agent clone %s            a second agent on the same binding, with its own context",
		id, pid, where, id, id)
}
