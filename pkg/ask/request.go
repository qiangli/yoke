package ask

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// SchemaVersion tags every record and every wire line this package emits, so a
// harness can recognise the protocol before it recognises the payload.
const SchemaVersion = "bashy-ask-v1"

// Request is the metadata of one pending ask.
//
// THE INVARIANT: a Request never holds the value, and this struct is the whole
// of what gets written to disk on the rendezvous path. It is the answer to
// "what is being asked, by whom, and where will the answer go" — a question the
// human must be able to answer before typing, and one that must be safe to leave
// on disk, safe to show in `ask ls`, and safe to publish as a notification.
type Request struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`

	// Prompt is the message the human sees. When an agent supplied it this text
	// is UNTRUSTED — it is sanitized on the way in (see sanitize.go) and always
	// rendered inside the frame, never as the frame.
	Prompt string `json:"prompt"`
	// Name labels the value. Constrained charset, so it is safe to render inside
	// the frame's chrome without escaping.
	Name string `json:"name"`
	// Secret disables echo when the value is typed.
	Secret bool `json:"secret"`

	Created time.Time `json:"created"`
	// Expires bounds how long the request waits for an answer.
	Expires time.Time `json:"expires"`
	// ValueExpires bounds how long a DELIVERED value survives on disk. Distinct
	// from Expires: waiting for a human is a couple of minutes, while the value
	// the agent then works with lives for the length of a task.
	ValueExpires time.Time `json:"value_expires"`

	Sink      Sink      `json:"sink"`
	Requester Requester `json:"requester"`
}

// Sink says where the answer goes. It is recorded on the request specifically so
// the human can see it BEFORE they type: a request that would print the value
// back to the agent, or pipe it into a command, has to say so up front. That is
// what turns an invisible exfiltration into a visible one.
type Sink struct {
	Kind   string `json:"kind"`   // file | out | stdout
	Detail string `json:"detail"` // the concrete destination, shown to the human
}

// Requester is the provenance shown in the frame. Every field is observed by
// bashy about itself and its environment — none of it is supplied by the caller,
// which is what makes it trustworthy when the caller is not.
type Requester struct {
	PID       int      `json:"pid"`
	PPID      int      `json:"ppid"`
	Principal string   `json:"principal"`
	Cwd       string   `json:"cwd"`
	Argv      []string `json:"argv"`
	Tool      string   `json:"tool"`
}

// currentRequester captures who we are and what we were told to do.
//
// Argv is recorded deliberately, and it is safe BECAUSE this command has no
// value-bearing argument: there is no positional VALUE and no --shared-secret, so
// the command line contains only the request's shape. That is the same reason
// argv can be shown to the human — it discloses the sink without disclosing the
// secret.
func currentRequester() Requester {
	cwd, _ := os.Getwd()
	tool, _ := fleet.DetectTool()
	return Requester{
		PID:       os.Getpid(),
		PPID:      os.Getppid(),
		Principal: principalName(),
		Cwd:       cwd,
		Argv:      append([]string(nil), os.Args...),
		Tool:      tool,
	}
}

// principalName names the human this session is attributed to. Best-effort and
// never fails — an unattributed request is still a request, and refusing to ask
// because we cannot name the asker would help nobody.
func principalName() string {
	for _, k := range []string{"BASHY_PRINCIPAL", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "unknown"
}

// newID mints a request id.
//
// 128 bits from crypto/rand, not a pid and not a timestamp. The id names a
// directory another local user can attempt to pre-create, and it is what an
// answering process quotes; a guessable id turns both of those into an attack
// rather than a nuisance.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Pending reports whether the request is still waiting for an answer.
func (r Request) Pending(now time.Time) bool { return now.Before(r.Expires) }

// Describe renders the sink for a human. Kept next to the struct so the wording
// the frame shows and the wording `ask ls` shows can never drift apart — they are
// the same sentence, and the human learns to recognise it.
func (s Sink) Describe() string {
	switch s.Kind {
	case SinkStdout:
		return "PRINTED BACK to the requesting program (it will be in that program's transcript)"
	case SinkOut:
		return "written to the file " + s.Detail + " (mode 0600)"
	default:
		return "written to " + s.Detail + " (mode 0600, private to you)"
	}
}
