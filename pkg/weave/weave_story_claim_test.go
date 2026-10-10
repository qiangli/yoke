package weave

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/policy/coord"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func storyClaimHolder(t *testing.T, sprint int64, storyID string) (string, bool) {
	t.Helper()
	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := storyClaimRef(sprint, storyID)
	for _, c := range claims {
		if c.Kind == ref.Kind && c.Resource == ref.Name {
			return c.Holder.Name, true
		}
	}
	return "", false
}

// TestStoryClaimIsInTheLedgerAndOnlyTheHolderReleasesIt: claiming a story
// records an announce-mode claim beside the todo Assignee, a stranger can
// neither yield nor submit it, and the holder's yield drops the claim.
func TestStoryClaimIsInTheLedgerAndOnlyTheHolderReleasesIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	seedLiveAgent(t, "boss")
	seedLiveAgent(t, "worker")

	root := mustStoryRoot(t)
	if out, code := runSprint(t, "add", "ledger claim"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	store := todopkg.RepoStore(root)
	it, err := todopkg.Add(store, "pick me", "", "p0", nil, "", "")
	if err != nil {
		t.Skipf("todo store unavailable here: %v", err)
	}
	it.Sprint = 1
	if _, err := store.Save(it); err != nil {
		t.Fatalf("attach story to sprint: %v", err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("track exit=%d: %s", code, out)
	}

	out, code := runSprint(t, "claim", "1", it.ID, "--owner", "worker")
	if code != 0 {
		t.Fatalf("claim exit=%d: %s", code, out)
	}
	if who, ok := storyClaimHolder(t, 1, it.ID); !ok || who != "worker" {
		t.Fatalf("claim ledger holder = %q (present=%v), want worker", who, ok)
	}

	out, code = runSprint(t, "yield", "1", it.ID, "--as", "boss", "-m", "not mine")
	if code == 0 {
		t.Fatalf("a stranger yielded someone else's story:\n%s", out)
	}
	if !strings.Contains(out, "worker") {
		t.Errorf("refusal must name the holder:\n%s", out)
	}
	held, err := todopkg.ResolveRef(store, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Assignee != "worker" {
		t.Fatalf("refused yield still changed the assignee: %q", held.Assignee)
	}
	if who, ok := storyClaimHolder(t, 1, it.ID); !ok || who != "worker" {
		t.Fatalf("refused yield disturbed the ledger: holder=%q present=%v", who, ok)
	}

	if out, code := runSprint(t, "yield", "1", it.ID, "--as", "worker", "-m", "out of context"); code != 0 {
		t.Fatalf("yield exit=%d: %s", code, out)
	}
	if who, ok := storyClaimHolder(t, 1, it.ID); ok {
		t.Fatalf("yield left the claim in the ledger (holder %q)", who)
	}

	if out, code := runSprint(t, "claim", "1", it.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("re-claim exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "submit", "1", it.ID, "--as", "worker", "-m", "done"); code != 0 {
		t.Fatalf("submit exit=%d: %s", code, out)
	}
	if who, ok := storyClaimHolder(t, 1, it.ID); ok {
		t.Fatalf("submit left the claim in the ledger (holder %q)", who)
	}
}

// Sprint: #413; Story: #1829; Story-ID: ce5985c75042
// A session whose sandbox denies the claim ledger used to move the todo
// assignment, then report "claim ledger NOT updated" and exit 0 — a partial
// mutation. The preflight must fail the claim before either store changes, with
// a diagnosis that names the denial (not contention) and the explicit grant.
func TestStoryClaimDeniedLedgerFailsBeforeAnyMutation(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not deny root or Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	coordDir := filepath.Join(home, "coord-denied")
	t.Setenv("BASHY_COORD_DIR", coordDir)
	seedLiveAgent(t, "boss")
	seedLiveAgent(t, "worker")

	root := mustStoryRoot(t)
	if out, code := runSprint(t, "add", "denied ledger"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	store := todopkg.RepoStore(root)
	it, err := todopkg.Add(store, "guarded", "", "p0", nil, "", "")
	if err != nil {
		t.Skipf("todo store unavailable here: %v", err)
	}
	it.Sprint = 1
	if _, err := store.Save(it); err != nil {
		t.Fatalf("attach story to sprint: %v", err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("track exit=%d: %s", code, out)
	}

	if err := os.MkdirAll(coordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(coordDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(coordDir, 0o755) })

	out, code := runSprint(t, "claim", "1", it.ID, "--owner", "worker")
	if code == 0 {
		t.Fatalf("claim against a denied ledger exited 0 (partial mutation):\n%s", out)
	}
	if !strings.Contains(out, "DENIED") || !strings.Contains(out, "not lock contention") ||
		!strings.Contains(out, "--writable-root "+coordDir) {
		t.Fatalf("denial is not diagnosed actionably:\n%s", out)
	}
	got, err := todopkg.ResolveRef(store, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Status == todopkg.StatusAssigned {
		t.Fatalf("denied claim still moved the story: assignee=%q status=%q", got.Assignee, got.Status)
	}
	if card, _ := runSprint(t, "show", "1"); strings.Contains(card, "worker claimed") {
		t.Fatalf("denied claim still wrote the card note:\n%s", card)
	}

	// The grant restored, the same claim succeeds — nothing was forced or deleted.
	if err := os.Chmod(coordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "claim", "1", it.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("claim after grant exit=%d: %s", code, out)
	}
	if who, ok := storyClaimHolder(t, 1, it.ID); !ok || who != "worker" {
		t.Fatalf("ledger holder = %q (present=%v), want worker", who, ok)
	}
}
