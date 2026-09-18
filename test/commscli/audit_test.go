package commscli

// One test per S80 acceptance claim, each driven through the built audit
// binary (see main_test.go). Every assertion is on what a CALLER observes:
// stdout, stderr, the exit code, and the durable stores.

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// auditbotYAML declares a catalog agent (and the tool it binds) in the temp
// fleet store, mirroring the baseline entry shape. This is the "known agent
// that has never read" of the delivery-state model, and the catalog name
// whois must answer with source=fleet.
const auditbotAgentYAML = `agents:
  - name: auditbot
    tool: stub-ok
    model: stub-model
    display: "auditbot"
`

const stubOkToolYAML = `name: stub-ok
kind: cli
cli:
  binary: stub-ok
  launch:
    exec: stub-ok -p {prompt}
`

func plantAuditbot(t *testing.T, w *world) {
	t.Helper()
	w.plantFleetEntry(t, "agents", "auditbot", auditbotAgentYAML)
	w.plantFleetEntry(t, "tools", "stub-ok", stubOkToolYAML)
}

// --- whois -----------------------------------------------------------------

func TestWhoisResolvesAnObservedNameWithSourceObserved(t *testing.T) {
	w := newWorld(t)
	w.plantBoardCursor(t, "zz-observed-worker")

	res := w.run(t, nil, "whois", "zz-observed-worker")
	if res.code != 0 {
		t.Fatalf("whois on an observed name must resolve, got exit %d\nstderr: %s", res.code, res.err)
	}
	if strings.Contains(res.err, "names nothing") || strings.Contains(res.out, "names nothing") {
		t.Fatalf("an observed name answered 'names nothing' — the S80 observation rung is not reaching the CLI:\n%s%s", res.out, res.err)
	}
	if !strings.Contains(res.out, "observed") {
		t.Fatalf("an observed-only name must carry source=observed, got:\n%s", res.out)
	}
}

func TestWhoisResolvesACatalogNameWithSourceFleet(t *testing.T) {
	w := newWorld(t)
	plantAuditbot(t, w)

	res := w.run(t, nil, "whois", "auditbot")
	if res.code != 0 {
		t.Fatalf("whois on a catalog agent must resolve, got exit %d\nstderr: %s", res.code, res.err)
	}
	if !strings.Contains(res.out, "fleet") {
		t.Fatalf("a declared catalog name must carry source=fleet, got:\n%s", res.out)
	}
	if strings.Contains(res.out, "observed") {
		t.Fatalf("a DECLARED name must not be downgraded to an observation:\n%s", res.out)
	}
}

func TestWhoisUnknownNameFailsClearly(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, nil, "whois", "zz-total-stranger")
	if res.code == 0 {
		t.Fatalf("a name with no catalog entry and no trace must not resolve:\n%s", res.out)
	}
	if !strings.Contains(res.err, "names nothing") {
		t.Fatalf("the refusal must say the name names nothing, got: %s", res.err)
	}
}

// --- agent whoami ----------------------------------------------------------

func TestAgentWhoamiReportsTheLauncherStampedIdentity(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, map[string]string{"BASHY_AGENT_ID": "auditbot"}, "agent", "whoami")
	if res.code != 0 {
		t.Fatalf("whoami with a launcher-stamped identity must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	if strings.TrimSpace(res.out) != "auditbot" {
		t.Fatalf("whoami must print the stamped agent identity, got: %q", res.out)
	}
}

func TestAgentWhoamiRefusesWithoutALauncherIdentity(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, nil, "agent", "whoami")
	if res.code == 0 {
		t.Fatalf("whoami with no launcher-stamped identity must refuse, printed: %q", res.out)
	}
	if !strings.Contains(res.err, "agent identity unavailable") {
		t.Fatalf("the refusal must be the clear one, got: %s", res.err)
	}
	if strings.TrimSpace(res.out) != "" {
		t.Fatalf("a refusal must print no identity at all, got: %q", res.out)
	}
}

func TestAgentWhoamiNeverAnswersWithTheToolName(t *testing.T) {
	// A harness marker (here: claude's, from the compiled-in catalog) proves a
	// TOOL is running, not an agent. Answering with the tool name would be the
	// attribution lie the board rejects — whoami must refuse and say why.
	w := newWorld(t)
	res := w.run(t, map[string]string{"CLAUDECODE": "1"}, "agent", "whoami")
	if res.code == 0 {
		t.Fatalf("whoami under a bare harness must refuse, printed: %q", res.out)
	}
	if strings.TrimSpace(res.out) != "" {
		t.Fatalf("whoami under a bare harness printed an identity: %q", res.out)
	}
	if !strings.Contains(res.err, "running under tool") {
		t.Fatalf("the refusal must name the tool as a tool, got: %s", res.err)
	}
}

// --- mb send: the delivery states ------------------------------------------

func TestMBSendToAnUnresolvableTargetFailsAndWritesNothing(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, nil, "mb", "send", "--as", "sender", "zz-no-such-target", "hello")
	if res.code == 0 {
		t.Fatalf("sending to an unresolvable target must exit non-zero:\nstdout: %s\nstderr: %s", res.out, res.err)
	}
	if !strings.Contains(res.err, "failed") {
		t.Fatalf("the receipt must say failed, got: %s", res.err)
	}
	if n := w.boardPostCount(t); n != 0 {
		t.Fatalf("a failed send wrote %d post(s) to the board; it must write NOTHING", n)
	}
}

func TestMBSendToAKnownReaderReportsQueued(t *testing.T) {
	w := newWorld(t)
	// Seed the reader's cursor through the CLI itself: a broadcast lands, the
	// reader reads it. That is what makes it a KNOWN reader — evidence it polls.
	if res := w.run(t, nil, "mb", "post", "--as", "seeder", "seed post"); res.code != 0 {
		t.Fatalf("seeding broadcast failed: %s", res.err)
	}
	if res := w.run(t, nil, "mb", "--as", "reader-a"); res.code != 0 {
		t.Fatalf("seeding read failed: %s", res.err)
	}

	res := w.run(t, nil, "mb", "send", "--as", "sender", "reader-a", "hello reader")
	if res.code != 0 {
		t.Fatalf("send to a known reader must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	if !strings.Contains(res.err, "queued") {
		t.Fatalf("a reader with a cursor must be reported queued, got: %s", res.err)
	}
}

func TestMBSendToACatalogAgentWithNoCursorReportsUnverified(t *testing.T) {
	w := newWorld(t)
	plantAuditbot(t, w)

	res := w.run(t, nil, "mb", "send", "--as", "sender", "auditbot", "hello agent")
	if res.code != 0 {
		t.Fatalf("send to a catalog agent must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	if !strings.Contains(strings.ToLower(res.err), "unverified") {
		t.Fatalf("an agent that has never read must be reported unverified, got: %s", res.err)
	}
}

// --- mb --wait: the bounded-wait contract -----------------------------------

func TestMBWaitIsWokenByAPostThatDidNotExistWhenTheWaitBegan(t *testing.T) {
	w := newWorld(t)
	// Seed the waiter's cursor through the CLI (post + read), so the cursor
	// sits at the board's REAL high-water mark. A hand-planted cursor value
	// would race the sequence numbering and could mask the wakeup.
	if res := w.run(t, nil, "mb", "post", "--as", "seeder", "seed post"); res.code != 0 {
		t.Fatalf("seeding broadcast failed: %s", res.err)
	}
	if res := w.run(t, nil, "mb", "--as", "waiter"); res.code != 0 {
		t.Fatalf("seeding read failed: %s", res.err)
	}

	waiter := w.start(t, nil, "mb", "--as", "waiter", "--wait", "30s")
	time.Sleep(400 * time.Millisecond) // let it reach its poll loop; the message does not exist yet
	if res := w.run(t, nil, "mb", "send", "--as", "sender", "waiter", "ARRIVED-DURING-WAIT"); res.code != 0 {
		t.Fatalf("wakeup send failed: %s", res.err)
	}

	res := waiter.wait(t, 25*time.Second)
	if res.code != 0 {
		t.Fatalf("a woken wait must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	if !strings.Contains(res.out, "ARRIVED-DURING-WAIT") {
		t.Fatalf("the wait returned without the message that woke it:\nstdout: %s\nstderr: %s", res.out, res.err)
	}
	if res.elapsed < 300*time.Millisecond {
		t.Fatalf("the waiter returned in %v — before the wakeup message existed, so it never actually waited", res.elapsed)
	}
	if res.elapsed > 20*time.Second {
		t.Fatalf("the waiter ran %v — it hit its bound instead of being woken", res.elapsed)
	}
}

func TestMBWaitTimeoutIsAnEmptySuccessfulReadWithAStderrNote(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, nil, "mb", "--as", "quiet", "--wait", "300ms")
	if res.code != 0 {
		t.Fatalf("a timeout must be a SUCCESSFUL read (agents poll every turn), got exit %d: %s", res.code, res.err)
	}
	if strings.TrimSpace(res.out) != "" {
		t.Fatalf("a timeout must print NO messages on stdout, got: %q", res.out)
	}
	if !strings.Contains(res.err, "nothing new") {
		t.Fatalf("a timeout must say so on stderr, got: %q", res.err)
	}
	if res.elapsed < 250*time.Millisecond {
		t.Fatalf("returned in %v — it did not honor the wait bound", res.elapsed)
	}
}

// --- bus watch --drain --wait: same contract on the bus ---------------------

func TestBusWatchDrainWaitIsWokenByAPublish(t *testing.T) {
	w := newWorld(t)
	watcher := w.start(t, nil, "bus", "watch", "--drain", "--wait", "30s", "--to", "audit-waiter", "--as", "audit-waiter")
	time.Sleep(400 * time.Millisecond)
	if res := w.run(t, nil, "bus", "publish", "--as", "sender", "--to", "audit-waiter", "BUS-ARRIVED-DURING-WAIT"); res.code != 0 {
		t.Fatalf("publish failed: %s", res.err)
	}

	res := watcher.wait(t, 25*time.Second)
	if res.code != 0 {
		t.Fatalf("a woken drain must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	if !strings.Contains(res.out, "BUS-ARRIVED-DURING-WAIT") {
		t.Fatalf("the drain returned without the notification that woke it:\nstdout: %s\nstderr: %s", res.out, res.err)
	}
	if res.elapsed < 300*time.Millisecond {
		t.Fatalf("the watcher returned in %v — before the notification existed", res.elapsed)
	}
}

func TestBusWatchDrainWaitTimeoutIsAnEmptySuccessfulRead(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, nil, "bus", "watch", "--drain", "--wait", "300ms", "--to", "nobody-here", "--as", "nobody-here")
	if res.code != 0 {
		t.Fatalf("a quiet bus must not be a failed turn, got exit %d: %s", res.code, res.err)
	}
	if strings.TrimSpace(res.out) != "" {
		t.Fatalf("a timeout must print no notifications, got: %q", res.out)
	}
	if !strings.Contains(res.err, "nothing new") {
		t.Fatalf("a timeout must note the quiet on stderr, got: %q", res.err)
	}
}

// --- weave fleet ------------------------------------------------------------

// gitWorld prepares a world whose work dir is a git repo and whose PATH also
// carries the host git — the one system tool this corner of the surface
// shells out to (weaveRepoRoot). Skips when the host has none.
func gitWorld(t *testing.T) (*world, map[string]string) {
	t.Helper()
	w := newWorld(t)
	vars := map[string]string{}
	if !w.withGitOnPath(t, vars) {
		t.Skip("weave fleet resolves the repo root via the host git; none on this host")
	}
	cmd := exec.Command("git", "-c", "init.defaultBranch=main", "init", "-q", w.work)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return w, vars
}

func TestWeaveFleetReportsPathEvidenceAsInstalled(t *testing.T) {
	w, vars := gitWorld(t)
	w.plantStub(t, "stub-ok")

	res := w.run(t, vars, "weave", "fleet", "--fleet", "stub-ok")
	if res.code != 0 {
		t.Fatalf("weave fleet must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	// The status is the word right after the row's label. It must be
	// "installed" — PATH evidence — and never "available", which would claim
	// usability nothing has verified. (Asserted on the status field, not the
	// whole output: the row also prints the stub's temp path.)
	status := ""
	for l := range strings.SplitSeq(res.out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "stub-ok" {
			status = f[1]
		}
	}
	if status != "installed" {
		t.Fatalf("a binary on PATH that has not been probed is 'installed', got status %q in:\n%s", status, res.out)
	}
}

func TestWeaveFleetReportsAMissingBinaryAsNotFound(t *testing.T) {
	w, vars := gitWorld(t)
	res := w.run(t, vars, "weave", "fleet", "--fleet", "zz-absent-tool")
	if res.code != 0 {
		t.Fatalf("weave fleet must succeed even when the roster is missing, got exit %d\nstderr: %s", res.code, res.err)
	}
	if !strings.Contains(res.out, "NOT FOUND") {
		t.Fatalf("a binary that does not resolve must be NOT FOUND, got:\n%s", res.out)
	}
}

func TestWeaveFleetProbeGatesOnOutputNotExitStatus(t *testing.T) {
	// Both stubs exit 0. Only stub-ok prints the smoke token. If --probe
	// trusted exit status, stub-mute would read as usable — the exact lie
	// S80's probe change exists to remove.
	w, vars := gitWorld(t)
	w.plantStub(t, "stub-ok")
	w.plantStub(t, "stub-mute")
	w.plantFleetEntry(t, "tools", "stub-ok", stubOkToolYAML)
	w.plantFleetEntry(t, "tools", "stub-mute", `name: stub-mute
kind: cli
cli:
  binary: stub-mute
  launch:
    exec: stub-mute -p {prompt}
`)

	res := w.run(t, vars, "weave", "fleet", "--fleet", "stub-ok,stub-mute", "--probe")
	if res.code != 0 {
		t.Fatalf("weave fleet --probe must succeed, got exit %d\nstderr: %s", res.code, res.err)
	}
	var okLine, muteLine string
	for l := range strings.SplitSeq(res.out, "\n") {
		if strings.HasPrefix(l, "stub-ok") {
			okLine = l
		}
		if strings.HasPrefix(l, "stub-mute") {
			muteLine = l
		}
	}
	if !strings.Contains(okLine, "USABLE") || strings.Contains(okLine, "UNUSABLE") {
		t.Fatalf("the stub that printed the smoke token must be USABLE, got: %q\nfull output:\n%s", okLine, res.out)
	}
	if !strings.Contains(muteLine, "UNUSABLE") {
		t.Fatalf("a zero-exit probe with no smoke token must be UNUSABLE — exit status is not evidence, got: %q\nfull output:\n%s", muteLine, res.out)
	}
}

// --- inbox / notify ---------------------------------------------------------

// The verbs this whole audit exists because of: present on the audit binary,
// and a notification round-trips from notify to the addressee's inbox. (Their
// remaining gap — no atlas entry, unmounted on the shipped bashy — is pinned
// in atlas_test.go and checked against a real bashy by
// scripts/comms-cli-audit.sh.)
func TestNotifyToInboxRoundTrip(t *testing.T) {
	w := newWorld(t)
	plantAuditbot(t, w)

	send := w.run(t, nil, "notify", "--as", "audit-sender", "auditbot", "PING-SUBJECT-7")
	if send.code != 0 {
		t.Fatalf("notify to a catalog agent must be accepted, got exit %d\nstderr: %s", send.code, send.err)
	}

	read := w.run(t, nil, "inbox", "--as", "auditbot")
	if read.code != 0 {
		t.Fatalf("inbox must read, got exit %d\nstderr: %s", read.code, read.err)
	}
	if !strings.Contains(read.out, "PING-SUBJECT-7") {
		t.Fatalf("the notification did not round-trip to the addressee's inbox:\nstdout: %s\nstderr: %s", read.out, read.err)
	}

	// Reading advanced the cursor: a second read is quiet.
	again := w.run(t, nil, "inbox", "--as", "auditbot")
	if again.code != 0 || strings.TrimSpace(again.out) != "" {
		t.Fatalf("a drained inbox must read empty and succeed, got exit %d stdout %q", again.code, again.out)
	}
	if !strings.Contains(again.err, "nothing new") {
		t.Fatalf("a quiet inbox must say so on stderr, got: %q", again.err)
	}
}

func TestNotifyToAnUnresolvableTargetFails(t *testing.T) {
	w := newWorld(t)
	res := w.run(t, nil, "notify", "--as", "audit-sender", "zz-nobody-at-all", "subject")
	if res.code == 0 {
		t.Fatalf("notify to a target nothing can read must fail:\nstdout: %s\nstderr: %s", res.out, res.err)
	}
}
