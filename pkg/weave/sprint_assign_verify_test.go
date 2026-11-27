package weave

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// A run assigned with --verify records the command, so the worker's terminal
// state carries a verify result and the run can submit its own story. Without
// it weaveSubmitWorkerStories refuses (Sprint 737: all three lift runs hit
// that refusal and the conductor had to submit by hand).
func TestSprintAssignRecordsVerifyCommand(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	home := t.TempDir()
	for k, v := range map[string]string{"BASHY_HOME": home, "BASHY_SPRINT_DIR": filepath.Join(home, "sprint"), "BASHY_ROOM_DIR": filepath.Join(home, "room")} {
		t.Setenv(k, v)
	}
	if out, code := runSprint(t, "add", "verify fixture", "--announce=false"); code != 0 {
		t.Fatal(out)
	}
	story, err := todopkg.Add(todopkg.RepoStore(root), "lift", "lift a surface", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	if _, err = todopkg.RepoStore(root).Save(story); err != nil {
		t.Fatal(err)
	}
	run, err := sprintAssignSeed(&cobra.Command{}, 1, root, story, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	const verify = "bashy conform spec check shapeware/x"
	if err := sprintAssignSetVerify(root, run, verify); err != nil {
		t.Fatal(err)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	item := findWeaveItem(q, run)
	if item == nil || item.VerifyCommand != verify {
		t.Fatalf("verify not recorded on run #%d: %+v", run, item)
	}
	if err := sprintAssignSetVerify(root, run+100, verify); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown run must be refused, got %v", err)
	}
}
