package atrmcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/schedule"
	"github.com/qiangli/coreutils/tool"
)

func runATRM(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   t.TempDir(),
		Env:   []string{"BASHY_SCHEDULE_STATE=" + os.Getenv("BASHY_SCHEDULE_STATE")},
		Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")},
	}
	code := cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestATRMRemovesByIDAndName(t *testing.T) {
	t.Setenv("BASHY_SCHEDULE_STATE", filepath.Join(t.TempDir(), "schedule.json"))
	identity, err := schedule.AuthenticatedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	err = schedule.UpdateJobs(func([]*schedule.Job) ([]*schedule.Job, error) {
		return []*schedule.Job{
			{ID: "one", Name: "first", Kind: "at", OwnerUID: identity.UID},
			{ID: "two", Name: "second", Kind: "at", OwnerUID: identity.UID},
			{ID: "keep", Kind: "at", OwnerUID: identity.UID},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	out, errb, code := runATRM(t, "one", "second")
	if code != 0 || out != "" || errb != "" {
		t.Fatalf("atrm: code=%d stdout=%q stderr=%q", code, out, errb)
	}
	jobs, err := schedule.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "keep" {
		t.Fatalf("remaining jobs=%+v, want only keep", jobs)
	}
}

func TestATRMMissingOperandAndUnknownJob(t *testing.T) {
	t.Setenv("BASHY_SCHEDULE_STATE", filepath.Join(t.TempDir(), "schedule.json"))
	_, errb, code := runATRM(t)
	if code != 2 || !strings.Contains(errb, "missing job ID") {
		t.Fatalf("atrm no operand: code=%d stderr=%q", code, errb)
	}
	out, errb, code := runATRM(t, "missing")
	if code != 1 || out != "" || !strings.Contains(errb, `no job "missing"`) {
		t.Fatalf("atrm missing job: code=%d stdout=%q stderr=%q", code, out, errb)
	}
}

func TestATRMForeignLegacyAndNonAtJobsFailClosedAtomically(t *testing.T) {
	t.Setenv("BASHY_SCHEDULE_STATE", filepath.Join(t.TempDir(), "schedule.json"))
	identity, err := schedule.AuthenticatedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	want := []*schedule.Job{
		{ID: "mine", Kind: "at", OwnerUID: identity.UID},
		{ID: "foreign", Kind: "at", OwnerUID: "other-uid"},
		{ID: "legacy", Kind: "at"},
		{ID: "generic", Kind: "every", OwnerUID: identity.UID},
	}
	if err := schedule.SaveJobs(want); err != nil {
		t.Fatal(err)
	}
	for _, inaccessible := range []string{"foreign", "legacy", "generic"} {
		_, stderr, code := runATRM(t, "mine", inaccessible)
		if code != 1 || !strings.Contains(stderr, `no job "`+inaccessible+`"`) {
			t.Fatalf("atrm mixed %s: code=%d stderr=%q", inaccessible, code, stderr)
		}
		jobs, err := schedule.LoadJobs()
		if err != nil || len(jobs) != len(want) {
			t.Fatalf("atrm mixed %s partially mutated jobs=%+v err=%v", inaccessible, jobs, err)
		}
	}
}
