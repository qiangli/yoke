package ask

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// DirEnv overrides the ask root, mirroring room.Dir's BASHY_ROOM_DIR so a test
// gets an isolated store with the same idiom.
const DirEnv = "BASHY_ASK_DIR"

const (
	requestFile = "request.json"
	valueFile   = "value"
	answerFile  = "answer"
	answerSock  = "answer.sock"
	// answeredFile marks a request that has been answered. It is the single-use
	// latch, and it is an explicit marker rather than an inference from "the
	// socket is gone" because the two transports close differently — inferring it
	// let a second answer land on the file channel after the socket had closed,
	// writing a plaintext value into a directory with nobody left to read or
	// unlink it.
	answeredFile = "answered"
	// channelFileName records which answer transport the listener opened, so the
	// answering side never has to guess.
	channelFileName = "channel"
)

// Dir returns the ask root.
//
// The location is a security decision, not a convenience one, and os.TempDir is
// specifically WRONG here — it is the /tmp/x habit this command exists to retire:
//
//   - On Linux /tmp is shared by every user on the box. The sticky bit stops
//     others DELETING your files; it does nothing to stop them PRE-CREATING a
//     path you are about to write, as a symlink into somewhere they can read.
//   - It is not cleaned on a schedule you control, so a value outlives the task,
//     the session, and often the reboot — into backups and file indexers.
//
// So: $XDG_RUNTIME_DIR when set (a per-user tmpfs, mode 0700, cleared at logout —
// the best available home for a secret at rest), else ~/.bashy/ask, which is at
// least per-user by construction. The home fallback IS on a journaled and possibly
// backed-up filesystem; that is a real cost, stated in the docs rather than hidden.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv(DirEnv)); d != "" {
		return d
	}
	if rt := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); rt != "" {
		return filepath.Join(rt, "bashy", "ask")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".bashy", "ask")
}

// ensureDir creates a directory owner-only and then VERIFIES it.
//
// The verification is the point. os.MkdirAll succeeds silently on a directory that
// already exists — whoever owns it, whatever its mode, and even when it is a
// symlink pointing somewhere else entirely. On a shared machine that turns "create
// my private directory" into "write my secrets wherever the first user to guess
// this path decided". Creating is not the same as owning, so we check.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("ask: creating %s: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("ask: %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("ask: refusing to use %s — it is not a real directory", path)
	}
	if err := checkOwner(path, fi); err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		// Tighten rather than refuse: an older layout or a permissive umask should
		// not strand an operator on an error they did not cause. If we cannot
		// tighten it, we do refuse — a directory others can read is not a place to
		// put a secret.
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("ask: %s is mode %#o and cannot be tightened: %w", path, perm, err)
		}
	}
	return nil
}

// requestDir is the per-request directory. One directory per id keeps the
// metadata, the answer channel and the delivered value together, so there is
// exactly one thing to reap.
func requestDir(id string) string { return filepath.Join(Dir(), id) }

// save writes the request metadata.
//
// O_EXCL, because the id is ours and a pre-existing file at that path means
// something is wrong — either a collision (impossible at 128 bits) or another
// process that got there first. Either way, refusing beats overwriting.
func save(r Request) error {
	// Verify the ROOT before the per-request directory, not just implicitly via
	// MkdirAll. A pre-existing root left at 0755 — by an older umask, another tool,
	// or a restore from backup — is not tightened by creating a child inside it,
	// and MkdirAll reports success either way. The values themselves stay safe
	// (each request directory is 0700 and each value 0600), but a readable root
	// lets any local user ENUMERATE request ids, which is exactly the identifier
	// the answer channel is keyed on. Verify the root explicitly.
	if err := ensureDir(Dir()); err != nil {
		return err
	}
	dir := requestDir(r.ID)
	if err := ensureDir(dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, requestFile),
		os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("ask: writing the request: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Load reads one request by exact id.
func Load(id string) (Request, error) {
	var r Request
	b, err := os.ReadFile(filepath.Join(requestDir(id), requestFile))
	if err != nil {
		return r, fmt.Errorf("ask: no request %s", id)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("ask: request %s is unreadable: %w", id, err)
	}
	return r, nil
}

// List returns the live requests, newest first, REAPING as it reads.
//
// Reading is the reconciliation — the same doctrine as room.Members, and for the
// same reason: a sweeper daemon is a second thing that can be down, and a board
// that asserts a dead request is live is worse than one that is merely stale. It
// also means cleanup happens on every invocation without anything scheduling it.
func List() ([]Request, error) {
	root := Dir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	now := time.Now()
	var out []Request
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Load(e.Name())
		if err != nil {
			// A directory with no readable request is debris from a crash between
			// mkdir and write. Remove it rather than reporting it forever.
			if stale(filepath.Join(root, e.Name()), now) {
				_ = os.RemoveAll(filepath.Join(root, e.Name()))
			}
			continue
		}
		if expired(r, now) {
			_ = os.RemoveAll(requestDir(r.ID))
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// expired decides whether a request's directory can go.
//
// The two clauses are deliberately different, and conflating them was a bug worth
// naming: an ANSWERED request has an on-disk value the agent is about to read, and
// the process that raised it has already exited. Reaping on "requester pid is
// gone" would therefore delete every delivered value the instant it was delivered.
// So the pid check only applies while the request is still waiting.
func expired(r Request, now time.Time) bool {
	if hasValue(r.ID) {
		return now.After(r.ValueExpires)
	}
	if now.After(r.Expires) {
		return true
	}
	// Still within its window, but nobody is waiting for it any more: the
	// requesting process died. Prompting a human for an answer that will be
	// delivered to a corpse is pure noise.
	return r.Requester.PID > 0 && !room.PidAlive(r.Requester.PID)
}

// stale is the fallback age check for a directory whose metadata we cannot read.
// A grace period avoids racing a request that is mid-creation right now.
func stale(dir string, now time.Time) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) > time.Minute
}

func hasValue(id string) bool {
	_, err := os.Stat(filepath.Join(requestDir(id), valueFile))
	return err == nil
}

// answered reports whether this request has already been satisfied.
func answered(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, answeredFile))
	return err == nil
}

// markAnswered latches the request closed. Best-effort by design: failing to
// create the marker must not discard a value the human has already typed, and the
// transport-level guards still refuse a second delivery.
func markAnswered(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, answeredFile),
		os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollow, 0o600)
	if err == nil {
		_ = f.Close()
	}
}

// Find resolves an id or a unique prefix, so an operator can type the six
// characters the instruction line showed them rather than all thirty-two. Same
// affordance as room.Find, and the ambiguity case is an error rather than a guess —
// a control verb must never pick one for you.
func Find(id string) (Request, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Request{}, fmt.Errorf("ask: name a request id — `bashy ask ls`")
	}
	if r, err := Load(id); err == nil {
		return r, nil
	}
	all, err := List()
	if err != nil {
		return Request{}, err
	}
	var hits []Request
	for _, r := range all {
		if strings.HasPrefix(r.ID, id) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Request{}, fmt.Errorf("ask: no pending request matching %q (%d pending) — `bashy ask ls`", id, len(all))
	default:
		return Request{}, fmt.Errorf("ask: %q matches %d pending requests — use more characters", id, len(hits))
	}
}

// Cancel removes a request and anything delivered under it.
func Cancel(id string) error {
	r, err := Find(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(requestDir(r.ID))
}
