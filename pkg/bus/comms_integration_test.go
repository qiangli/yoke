package bus_test

// End-to-end tests for the agent communication surface delivered in sprint 80.
//
// These are INTEGRATION tests: they drive the real cobra commands the way an
// agent does, against a temporary board, and assert on what a caller actually
// observes — rendered output, exit behaviour, and the durable store. Unit tests
// already cover the pieces; what was missing was proof that the pieces compose.
//
// Every assertion here corresponds to a claim in the sprint's acceptance
// criteria, because a claim nobody can falsify by running something is not a
// claim, it is a hope.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/spf13/cobra"
)

// board points every store at a temp dir so a test can never read, advance, or
// pollute the operator's real board.
func board(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_MB_DIR", dir)
	// The board reads subscriptions too (declared concerns route posts), so
	// the room store must be a temp dir or a test would read the host's.
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("USER", "tester")
	// The send path's resolver fallback reads the fleet catalog and the
	// observation stores; keep those hermetic too.
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_AGENTS_DIR", "")
	t.Setenv("BASHY_PEOPLE_DIR", "")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_PEOPLE_PATH", "")
	return dir
}

func run(t *testing.T, cmd *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// seed gives a reader a cursor, which is what distinguishes `queued` (someone
// demonstrably polls) from `unverified` (nobody has ever looked).
//
// A cursor is only written when there is something to mark seen, so an empty
// board cannot seed one — the reader must have actually read a post. That is
// the same reason `unverified` exists: the store records reading, not intent.
func seed(t *testing.T, who string) {
	t.Helper()
	if err := bus.PostMessage(bus.Post{From: "seed", Body: "seed post"}); err != nil {
		t.Fatalf("seeding a post: %v", err)
	}
	if _, _, err := run(t, bus.NewMessageBoardCmd(), "--as", who); err != nil {
		t.Fatalf("seeding cursor for %s: %v", who, err)
	}
	if bus.SeenSeq(who) == 0 {
		t.Fatalf("seed did not establish a cursor for %s", who)
	}
}

func TestSendToAReaderWithACursorReportsQueued(t *testing.T) {
	board(t)
	seed(t, "reader-a")
	_, errOut, err := run(t, bus.NewMessageBoardCmd(), "send", "--as", "sender", "reader-a", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(errOut, "queued") {
		t.Fatalf("a reader that polls should be reported queued, got: %q", errOut)
	}
}

func TestSendAcceptsExplicitTo(t *testing.T) {
	board(t)
	seed(t, "reader-a")
	_, _, err := run(t, bus.NewMessageBoardCmd(), "send", "--as", "sender", "--to", "reader-a", "hello")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, bus.NewMessageBoardCmd(), "--as", "reader-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("explicit --to send was not readable by addressee:\n%s", out)
	}
}

func TestPingAcceptsExplicitTo(t *testing.T) {
	board(t)
	seed(t, "reader-a")
	_, _, err := run(t, bus.NewPingCmd(), "--as", "sender", "--to", "reader-a", "hello")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, bus.NewMessageBoardCmd(), "--as", "reader-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("ping --to send was not readable by addressee:\n%s", out)
	}
}

// The state that earns six over two: a reader with NO cursor is not merely
// behind. Nothing is known about whether it will ever read, and reporting
// `queued` there would claim more than the evidence supports.
func TestSendToAReaderThatHasNeverReadReportsUnverified(t *testing.T) {
	board(t)
	bus.FleetNames = func() []string { return []string{"never-read"} }
	t.Cleanup(func() { bus.FleetNames = nil })

	_, errOut, err := run(t, bus.NewMessageBoardCmd(), "send", "--as", "sender", "never-read", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(strings.ToLower(errOut), "unverified") {
		t.Fatalf("a reader with no cursor should be reported unverified, got: %q", errOut)
	}
}

// THE DEFECT THIS SPRINT FIXED. `ping profile-b "..."` was accepted, written to
// the board with a literal to:, and reported as though it had been delivered.
// Nothing could ever read it. The send must fail and write NOTHING.
func TestUnresolvableTargetFailsAndWritesNothing(t *testing.T) {
	dir := board(t)
	seed(t, "someone") // give the store a file to grow, so "unchanged" is meaningful
	before := postCount(t, dir)

	_, _, err := run(t, bus.NewMessageBoardCmd(), "send", "--as", "sender", "no-such-target-zz9", "hello")
	if err == nil {
		t.Fatal("sending to an unresolvable target must fail, not succeed quietly")
	}
	if got := postCount(t, dir); got != before {
		t.Fatalf("a failed send wrote to the board: %d posts before, %d after", before, got)
	}
}

func postCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "posts.jsonl"))
	if err != nil {
		return 0
	}
	return strings.Count(strings.TrimSpace(string(b)), "\n")
}

// The round trip the whole per-turn polling design rests on: a reader blocks on
// a message that DOES NOT YET EXIST, and is woken by it.
func TestBoundedWaitIsWokenByAPostThatArrivesDuringTheWait(t *testing.T) {
	board(t)
	seed(t, "waiter")

	posted := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		posted <- bus.PostMessage(bus.Post{From: "sender", To: "waiter", Body: "ARRIVED-DURING-WAIT"})
	}()

	start := time.Now()
	out, _, err := run(t, bus.NewMessageBoardCmd(), "--as", "waiter", "--wait", "10s")
	if err != nil {
		t.Fatalf("waited read: %v", err)
	}
	if e := <-posted; e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out, "ARRIVED-DURING-WAIT") {
		t.Fatalf("the wait returned without the message that woke it:\n%s", out)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("returned before the message could have been posted — it did not actually wait")
	}
}

// A quiet turn must not look like a failure. An agent polls every turn; if a
// timeout were an error, every idle turn would report one.
func TestTimeoutIsAnEmptySuccessfulRead(t *testing.T) {
	board(t)
	seed(t, "quiet")

	out, errOut, err := run(t, bus.NewMessageBoardCmd(), "--as", "quiet", "--wait", "60ms")
	if err != nil {
		t.Fatalf("a timeout must be a SUCCESSFUL read, got error: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a timeout must print no messages, got: %q", out)
	}
	if !strings.Contains(errOut, "nothing new") {
		t.Fatalf("a timeout must say so on stderr, got: %q", errOut)
	}
}

// Directed mail carries an obligation, so it is never trimmed away by a cap.
// A reader that hits -n must still see everything addressed to it by name.
func TestDirectedPostsSurviveTheLimitThatTrimsBroadcasts(t *testing.T) {
	board(t)
	seed(t, "target")
	for i := 0; i < 6; i++ {
		if err := bus.PostMessage(bus.Post{From: "sender", Body: "broadcast filler"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bus.PostMessage(bus.Post{From: "sender", To: "target", Body: "DIRECTED-MUST-SURVIVE"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, bus.NewMessageBoardCmd(), "--as", "target", "-n", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "DIRECTED-MUST-SURVIVE") {
		t.Fatalf("-n 1 trimmed a DIRECTED post; directed mail is never capped:\n%s", out)
	}
}

// --peek is how an agent looks without claiming. If it advanced the cursor, a
// second reader would never be shown what the first one merely glanced at.
func TestPeekDoesNotAdvanceTheCursor(t *testing.T) {
	board(t)
	seed(t, "peeker")
	if err := bus.PostMessage(bus.Post{From: "sender", To: "peeker", Body: "STILL-UNREAD"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, bus.NewMessageBoardCmd(), "--as", "peeker", "--peek"); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, bus.NewMessageBoardCmd(), "--as", "peeker", "--peek")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "STILL-UNREAD") {
		t.Fatalf("--peek advanced the cursor; the second peek lost the message:\n%s", out)
	}
}
