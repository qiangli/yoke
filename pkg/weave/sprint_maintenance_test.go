package weave

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func maintenanceFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	board := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", board)
	t.Setenv("BASHY_SPRINT_ANNOUNCE", "0")
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/test")
	return board, mustStoryRoot(t)
}

func maintenanceTodo(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := todopkg.NewTodoCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--base-dir", root}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestMaintenanceTodoRouting(t *testing.T) {
	board, root := maintenanceFixture(t)
	for _, tc := range []struct {
		name  string
		flags []string
		want  int64
	}{
		{"default", nil, 9999}, {"explicit", []string{"--sprint", "42"}, 42},
		{"opt-out", []string{"--no-sprint"}, 0}, {"maintenance", []string{"--sprint", "9999"}, 9999},
	} {
		if out, err := maintenanceTodo(t, root, append([]string{"add", tc.name}, tc.flags...)...); err != nil {
			t.Fatalf("%s: %v: %s", tc.name, err, out)
		}
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			t.Fatal(err)
		}
		var found *issue.Issue
		for _, it := range items {
			if it.Title == tc.name {
				found = it
			}
		}
		if found == nil || found.Sprint != tc.want {
			t.Fatalf("%s: %+v", tc.name, found)
		}
		if tc.want == 9999 && found.SprintID != maintenanceUUID {
			t.Fatalf("missing portable identity: %+v", found)
		}
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Stories) != 1 || len(q.Stories[0].StoryRoots) != 1 || q.NextStoryID != 0 {
		t.Fatalf("unexpected board %+v", q)
	}
	if out, err := maintenanceTodo(t, root, "add", "bad", "--no-sprint", "--sprint", "42"); err == nil {
		t.Fatalf("conflicting flags accepted: %s", out)
	}
	personalCmd := todopkg.NewTodoCmd()
	personalCmd.SetArgs([]string{"--user", "add", "personal"})
	if err := personalCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	personal, err := todopkg.UserStore(todopkg.DefaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	items, err := todopkg.List(personal, "")
	if err != nil || len(items) != 1 || items[0].Sprint != 0 {
		t.Fatalf("personal default: %v %+v", err, items)
	}
}

func TestMaintenanceInitializationAndAllocation(t *testing.T) {
	board, root := maintenanceFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, err := prepareMaintenance(todopkg.RepoStore(root)); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Stories) != 1 || len(q.Stories[0].StoryRoots) != 1 {
		t.Fatalf("duplicate initialization: %+v", q)
	}
	q.NextStoryID = 9998
	if got := nextOrdinarySprintID(q); got != 9998 {
		t.Fatal(got)
	}
	if got := nextOrdinarySprintID(q); got != 10000 {
		t.Fatal(got)
	}
	q.NextStoryID = 1
	if got := nextOrdinarySprintID(q); got != 1 {
		t.Fatal(got)
	}
	q.Stories[0].Column = "done"
	if err := saveWeaveQueue(board, q); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareMaintenance(todopkg.RepoStore(root)); err != nil {
		t.Fatal(err)
	}
	q, _ = loadWeaveQueue(board)
	if q.Stories[0].Column != "done" {
		t.Fatal("filing silently reopened done sprint")
	}
	q.Stories[0].UUID = "ordinary"
	if err := saveWeaveQueue(board, q); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenanceTodo(t, root, "add", "collision"); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatal(err)
	}
	items, _ := todopkg.List(todopkg.RepoStore(root), "")
	if len(items) != 0 {
		t.Fatal("conflict left a story behind")
	}
}

func TestMaintenanceBoardFailureLeavesNoStory(t *testing.T) {
	_, root := maintenanceFixture(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_SPRINT_DIR", blocker)
	if _, err := maintenanceTodo(t, root, "add", "cannot file"); err == nil {
		t.Fatal("bad board accepted")
	}
	items, _ := todopkg.List(todopkg.RepoStore(root), "")
	if len(items) != 0 {
		t.Fatal("board failure left a story behind")
	}
}

func TestMaintenanceOmittedOperandsReachHostAdapters(t *testing.T) {
	maintenanceFixture(t)
	var received []string
	cmd := NewSprintCmd(func(root *cobra.Command) {
		var replace func(*cobra.Command)
		replace = func(c *cobra.Command) {
			for _, child := range c.Commands() {
				replace(child)
			}
			if c.RunE != nil {
				c.RunE = func(_ *cobra.Command, args []string) error { received = append([]string(nil), args...); return nil }
			}
		}
		replace(root)
	})
	for _, tc := range []struct {
		path []string
		args []string
		want string
	}{
		{[]string{"show"}, nil, "9999"},
		{[]string{"show"}, []string{"42"}, "42"},
		{[]string{"claim"}, []string{"story-id"}, "9999,story-id"},
		{[]string{"claim"}, []string{"42", "story-id"}, "42,story-id"},
		{[]string{"move"}, []string{"doing"}, "9999,doing"},
		{[]string{"goal", "link"}, []string{"goal-id"}, "9999,goal-id"},
		{[]string{"arena", "status"}, nil, "9999"},
		{[]string{"grade"}, nil, "9999"},
		{[]string{"start"}, nil, "9999"},
		{[]string{"stop"}, nil, "9999"},
		{[]string{"end"}, nil, "9999"},
		{[]string{"advance"}, nil, "9999"},
		{[]string{"comment"}, nil, "9999"},
		{[]string{"add"}, []string{"new sprint title"}, "new sprint title"},
		{[]string{"board"}, nil, ""},
	} {
		c, _, err := cmd.Find(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateArgs(tc.args); err != nil {
			t.Fatalf("%v args: %v", tc.path, err)
		}
		if err := c.RunE(c, tc.args); err != nil {
			t.Fatalf("%v run: %v", tc.path, err)
		}
		if got := strings.Join(received, ","); got != tc.want {
			t.Fatalf("%v: got %q want %q", tc.path, got, tc.want)
		}
	}
}

func TestMaintenancePortableReconstruction(t *testing.T) {
	board, root := maintenanceFixture(t)
	st := todopkg.RepoStore(root)
	it, err := todopkg.Add(st, "portable fix", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint, it.SprintID, it.SprintTitle = 9999, maintenanceUUID, "Maintenance"
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s, created, err := ensureSprintFromRepo(board, maintenanceUUID)
	if err != nil || !created || !isMaintenance(s) {
		t.Fatalf("reconstruction: %v %v %+v", err, created, s)
	}
	q, _ := loadWeaveQueue(board)
	if got := nextOrdinarySprintID(q); got != 1 {
		t.Fatalf("default bumped normal numbering to %d", got)
	}
	// A foreign ordinary sprint filed under the reserved label must be
	// reconstructed under an ordinary local number, preserving its UUID.
	it.SprintID = "11111111-2222-5333-8444-555555555555"
	it.SprintTitle = "Foreign ordinary sprint"
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s, _, err = ensureSprintFromRepo(board, it.SprintID)
	if err != nil || s.ID != 1 || s.UUID != it.SprintID {
		t.Fatalf("foreign reserved label: %v %+v", err, s)
	}
}

func TestMaintenanceDefaultCommandsAndOrdinaryLifecycle(t *testing.T) {
	board, root := maintenanceFixture(t)
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	t.Setenv("BASHY_AGENT", "boss")
	t.Setenv("BASHY_PRINCIPAL", "boss")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "")
	seedLiveAgent(t, "boss")
	seedLiveAgent(t, "worker")
	if out, err := maintenanceTodo(t, root, "add", "urgent fix"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	items, _ := todopkg.List(todopkg.RepoStore(root), "")
	it := items[0]
	if out, err := runSprintErr(t, "show"); err != nil || !strings.Contains(out, "sprint #9999") {
		t.Fatalf("show: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "claim", it.ID, "--owner", "worker"); err == nil || !strings.Contains(out, "ACTIVE") {
		t.Fatalf("inactive claim: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "take", "--owner", "boss"); err != nil {
		t.Fatalf("take: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "move", "doing"); err != nil {
		t.Fatalf("move: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "claim", it.ID, "--owner", "worker"); err != nil {
		t.Fatalf("claim: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "claim", it.ID, "--owner", "boss"); err == nil || !strings.Contains(out, "held by worker") {
		t.Fatalf("competing claim: %v %s", err, out)
	}
	if _, err := todopkg.SetStatus(todopkg.RepoStore(root), it.ID, todopkg.StatusDone); err == nil {
		t.Fatal("generic close accepted maintenance story")
	}
	if out, err := runSprintErr(t, "accept", it.ID, "-m", "verified", "--no-rating"); err == nil || !strings.Contains(out, "no submitted") {
		t.Fatalf("missing submission: %v %s", err, out)
	}
	if out, err := runSprintErr(t, "submit", it.ID, "--as", "worker", "-m", "fix and regression verified"); err != nil {
		t.Fatalf("submit: %v %s", err, out)
	}
	message := filepath.Join(root, "commit-message")
	if err := os.WriteFile(message, []byte(fmt.Sprintf("fix: urgent repair\n\nSprint: #9999\nStory: #%d\nStory-ID: %s\nAgent: worker\n", it.Seq, it.ID)), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSprintErr(t, "commit-msg", message); err != nil {
		t.Fatalf("maintenance provenance: %v %s", err, out)
	}
	t.Setenv("WEAVE_CONDUCTOR", "worker")
	t.Setenv("BASHY_AGENT", "worker")
	t.Setenv("BASHY_PRINCIPAL", "worker")
	if out, err := runSprintErr(t, "accept", it.ID, "-m", "self approval", "--no-rating"); err == nil {
		t.Fatalf("worker accepted without manager authorization: %s", out)
	}
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	t.Setenv("BASHY_AGENT", "boss")
	t.Setenv("BASHY_PRINCIPAL", "boss")
	if out, err := runSprintErr(t, "accept", it.ID, "-m", "independently verified", "--no-rating"); err != nil {
		t.Fatalf("accept: %v %s", err, out)
	}
	got, _ := todopkg.ResolveRef(todopkg.RepoStore(root), it.ID)
	if got.Status != todopkg.StatusDone || got.ClosedBy != "boss" {
		t.Fatalf("closure: %+v", got)
	}
	if out, err := runSprintErr(t, "handoff", "-m", "urgent fix delivered"); err != nil {
		t.Fatalf("handoff: %v %s", err, out)
	}
	q, _ := loadWeaveQueue(board)
	if q.Stories[0].Lease != nil && q.Stories[0].Lease.Holder != "" {
		t.Fatal("handoff retained lease")
	}
	// No special closure restriction: ordinary hygiene/coverage determines
	// whether it can finish, just as for any other sprint.
	q.Stories[0].Column = "backlog"
	q.Stories[0].Owner = ""
	q.Stories[0].Runs = nil
	q.Stories[0].Contact = nil
	q.Stories[0].Lease = nil
	q.Stories[0].Created = time.Now().UTC()
	if err := saveWeaveQueue(board, q); err != nil {
		t.Fatal(err)
	}
	if out, err := runSprintErr(t, "rm"); err != nil {
		t.Fatalf("ordinary removal: %v %s", err, out)
	}
}
