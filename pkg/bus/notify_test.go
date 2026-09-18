package bus

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/schedule"
	"github.com/qiangli/yoke/pkg/room"
)

func runNotifyCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewNotifyCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func withNotifyTargets(t *testing.T, agents ...string) {
	t.Helper()
	oldNames, oldResolve, oldRoles := FleetNames, FleetResolveName, HostRoles
	FleetNames = func() []string { return agents }
	FleetResolveName = func(s string) string { return s }
	HostRoles = func() []HostRole {
		return []HostRole{{Label: "steward", Topic: "steward.host-1"}}
	}
	t.Cleanup(func() {
		FleetNames, FleetResolveName, HostRoles = oldNames, oldResolve, oldRoles
	})
}

func TestNotifyPublishesOneSubjectOnlyBusNotification(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	_, errOut, err := runNotifyCommand(t, "--as", "scheduler", "alice", "meet 3 — your turn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, StateUnverified) {
		t.Fatalf("receipt = %q", errOut)
	}
	events, err := room.Timeline(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("timeline = %+v, %v", events, err)
	}
	e := events[0]
	if e.Type != room.EventNotify || e.Principal != "scheduler" || e.To != "alice" || e.Body != "meet 3 — your turn" {
		t.Fatalf("notification = %+v", e)
	}
	if e.Topic != "" || e.Room != "" {
		t.Fatalf("notify invented parallel addressing fields: %+v", e)
	}
}

func TestNotifyAcceptsCanonicalToFlag(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	if _, _, err := runNotifyCommand(t, "--as", "scheduler", "--to", "alice", "gate finished"); err != nil {
		t.Fatal(err)
	}
	events, _ := room.Timeline(0)
	if len(events) != 1 || events[0].To != "alice" || events[0].Body != "gate finished" {
		t.Fatalf("--to notification = %+v", events)
	}
}

func TestNotifyRefusesOverlongSubjectWithoutTruncatingOrWriting(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	subject := strings.Repeat("x", MaxNotifySubjectBytes+1)
	_, _, err := runNotifyCommand(t, "--as", "scheduler", "alice", subject)
	if err == nil || !strings.Contains(err.Error(), "refused, not truncated") {
		t.Fatalf("overlong subject returned %v", err)
	}
	events, _ := room.Timeline(0)
	if len(events) != 0 {
		t.Fatalf("refused subject was written: %+v", events)
	}
}

func TestNotifyRefusesBodiesAndExtraOperands(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	for _, args := range [][]string{
		{"--as", "scheduler", "alice", "subject\nbody"},
		{"--as", "scheduler", "alice", "unquoted", "body"},
		{"--as", "scheduler", "--to", "alice", "alice", "subject"},
	} {
		if _, _, err := runNotifyCommand(t, args...); err == nil {
			t.Fatalf("notify %q accepted a body or duplicate address", args)
		}
	}
	events, _ := room.Timeline(0)
	if len(events) != 0 {
		t.Fatalf("invalid notification was written: %+v", events)
	}
}

func TestNotifyUnresolvableTargetReportsFailedAndWritesNothing(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	_, _, err := runNotifyCommand(t, "--as", "scheduler", "nobody", "gate finished")
	if err == nil || !strings.Contains(err.Error(), StateFailed) {
		t.Fatalf("unresolvable target returned %v", err)
	}
	events, _ := room.Timeline(0)
	if len(events) != 0 {
		t.Fatalf("failed target was written: %+v", events)
	}
}

func TestNotifyRequiresAnAttributedSender(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")
	_, _, err := runNotifyCommand(t, "alice", "gate finished")
	if err == nil || !strings.Contains(err.Error(), "sender identity is required") {
		t.Fatalf("unattributed notify returned %v", err)
	}
	events, _ := room.Timeline(0)
	if len(events) != 0 {
		t.Fatalf("unattributed notification was written: %+v", events)
	}
}

func TestNotifyRoleUsesStableAddressAndAcceptedReceipt(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	_, errOut, err := runNotifyCommand(t, "--as", "scheduler", "steward", "nightly backup failed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, StateAccepted) || !strings.Contains(errOut, "steward") {
		t.Fatalf("role receipt = %q", errOut)
	}
	events, _ := room.Timeline(0)
	if len(events) != 1 || events[0].To != "steward.host-1" {
		t.Fatalf("role notification = %+v", events)
	}
}

func TestNotifyReceiptUsesCanonicalQueuedAndJSONState(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}
	out, _, err := runNotifyCommand(t, "--as", "scheduler", "--json", "alice", "gate finished")
	if err != nil {
		t.Fatal(err)
	}
	var got NotifyReceipt
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("notify --json: %v\n%s", err, out)
	}
	if got.State != StateQueued || got.To != "alice" || got.Subject != "gate finished" || got.Principal != "scheduler" {
		t.Fatalf("receipt = %+v", got)
	}
}

func TestScheduleNotifyCapturesPrincipalInFireCommand(t *testing.T) {
	isolate(t)
	state := t.TempDir() + "/schedule.json"
	t.Setenv("BASHY_SCHEDULE_STATE", state)
	id, err := ScheduleNotify("scheduler", "alice", "wake up", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := schedule.NewStore(state).LoadJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, err=%v", jobs, err)
	}
	job := jobs[0]
	if job.ID != id || len(job.Command) < 4 || job.Command[1] != "notify" || job.Command[2] != "--as" || job.Command[3] != "scheduler" {
		t.Fatalf("scheduled command = %#v", job.Command)
	}
	if !job.EnvSet {
		t.Fatal("scheduled notification did not capture its environment")
	}
}

func TestNotifyResolvesARecipientKnownOnlyToTheBusCursor(t *testing.T) {
	isolate(t)
	withNotifyTargets(t)
	if err := writeCursor("bus-only", 0); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runNotifyCommand(t, "--as", "scheduler", "bus-only", "check the gate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, StateQueued) {
		t.Fatalf("bus-only receipt = %q", errOut)
	}
	events, _ := room.Timeline(0)
	if len(events) != 1 || events[0].To != "bus-only" {
		t.Fatalf("bus-only notification = %+v", events)
	}
}

func TestNotifyJSONFailureIsNonzeroAndMachineReadable(t *testing.T) {
	isolate(t)
	withNotifyTargets(t, "alice")
	_, errOut, err := runNotifyCommand(t, "--as", "scheduler", "--json", "nobody", "gate finished")
	if err == nil || !Reported(err) {
		t.Fatalf("JSON failure returned %v", err)
	}
	var got NotifyReceipt
	if uerr := json.Unmarshal([]byte(errOut), &got); uerr != nil {
		t.Fatalf("failure receipt is not JSON: %v\n%s", uerr, errOut)
	}
	if got.State != StateFailed || got.Error == "" {
		t.Fatalf("failure receipt = %+v", got)
	}
}
