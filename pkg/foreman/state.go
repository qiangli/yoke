package foreman

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusBlocked = "blocked"
	StatusDone    = "done"
)

const (
	CommandTell   = "tell"
	CommandPause  = "pause"
	CommandResume = "resume"
	CommandSkip   = "skip"
	CommandPrio   = "prio"
	CommandStop   = "stop"

	// CommandKey presses a KEY at the running agent (esc / enter / ctrl-c) instead
	// of saying something to it. The one control that reaches an agent stuck in a
	// tool loop, whose turn is never going to end and which will therefore never
	// read a queued message.
	CommandKey = "key"
)

type State struct {
	ID          string    `json:"id"`
	Goal        string    `json:"goal"`
	Status      string    `json:"status"`
	CurrentStep string    `json:"current_step,omitempty"`
	DriveLease  string    `json:"drive_lease,omitempty"`
	CtlSock     string    `json:"ctl_sock,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	Role        string    `json:"role,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Stopped     bool      `json:"stopped,omitempty"`
	Paused      bool      `json:"paused,omitempty"`
	MaxRuntime  string    `json:"max_runtime,omitempty"`
	Deadline    time.Time `json:"deadline,omitempty"`
	StopReason  string    `json:"stop_reason,omitempty"`
	// OpeningSendOnce is persisted because a detached start is reopened by the
	// serve subprocess before it launches the agent.
	OpeningSendOnce bool `json:"opening_send_once,omitempty"`
	// AllowUnsafe carries an explicit --yolo decision across detached startup.
	// Without persistence, the parent accepts the mode and the serve child loses
	// it just before chat applies the unattended-host launch guard.
	AllowUnsafe bool `json:"allow_unsafe,omitempty"`

	// Binding is the canonical tool:model this session is actually talking to.
	// Agent may be an alias or a nickname; a record must never store one of those.
	Binding string `json:"binding,omitempty"`

	// Steering says whether `tell` reaches a LIVE agent (a keystroke into an open
	// session) or merely queues a message for the next fresh spawn.
	//
	// The two look identical from outside — the operator types tell, the status
	// goes to working, an answer comes back — and they are not remotely the same
	// thing. So the state says which one happened, and SteerWhyNot says why when it
	// is the lesser one. An operator who thinks they interrupted an agent, and did
	// not, has been lied to by silence.
	Steering    bool   `json:"steering"`
	SteerWhyNot string `json:"steer_why_not,omitempty"`

	// Blocker is WHY the session is blocked, when it is: a paused operator, a
	// runner that exited non-zero, a live agent that could not be reached, a
	// runtime that expired. It is cleared by any transition out of blocked. The
	// checkpoint renders it, and `status --wait` carries it, so a supervisor never
	// has to open the log to learn what a `blocked` row means.
	Blocker string `json:"blocker,omitempty"`

	// Seq and Digest are the state-change contract (see Store.Commit).
	//
	// Seq is a per-session monotonic sequence that advances ONLY when the
	// canonical content of the state changes; Digest is the canonical digest of
	// that content. A poller that remembers the seq it last saw can ask for
	// "everything after N" and get exactly the transitions it missed and nothing
	// when nothing happened — which is the difference between supervision and a
	// heartbeat the model has to read.
	Seq    int64  `json:"seq,omitempty"`
	Digest string `json:"digest,omitempty"`
}

type Command struct {
	Seq      int64     `json:"seq,omitempty"`
	Verb     string    `json:"verb"`
	Message  string    `json:"message,omitempty"`
	Target   string    `json:"target,omitempty"`
	Priority string    `json:"priority,omitempty"`
	At       time.Time `json:"at"`
}

type Store struct {
	Root string
	ID   string
}

func DefaultRoot() string {
	if v := strings.TrimSpace(os.Getenv("BASHY_FOREMAN_DIR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("BASHY_HOME")); v != "" {
		return filepath.Join(v, "foreman")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "bashy", "foreman")
	}
	return filepath.Join(home, ".bashy", "foreman")
}

func NewStore(root, id string) Store {
	if root == "" {
		root = DefaultRoot()
	}
	return Store{Root: root, ID: id}
}

func (s Store) Dir() string {
	return filepath.Join(s.Root, s.ID)
}

func (s Store) StatePath() string {
	return filepath.Join(s.Dir(), "state.json")
}

func (s Store) CommandsPath() string {
	return filepath.Join(s.Dir(), "commands")
}

func (s Store) CtlSockPath() string {
	p := filepath.Join(s.Dir(), "ctl.sock")
	if len(p) <= 100 {
		return p
	}
	return filepath.Join(os.TempDir(), "bashy-foreman-"+s.ID+".sock")
}

// LogPath is where the live agent's output is TEE'd.
//
// A detached foreman held its agent's output in the daemon's memory and nowhere
// else, so an operator supervising a run could see that it was `working` and not
// one word of what it was doing. You cannot steer what you cannot see: the whole
// value of a mid-turn correction is that you noticed the agent going wrong, and
// noticing requires watching.
func (s Store) LogPath() string {
	return filepath.Join(s.Dir(), "log")
}

func (s Store) Ensure() error {
	return os.MkdirAll(s.Dir(), 0o700)
}

func (s Store) LoadState() (State, error) {
	data, err := os.ReadFile(s.StatePath())
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// SaveState persists st, advancing the session sequence if — and only if — its
// canonical content changed. See Commit for the contract; SaveState is the
// fire-and-forget form for callers that do not need the committed record back.
func (s Store) SaveState(st State) error {
	_, err := s.Commit(st)
	return err
}

// Commit is the ONE write path for state.json, and it is where the state-change
// contract lives.
//
// It compares the canonical digest of st (everything except the volatile
// UpdatedAt/Seq/Digest fields) with what is already on disk:
//
//   - identical: nothing is written, no transition is journaled, and the
//     on-disk seq/digest are returned. A healthy session that persists the same
//     state every tick therefore produces NO observable change — no mtime bump,
//     no journal line, no payload for a waiting supervisor.
//   - different: seq advances by exactly one, state.json is replaced
//     atomically, and one Transition is appended to the journal. The journal is
//     a delta view over state.json (the truth), never a second truth: a reader
//     that finds state.json ahead of the journal synthesizes the missing head
//     record (see Changes), so a crash between the two writes loses nothing.
//
// The next seq is max(on-disk seq, journal tail seq)+1, so a session restarted
// from disk continues the sequence it had, and a reader's cursor stays valid
// across restarts. A legacy state.json without a seq counts as seq 1 — the same
// number Changes synthesizes for it — so a cursor taken before this contract
// existed never collides with the first real transition.
func (s Store) Commit(st State) (State, error) {
	if err := s.Ensure(); err != nil {
		return st, err
	}
	digest := CanonicalDigest(st)
	prev, prevErr := s.LoadState()
	if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
		return st, prevErr
	}
	havePrev := prevErr == nil
	last, err := s.lastTransitionSeq()
	if err != nil {
		return st, err
	}
	if havePrev {
		prevSeq := prev.Seq
		if prevSeq == 0 {
			prevSeq = 1 // legacy record: Changes synthesizes it as seq 1
		}
		// Repair the crash window where state.json landed but its delta did not.
		// Do this even for an identical state: otherwise the next distinct commit
		// can advance past the missing sequence and make it unrecoverable.
		if last < prevSeq {
			prev.Seq = prevSeq
			if prev.Digest == "" {
				prev.Digest = CanonicalDigest(prev)
			}
			if err := s.appendTransition(transitionOf(prev)); err != nil {
				return st, err
			}
			last = prevSeq
		}
		if prev.Digest == digest && prev.Seq > 0 {
			st.Seq, st.Digest, st.UpdatedAt = prev.Seq, prev.Digest, prev.UpdatedAt
			return st, nil
		}
	}
	st.Seq = last + 1
	st.Digest = digest
	st.UpdatedAt = time.Now().UTC()
	if err := s.writeState(st); err != nil {
		return st, err
	}
	tr := transitionOf(st)
	if havePrev {
		tr.PreviousStatus = prev.Status
	}
	if err := s.appendTransition(tr); err != nil {
		return st, err
	}
	return st, nil
}

func (s Store) writeState(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir(), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.StatePath())
}

func (s Store) AppendCommand(cmd Command) error {
	if strings.TrimSpace(cmd.Verb) == "" {
		return errors.New("foreman: command verb required")
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	cmd.At = time.Now().UTC()
	f, err := os.OpenFile(s.CommandsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(cmd)
}

func (s Store) LoadCommands() ([]Command, error) {
	f, err := os.Open(s.CommandsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Command
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("foreman: read command: %w", err)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

func (s Store) TruncateCommands() error {
	if err := s.Ensure(); err != nil {
		return err
	}
	return os.WriteFile(s.CommandsPath(), nil, 0o600)
}

func List(root string) ([]State, error) {
	if root == "" {
		root = DefaultRoot()
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := NewStore(root, e.Name()).LoadState()
		if err == nil {
			out = append(out, st)
		}
	}
	return out, nil
}
