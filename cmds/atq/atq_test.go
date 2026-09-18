package atqcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/schedule"
	"github.com/qiangli/coreutils/tool"
)

func runATQ(t *testing.T, args ...string) (string, string, int) {
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

func TestATQListsOnlyEnabledAtJobs(t *testing.T) {
	t.Setenv("BASHY_SCHEDULE_STATE", filepath.Join(t.TempDir(), "schedule.json"))
	identity, err := schedule.AuthenticatedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC)
	err = schedule.UpdateJobs(func([]*schedule.Job) ([]*schedule.Job, error) {
		return []*schedule.Job{
			{ID: "shown", Kind: "at", OwnerUID: identity.UID, Enabled: true, NextRun: next, Command: []string{"printf", "hello world"}},
			{ID: "disabled", Kind: "at", OwnerUID: identity.UID, Enabled: false, NextRun: next, Command: []string{"false"}},
			{ID: "cron", Kind: "cron", Enabled: true, NextRun: next, Command: []string{"false"}},
			{ID: "foreign", Kind: "at", OwnerUID: "other-uid", Enabled: true, NextRun: next, Command: []string{"echo", "foreign-secret"}},
			{ID: "legacy", Kind: "at", Enabled: true, NextRun: next, Command: []string{"echo", "legacy-secret"}},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	out, errb, code := runATQ(t)
	if code != 0 || errb != "" {
		t.Fatalf("atq: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "shown\t"+next.Format(time.RFC3339)+"\tprintf hello world") {
		t.Fatalf("atq missing enabled at job: %q", out)
	}
	if strings.Contains(out, "disabled") || strings.Contains(out, "cron") || strings.Contains(out, "foreign") || strings.Contains(out, "legacy") || strings.Contains(out, "secret") {
		t.Fatalf("atq disclosed non-pending jobs: %q", out)
	}
}

func TestATQEmptyAndUsage(t *testing.T) {
	t.Setenv("BASHY_SCHEDULE_STATE", filepath.Join(t.TempDir(), "schedule.json"))
	out, errb, code := runATQ(t)
	if code != 0 || errb != "" || out != "no pending at jobs\n" {
		t.Fatalf("empty atq: code=%d stdout=%q stderr=%q", code, out, errb)
	}
	_, errb, code = runATQ(t, "operand")
	if code != 2 || !strings.Contains(errb, "extra operand") {
		t.Fatalf("atq operand: code=%d stderr=%q", code, errb)
	}
}
