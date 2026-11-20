package weave

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/lens"
	"github.com/qiangli/yoke/pkg/room"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func isolateSprintDirectiveFixture(t *testing.T) (home, sprintDir string) {
	t.Helper()
	home = t.TempDir()
	sprintDir = filepath.Join(home, ".bashy", "sprint")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_SPRINT_DIR", sprintDir)
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, ".bashy", "room"))
	t.Setenv("BASHY_MB_DIR", filepath.Join(home, ".bashy", "mb"))
	t.Setenv("BASHY_MEET_DIR", filepath.Join(home, ".bashy", "meet"))
	t.Setenv("BASHY_TODO_DIR", filepath.Join(home, ".bashy", "todo"))
	t.Setenv("BASHY_FLEET_DIR", filepath.Join(home, ".config", "bashy"))
	t.Setenv("BASHY_TOOLS_DIR", "")
	t.Setenv("BASHY_MODELS_DIR", "")
	t.Setenv("BASHY_AGENTS_DIR", "")
	t.Setenv("BASHY_SPRINT_ANNOUNCE", "0")
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT", "")
	t.Setenv("BASHY_AGENT_ID", "")
	t.Setenv("WEAVE_AGENT", "")
	t.Setenv("WEAVE_OWNER", "")
	t.Setenv("WEAVE_CONDUCTOR", "")
	t.Setenv("WEAVE_ID", "")
	t.Setenv("WEAVE_ISSUE", "")
	t.Setenv("WEAVE_ISSUE_TITLE", "")
	t.Setenv("WEAVE_ISSUE_BODY", "")
	t.Setenv("WEAVE_BRANCH", "")
	t.Setenv("WEAVE_BASE", "")
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "")
	t.Setenv("USER", "operator-a")
	t.Setenv("LOGNAME", "operator-a")
	return home, sprintDir
}

func initCommittedGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.invalid"},
	} {
		if out, err := execCommandForTest("git", append([]string{"-C", repo}, args...)...); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-qm", "initial"},
	} {
		if out, err := execCommandForTest("git", append([]string{"-C", repo}, args...)...); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return repo
}

func TestSprintDirectiveAddListRmAndThreadAudit(t *testing.T) {
	_, sprintDir := isolateSprintDirectiveFixture(t)
	if out, code := runSprint(t, "add", "directive lifecycle"); code != 0 {
		t.Fatalf("sprint add exit=%d: %s", code, out)
	}

	// Empty list: plain and JSON.
	out, code := runSprint(t, "directive", "list", "1")
	if code != 0 || !strings.Contains(out, "no active directives") {
		t.Fatalf("empty list exit=%d: %s", code, out)
	}
	jsonOut, code := runSprint(t, "directive", "list", "--json")
	if code != 0 {
		t.Fatalf("json list exit=%d: %s", code, jsonOut)
	}
	var listEnv struct {
		Status string `json:"status"`
		Result struct {
			Sprint     int64             `json:"sprint"`
			Directives []SprintDirective `json:"directives"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &listEnv); err != nil {
		t.Fatalf("unmarshal list json: %v\n%s", err, jsonOut)
	}
	if listEnv.Status != "ok" || listEnv.Result.Sprint != 1 || listEnv.Result.Directives == nil || len(listEnv.Result.Directives) != 0 {
		t.Fatalf("unexpected empty json list: %+v (%s)", listEnv, jsonOut)
	}

	// Add with explicit sprint, --by, and --preapprove.
	out, code = runSprint(t, "directive", "add", "1", "ROOT CAUSE FIRST; never push", "--by", "lead-op", "--preapprove", "defer:p2,p3")
	if code != 0 {
		t.Fatalf("directive add #1 exit=%d: %s", code, out)
	}
	for _, want := range []string{"+#1", "lead-op", "defer:p2,p3", "ROOT CAUSE FIRST; never push"} {
		if !strings.Contains(out, want) {
			t.Errorf("directive add output missing %q: %s", want, out)
		}
	}

	// Add with omitted sprint (defaults to latest card #1) and default actor.
	out, code = runSprint(t, "directive", "add", "Go tests GOWORK=off, targeted packages only")
	if code != 0 {
		t.Fatalf("directive add #2 (default sprint) exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "+#2") || !strings.Contains(out, "operator-a") {
		t.Fatalf("directive add #2 output = %s", out)
	}

	// Stored directly in queue.json on the card + thread; no extra store files created.
	entries, err := os.ReadDir(sprintDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "queue.json" && name != "queue.lock" {
			t.Fatalf("unexpected extra store entry %q in %s", name, sprintDir)
		}
	}

	listText, code := runSprint(t, "directive", "list")
	if code != 0 {
		t.Fatalf("directive list exit=%d: %s", code, listText)
	}
	for _, want := range []string{
		"sprint #1 directives (2 active)",
		"[#1 by lead-op; preapprove: defer:p2,p3] ROOT CAUSE FIRST; never push",
		"[#2 by operator-a] Go tests GOWORK=off, targeted packages only",
	} {
		if !strings.Contains(listText, want) {
			t.Errorf("directive list missing %q:\n%s", want, listText)
		}
	}

	// Remove #1 (omitting sprint, passing #1); next add must allocate #3, never reusing #1.
	rmOut, code := runSprint(t, "directive", "rm", "#1", "--by", "lead-op", "--reason", "scope tightened")
	if code != 0 {
		t.Fatalf("directive rm exit=%d: %s", code, rmOut)
	}
	if !strings.Contains(rmOut, "removed directive #1 (1 active)") {
		t.Fatalf("unexpected rm output: %s", rmOut)
	}

	if out, code := runSprint(t, "directive", "add", "last", "Never push tags", "--by", "lead-op"); code != 0 || !strings.Contains(out, "+#3") {
		t.Fatalf("directive add #3 after rm did not allocate monotonic #3: exit=%d out=%s", code, out)
	}

	q, err := loadWeaveQueue(sprintDir)
	if err != nil {
		t.Fatal(err)
	}
	card := findWeaveStory(q, 1)
	if len(card.Directives) != 2 || card.Directives[0].ID != "2" || card.Directives[1].ID != "3" {
		t.Fatalf("active directives on card = %+v", card.Directives)
	}

	// Both additions and the removal are preserved on the card's thread.
	var threadJoined strings.Builder
	for _, c := range card.Thread {
		threadJoined.WriteString(c.Author + ":" + c.Kind + ":" + c.Body + "\n")
	}
	for _, want := range []string{
		"lead-op:decision:added directive #1 (by lead-op; preapprove: defer:p2,p3): ROOT CAUSE FIRST; never push",
		"lead-op:decision:removed directive #1 — scope tightened",
		"was (by lead-op; preapprove: defer:p2,p3): ROOT CAUSE FIRST; never push",
		"lead-op:decision:added directive #3 (by lead-op): Never push tags",
	} {
		if !strings.Contains(threadJoined.String(), want) {
			t.Errorf("thread missing %q:\n%s", want, threadJoined.String())
		}
	}

	// Operator directives work without the manager's lease token even under BASHY_SPRINT_ENFORCE=must.
	seedLiveAgent(t, "mgr-agent")
	if out, code := runSprint(t, "take", "1", "--owner", "mgr-agent"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "")
	if out, code := runSprint(t, "directive", "add", "1", "Operator rule while lease held", "--by", "lead-op"); code != 0 {
		t.Fatalf("operator directive refused under BASHY_SPRINT_ENFORCE=must: exit=%d %s", code, out)
	}

	// Validation errors fail loudly with ExitInvalidArg.
	for _, bad := range [][]string{
		{"directive", "add", "1", "   "},
		{"directive", "add", "1", "valid text", "--by", "   "},
		{"directive", "add", "1", "valid text", "--preapprove", "   "},
		{"directive", "add", "1", "valid text", "--id", "2"},
		{"directive", "rm", "1", "999"},
	} {
		stdout, stderr, exitCode, _ := runSprintStreams(t, bad...)
		if exitCode != weavecli.ExitInvalidArg || strings.TrimSpace(stderr) == "" {
			t.Errorf("sprint %v: exit=%d stdout=%q stderr=%q, want ExitInvalidArg (%d) and non-empty stderr",
				bad, exitCode, stdout, stderr, weavecli.ExitInvalidArg)
		}
	}
}

func TestSprintDirectivePreapproveMatching(t *testing.T) {
	isolateSprintDirectiveFixture(t)
	if out, code := runSprint(t, "add", "preapprovals"); code != 0 {
		t.Fatalf("add: %s", out)
	}
	if out, code := runSprint(t, "directive", "add", "1", "Defer low-priority items", "--by", "lead-op", "--preapprove", "defer:p2,p3", "--preapprove", "drop:p3"); code != 0 {
		t.Fatalf("directive add: %s", out)
	}

	cases := []struct {
		action string
		target string
		want   bool
	}{
		{"defer", "p2", true},
		{"defer", "P3", true},
		{"defer", "p0", false},
		{"defer", "p1", false},
		{"drop", "p3", true},
		{"drop", "p2", false},
		{"merge", "p3", false},
	}
	for _, tc := range cases {
		d, ok := SprintPreapproves(1, tc.action, tc.target)
		if ok != tc.want {
			t.Errorf("SprintPreapproves(%q, %q) = (%+v, %v), want %v", tc.action, tc.target, d, ok, tc.want)
		}
	}

	if out, code := runSprint(t, "directive", "rm", "1", "1"); code != 0 {
		t.Fatalf("directive rm: %s", out)
	}
	if _, ok := SprintPreapproves(1, "defer", "p2"); ok {
		t.Fatal("removed directive still pre-approved defer:p2")
	}
}

func TestSprintDirectiveVisibleInShowTickTakeStartAndInstructPayloads(t *testing.T) {
	isolateSprintDirectiveFixture(t)
	seedLiveAgent(t, "mgr-one")
	seedLiveAgent(t, "mgr-two")

	if out, code := runSprint(t, "add", "manager payloads"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "directive", "add", "1", "ROOT CAUSE FIRST and record it", "--by", "lead-op", "--preapprove", "defer:p2,p3"); code != 0 {
		t.Fatalf("directive add #1 exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "directive", "add", "1", "Never push cloudbox tags", "--by", "lead-op"); code != 0 {
		t.Fatalf("directive add #2 exit=%d: %s", code, out)
	}

	// 1. sprint show (plain + JSON)
	showOut, code := runSprint(t, "show", "1")
	if code != 0 {
		t.Fatalf("show exit=%d: %s", code, showOut)
	}
	for _, want := range []string{
		"── directives (standing operator rules) ──",
		"[#1 by lead-op; preapprove: defer:p2,p3] ROOT CAUSE FIRST and record it",
		"[#2 by lead-op] Never push cloudbox tags",
	} {
		if !strings.Contains(showOut, want) {
			t.Errorf("sprint show missing %q:\n%s", want, showOut)
		}
	}
	showJSON, code := runSprint(t, "show", "1", "--json")
	if code != 0 {
		t.Fatalf("show --json exit=%d: %s", code, showJSON)
	}
	if !strings.Contains(showJSON, `"ROOT CAUSE FIRST and record it"`) || !strings.Contains(showJSON, `"defer:p2,p3"`) {
		t.Fatalf("sprint show --json missing directives:\n%s", showJSON)
	}

	// 2. sprint start --instruction + stdout payload
	var requests []SprintOwnerRequest
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		requests = append(requests, req)
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: "sprint-1-manager", Transport: room.TransportManaged}, nil
	})
	withSprintOwnerStopper(t, func(context.Context, SprintOwnerRequest) error { return nil })

	startOut, code := runSprint(t, "start", "1", "--owner", "mgr-one", "--for", "1h", "--instruction", "begin sprint execution")
	if code != 0 {
		t.Fatalf("start exit=%d: %s", code, startOut)
	}
	for _, want := range []string{
		"SPRINT STANDING DIRECTIVES (sprint #1 — active operator rules):",
		"[#1 by lead-op; preapprove: defer:p2,p3] ROOT CAUSE FIRST and record it",
		"[#2 by lead-op] Never push cloudbox tags",
	} {
		if !strings.Contains(startOut, want) {
			t.Errorf("sprint start output missing %q:\n%s", want, startOut)
		}
		if len(requests) != 1 || !strings.Contains(requests[0].Brief, want) {
			t.Errorf("sprint start manager request Brief missing %q:\n%+v", want, requests)
		}
	}
	if !strings.HasPrefix(requests[0].Brief, "begin sprint execution\n\nSPRINT STANDING DIRECTIVES") {
		t.Fatalf("unexpected sprint start Brief layout:\n%s", requests[0].Brief)
	}

	// 3. sprint instruct payload — then remove directive #2 and verify only active #1 remains in next instruct.
	instructOut, code := runSprint(t, "instruct", "1", "--instruction", "review submitted lanes")
	if code != 0 {
		t.Fatalf("instruct exit=%d: %s", code, instructOut)
	}
	if len(requests) != 2 || !strings.Contains(requests[1].Brief, "review submitted lanes") ||
		!strings.Contains(requests[1].Brief, "ROOT CAUSE FIRST and record it") ||
		!strings.Contains(requests[1].Brief, "Never push cloudbox tags") {
		t.Fatalf("sprint instruct Brief = %q", requests[1].Brief)
	}

	if out, code := runSprint(t, "directive", "rm", "1", "2", "--by", "lead-op"); code != 0 {
		t.Fatalf("directive rm #2: %s", out)
	}
	if out, code := runSprint(t, "instruct", "1", "--instruction", "re-check queue"); code != 0 {
		t.Fatalf("instruct after rm exit=%d: %s", code, out)
	}
	if len(requests) != 3 {
		t.Fatalf("requests len = %d, want 3", len(requests))
	}
	if !strings.Contains(requests[2].Brief, "ROOT CAUSE FIRST and record it") {
		t.Errorf("active directive #1 missing after rm #2:\n%s", requests[2].Brief)
	}
	if strings.Contains(requests[2].Brief, "Never push cloudbox tags") {
		t.Errorf("removed directive #2 still injected into instruct payload:\n%s", requests[2].Brief)
	}

	// 4. sprint tick (plain + JSON) carries only active directives.
	tickOut, code := runSprint(t, "tick", "1")
	if code != 0 {
		t.Fatalf("tick exit=%d: %s", code, tickOut)
	}
	if !strings.Contains(tickOut, "ROOT CAUSE FIRST and record it") || strings.Contains(tickOut, "Never push cloudbox tags") {
		t.Fatalf("sprint tick output mismatch:\n%s", tickOut)
	}
	tickJSON, code := runSprint(t, "tick", "1", "--json")
	if code != 0 || !strings.Contains(tickJSON, `"ROOT CAUSE FIRST and record it"`) || strings.Contains(tickJSON, `"Never push cloudbox tags"`) {
		t.Fatalf("sprint tick --json mismatch (exit=%d):\n%s", code, tickJSON)
	}

	// 5. sprint take output carries active directives.
	takeOut, code := runSprint(t, "take", "1", "--owner", "mgr-two", "--force")
	if code != 0 {
		t.Fatalf("take exit=%d: %s", code, takeOut)
	}
	if !strings.Contains(takeOut, "SPRINT STANDING DIRECTIVES (sprint #1 — active operator rules):") ||
		!strings.Contains(takeOut, "ROOT CAUSE FIRST and record it") ||
		strings.Contains(takeOut, "Never push cloudbox tags") {
		t.Fatalf("sprint take output mismatch:\n%s", takeOut)
	}
}

func TestSprintDirectiveAppendedToEveryWeaveBriefForSprint(t *testing.T) {
	isolateSprintDirectiveFixture(t)
	repo := initCommittedGitRepo(t)
	t.Chdir(repo)

	if out, code := runSprint(t, "add", "weave brief directives"); code != 0 {
		t.Fatalf("sprint add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "track", "1", "--repo", repo); code != 0 {
		t.Fatalf("sprint track exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "directive", "add", "1", "ROOT CAUSE FIRST; GOWORK=off targeted tests only", "--by", "lead-op"); code != 0 {
		t.Fatalf("directive add exit=%d: %s", code, out)
	}

	qdir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	readItem := func(id int64) *weaveItem {
		t.Helper()
		q, err := loadWeaveQueue(qdir)
		if err != nil {
			t.Fatal(err)
		}
		it := findWeaveItem(q, id)
		if it == nil {
			t.Fatalf("weave run #%d not found", id)
		}
		return it
	}

	// Path 1: sprintAssignSeed appends active directives to fleet briefs, while leaving --script-task digest intact.
	st := todopkg.RepoStore(repo)
	assignStory, err := todopkg.Add(st, "assigned story", "Implement assigned story.", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	assignStory.Sprint = 1
	if _, err := st.Save(assignStory); err != nil {
		t.Fatal(err)
	}
	assignRunID, err := sprintAssignSeed(&cobra.Command{}, 1, repo, assignStory, 3, 3)
	if err != nil {
		t.Fatalf("sprintAssignSeed: %v", err)
	}
	if got := readItem(assignRunID).Body; !strings.Contains(got, "ROOT CAUSE FIRST; GOWORK=off targeted tests only") {
		t.Fatalf("sprintAssignSeed brief missing directive:\n%s", got)
	}

	scriptStory, err := todopkg.Add(st, "script story", "Scripted task", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scriptStory.Sprint = 1
	if _, err := st.Save(scriptStory); err != nil {
		t.Fatal(err)
	}
	exactScript := &weaveScriptAssignment{Body: `{"exact":"script-contract"}`, Digest: "sha256:test", Runner: "/bin/true"}
	_ = lens.ScriptTask{}
	scriptRunID, err := sprintAssignSeed(&cobra.Command{}, 1, repo, scriptStory, 1, 3, exactScript)
	if err != nil {
		t.Fatalf("sprintAssignSeed script: %v", err)
	}
	if got := readItem(scriptRunID).Body; got != exactScript.Body {
		t.Fatalf("script assignment body was mutated: got %q, want %q", got, exactScript.Body)
	}

	// Path 2: weave add --from-todo for a sprint story appends active directives.
	todoStory, err := todopkg.Add(st, "from-todo story", "Implement from-todo story.", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	todoStory.Sprint = 1
	if _, err := st.Save(todoStory); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "add", "--from-todo", todoStory.ID, "--points", "2"); code != 0 {
		t.Fatalf("weave add --from-todo exit=%d: %s", code, out)
	}
	todoUpdated, err := todopkg.ResolveRef(st, todoStory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readItem(todoUpdated.Weave).Body; !strings.Contains(got, "ROOT CAUSE FIRST; GOWORK=off targeted tests only") {
		t.Fatalf("weave add --from-todo brief missing directive:\n%s", got)
	}

	// Path 3: weave add with Sprint trailer appends active directives immediately.
	trailerBody := "Implement trailer story.\n\nSprint: #1\nStory: #99\nStory-ID: 0123456789ab"
	if out, code := runWeave(t, "add", "trailer run", "--body", trailerBody, "--points", "2"); code != 0 {
		t.Fatalf("weave add trailer exit=%d: %s", code, out)
	}
	qAfterTrailer, err := loadWeaveQueue(qdir)
	if err != nil {
		t.Fatal(err)
	}
	trailerRun := qAfterTrailer.Items[len(qAfterTrailer.Items)-1]
	if !strings.Contains(trailerRun.Body, "ROOT CAUSE FIRST; GOWORK=off targeted tests only") {
		t.Fatalf("weave add with Sprint trailer missing directive:\n%s", trailerRun.Body)
	}

	// Path 4: weave add (unattributed at add time) -> sprint link -> directive update -> weave start (--no-spawn)
	// syncs the sprint's current active directives without duplicating or leaking into standalone runs.
	if out, code := runWeave(t, "add", "linked later title only", "--points", "2"); code != 0 {
		t.Fatalf("weave add linked-later exit=%d: %s", code, out)
	}
	if out, code := runWeave(t, "add", "standalone non-sprint work", "--body", "Independent task.", "--points", "2"); code != 0 {
		t.Fatalf("weave add standalone exit=%d: %s", code, out)
	}
	qNow, err := loadWeaveQueue(qdir)
	if err != nil {
		t.Fatal(err)
	}
	linkedRunID := qNow.Items[len(qNow.Items)-2].ID
	standaloneRunID := qNow.Items[len(qNow.Items)-1].ID

	if out, code := runSprint(t, "link", "1", "--repo", filepath.Base(repo), "--task", jsonNumberString(linkedRunID)); code != 0 {
		t.Fatalf("sprint link exit=%d: %s", code, out)
	}
	// Add a second directive and remove the first before starting both runs:
	// started sprint runs must receive the CURRENT active set (no stale directive, no duplicate block).
	if out, code := runSprint(t, "directive", "add", "1", "Commit on your weave branch, never push", "--by", "lead-op"); code != 0 {
		t.Fatalf("directive add #2: %s", out)
	}
	if out, code := runSprint(t, "directive", "rm", "1", "1", "--by", "lead-op"); code != 0 {
		t.Fatalf("directive rm #1: %s", out)
	}

	for _, runID := range []int64{assignRunID, linkedRunID, standaloneRunID} {
		if out, code := runWeave(t, "start", "--run", jsonNumberString(runID), "--no-spawn"); code != 0 {
			t.Fatalf("weave start --run %d --no-spawn exit=%d: %s", runID, code, out)
		}
	}

	// Assigned run had directive #1 at seed time; starting it refreshes to active #2 only, once.
	refreshedAssignBody := readItem(assignRunID).Body
	if !strings.Contains(refreshedAssignBody, "Commit on your weave branch, never push") {
		t.Errorf("started assigned run missing new directive #2:\n%s", refreshedAssignBody)
	}
	if strings.Contains(refreshedAssignBody, "GOWORK=off targeted tests only") {
		t.Errorf("started assigned run kept removed directive #1:\n%s", refreshedAssignBody)
	}
	if n := strings.Count(refreshedAssignBody, sprintDirectivesBlockHeaderPrefix); n != 1 {
		t.Errorf("directives block count = %d, want 1:\n%s", n, refreshedAssignBody)
	}

	// Title-only run linked after `weave add` preserves its title AND gets active directive #2 on `weave start`.
	linkedBody := readItem(linkedRunID).Body
	if !strings.Contains(linkedBody, "linked later title only") || !strings.Contains(linkedBody, "Commit on your weave branch, never push") {
		t.Errorf("started linked run body missing title or directive:\n%s", linkedBody)
	}

	// Standalone run not belonging to any sprint stays untouched.
	if got := readItem(standaloneRunID).Body; got != "Independent task." {
		t.Errorf("standalone run body was polluted: %q", got)
	}

	// Verify weaveExpandAgent + weaveChildEnv deliver the active directives in both CLI prompt argv and WEAVE_ISSUE_BODY.
	seedAgent(t, "worker-one")
	startedItem := readItem(linkedRunID)
	launch, argv, err := weaveExpandAgent([]string{"worker-one"}, startedItem.Body, startedItem.Title)
	if err != nil || launch == nil || len(argv) == 0 {
		t.Fatalf("weaveExpandAgent: launch=%+v argv=%v err=%v", launch, argv, err)
	}
	promptArg := argv[len(argv)-1]
	if !strings.Contains(promptArg, "Commit on your weave branch, never push") || !strings.Contains(promptArg, "WEAVE WORKER CONTRACT") {
		t.Fatalf("worker CLI prompt arg missing active directive or worker contract:\n%s", promptArg)
	}
	childEnv := weaveChildEnv(os.Environ(), t.TempDir(), "agent/weave-issue-test", "main", qdir, startedItem, launch)
	envBody := boothGetEnv(childEnv, "WEAVE_ISSUE_BODY")
	if !strings.Contains(envBody, "Commit on your weave branch, never push") {
		t.Fatalf("WEAVE_ISSUE_BODY missing active directive: %q", envBody)
	}
}

func jsonNumberString(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
