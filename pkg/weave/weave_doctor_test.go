package weave

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDoctorAgeThresholdsUseInjectedClock(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	thresholds := weaveDoctorThresholds{Todo: 7 * 24 * time.Hour, Submitted: 4 * time.Hour, Allocated: 30 * time.Minute}
	items := []*weaveItem{
		{ID: 1, State: "todo", Created: now.Add(-thresholds.Todo - time.Second)},
		{ID: 2, State: "todo", Created: now.Add(-thresholds.Todo + time.Second)},
		{ID: 3, State: "submitted", Created: now.Add(-30 * 24 * time.Hour), FinishedAt: now.Add(-thresholds.Submitted - time.Second)},
		{ID: 4, State: "submitted", Created: now.Add(-30 * 24 * time.Hour), FinishedAt: now.Add(-thresholds.Submitted + time.Second)},
		{ID: 5, State: "allocated", Created: now.Add(-thresholds.Allocated - time.Second)},
		{ID: 6, State: "allocated", Created: now.Add(-thresholds.Allocated + time.Second)},
	}
	rows := weaveDoctorOpenItems(&weaveQueue{Items: items}, thresholds, now)
	if len(rows) != len(items) {
		t.Fatalf("open rows = %d, want %d", len(rows), len(items))
	}
	for i, row := range rows {
		wantStale := i%2 == 0
		if got := hasDoctorFlag(row, "stale"); got != wantStale {
			t.Errorf("issue %d stale = %v, want %v (age=%ds flags=%v)", row.Issue, got, wantStale, row.AgeSeconds, row.Flags)
		}
	}
	if got, want := rows[3].AgeSeconds, int64((thresholds.Submitted-time.Second)/time.Second); got != want {
		t.Errorf("fresh submitted age = %d, want %d from FinishedAt (not Created)", got, want)
	}
}

func TestDoctorZeroThresholdDisablesCheckAndKeepsFlagsAlongsideStale(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	q := &weaveQueue{Items: []*weaveItem{
		{ID: 1, State: "todo", Created: now.Add(-365 * 24 * time.Hour)},
		{ID: 2, State: "submitted", Created: now.Add(-time.Hour), NeedsSteward: true},
	}}
	rows := weaveDoctorOpenItems(q, weaveDoctorThresholds{Submitted: 30 * time.Minute}, now)
	if hasDoctorFlag(rows[0], "stale") {
		t.Fatal("todo threshold 0 did not disable its stale check")
	}
	for _, flag := range []string{"needs-steward", "stale"} {
		if !hasDoctorFlag(rows[1], flag) {
			t.Errorf("submitted flags = %v, missing %q", rows[1].Flags, flag)
		}
	}
}

func TestWeaveListPrintsUnattendedBanner(t *testing.T) {
	dir, root := newQueueInTempRepo(t)
	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{{
		ID: 1, Title: "forgotten", State: "todo", Created: time.Now().UTC().Add(-8 * 24 * time.Hour),
	}}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newWeaveListCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("weave list: %v\n%s", err, out.String())
	}
	want := "ATTENTION: 1 unattended item(s) — see `weave doctor`"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("list output missing %q:\n%s", want, out.String())
	}
}

func hasDoctorFlag(row weaveOpenItem, want string) bool {
	for _, flag := range row.Flags {
		if flag == want {
			return true
		}
	}
	return false
}
