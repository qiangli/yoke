// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package todo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
)

func TestKnownStatusPredicatesAndExistingUnchanged(t *testing.T) {
	if !ValidStatus(StatusKnown) {
		t.Fatal("StatusKnown must be a valid status")
	}
	for status, want := range map[string]struct {
		closed bool
		open   bool
	}{
		StatusTodo:         {closed: false, open: true},
		StatusAssigned:     {closed: false, open: true},
		StatusDoing:        {closed: false, open: true},
		StatusBlocked:      {closed: false, open: true},
		StatusKnown:        {closed: false, open: false},
		StatusDone:         {closed: true, open: false},
		StatusWontfix:      {closed: true, open: false},
		issue.StatusClosed: {closed: true, open: false},
	} {
		if got := IsClosed(status); got != want.closed {
			t.Errorf("IsClosed(%q) = %v, want %v", status, got, want.closed)
		}
		if got := IsOpen(status); got != want.open {
			t.Errorf("IsOpen(%q) = %v, want %v", status, got, want.open)
		}
	}
}

func TestIsExpiredAt(t *testing.T) {
	now := time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		it   *issue.Issue
		want bool
	}{
		{name: "past date", it: &issue.Issue{Status: StatusKnown, Expires: "2026-11-24"}, want: true},
		{name: "same day not yet expired", it: &issue.Issue{Status: StatusKnown, Expires: "2026-11-25"}, want: false},
		{name: "future date", it: &issue.Issue{Status: StatusKnown, Expires: "2026-12-25"}, want: false},
		{name: "past rfc3339", it: &issue.Issue{Status: StatusKnown, Expires: "2026-11-25T17:59:00Z"}, want: true},
		{name: "future rfc3339", it: &issue.Issue{Status: StatusKnown, Expires: "2026-11-25T19:00:00Z"}, want: false},
		{name: "leading iso date with note", it: &issue.Issue{Status: StatusKnown, Expires: "2026-11-24 (next release)"}, want: true},
		{name: "empty expires", it: &issue.Issue{Status: StatusKnown}, want: false},
		{name: "non-known status never expires", it: &issue.Issue{Status: StatusDone, Expires: "2020-01-01"}, want: false},
		{name: "todo status never expires", it: &issue.Issue{Status: StatusTodo, Expires: "2020-01-01"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsExpiredAt(tc.it, now); got != tc.want {
				t.Errorf("IsExpiredAt(%+v) = %v, want %v", tc.it, got, tc.want)
			}
		})
	}
}

func TestParseExpires(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if got, err := parseExpiresAt("+30d", now); err != nil || got != "2026-12-25" {
		t.Fatalf("parseExpiresAt(+30d) = %q, %v; want 2026-12-25", got, err)
	}
	if got, err := parseExpiresAt("2026-12-15", now); err != nil || got != "2026-12-15" {
		t.Fatalf("parseExpiresAt(2026-12-15) = %q, %v", got, err)
	}
	if got, err := parseExpiresAt("", now); err != nil || got != "" {
		t.Fatalf("parseExpiresAt('') = %q, %v", got, err)
	}
	if _, err := parseExpiresAt("not-a-date", now); err == nil {
		t.Fatal("invalid expires must be rejected")
	}
}

func TestKnownIssueCreateListShowAndExpiryFlagged(t *testing.T) {
	base := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := NewTodoCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--base-dir", base}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("todo %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	// 1. Open item + active known item + expired known item.
	run("add", "open work item", "--no-sprint")

	futureDate := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	pastDate := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")

	addOut := run("add", "windows chrome first-nav abort", "--no-sprint",
		"--status", "known",
		"--workaround", "retry first navigation once after startup settles",
		"--expires", futureDate,
		"--json")
	var added map[string]any
	if err := json.Unmarshal([]byte(addOut), &added); err != nil {
		t.Fatalf("parse add --json: %v\n%s", err, addOut)
	}
	if added["status"] != StatusKnown || added["expires"] != futureDate {
		t.Fatalf("add --json = %+v", added)
	}
	activeID := added["id"].(string)

	// Move a wontfix sprint story into known via `todo status --workaround --expires`.
	st := RepoStore(base)
	wontfixStory, err := Add(st, "expired known issue", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	wontfixStory.Sprint = 708
	if _, err := st.Save(wontfixStory); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(st, wontfixStory.ID, StatusWontfix); err != nil {
		t.Fatal(err)
	}
	run("status", wontfixStory.ID, "known",
		"--workaround", "use alternate path until fixed",
		"--expires", pastDate)

	reloaded, err := ResolveRef(st, wontfixStory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusKnown || reloaded.Closed != nil {
		t.Fatalf("wontfix -> known: status=%q closed=%v", reloaded.Status, reloaded.Closed)
	}
	if reloaded.Workaround != "use alternate path until fixed" || reloaded.Expires != pastDate {
		t.Fatalf("reloaded known fields: workaround=%q expires=%q", reloaded.Workaround, reloaded.Expires)
	}

	// 2. Default `todo list` excludes known issues (not counted as open work).
	openJSON := run("list", "--json")
	var openEnv envelopeShape
	if err := json.Unmarshal([]byte(openJSON), &openEnv); err != nil {
		t.Fatalf("parse open list: %v\n%s", err, openJSON)
	}
	if openEnv.Result.Count != 1 || len(openEnv.Result.Items) != 1 || openEnv.Result.Items[0].Title != "open work item" {
		t.Fatalf("default open list must exclude known items, got: %s", openJSON)
	}

	// 3. `todo list --known` and `--status known` list both known items and flag ONLY the expired one.
	knownText := run("list", "--known")
	if !strings.Contains(knownText, "windows chrome first-nav abort") || !strings.Contains(knownText, "expired known issue") {
		t.Fatalf("list --known missing known items:\n%s", knownText)
	}
	if strings.Contains(knownText, "open work item") {
		t.Fatalf("list --known leaked open item:\n%s", knownText)
	}
	if n := strings.Count(knownText, "(EXPIRED)"); n != 1 {
		t.Fatalf("list --known should flag exactly 1 expired item, got %d:\n%s", n, knownText)
	}

	knownJSON := run("list", "--status", "known", "--json")
	var knownEnv envelopeShape
	if err := json.Unmarshal([]byte(knownJSON), &knownEnv); err != nil {
		t.Fatalf("parse known list JSON: %v\n%s", err, knownJSON)
	}
	if knownEnv.Result.Count != 2 || len(knownEnv.Result.Items) != 2 {
		t.Fatalf("known list JSON count = %d, want 2:\n%s", knownEnv.Result.Count, knownJSON)
	}
	byID := map[string]listItem{}
	for _, it := range knownEnv.Result.Items {
		byID[it.ID] = it
	}
	if got := byID[activeID]; got.State != StatusKnown || got.Expired || got.Expires != futureDate || got.Workaround == "" {
		t.Fatalf("active known item wrong: %+v", got)
	}
	if got := byID[wontfixStory.ID]; got.State != StatusKnown || !got.Expired || got.Expires != pastDate || got.Workaround == "" {
		t.Fatalf("expired known item wrong: %+v", got)
	}

	// 4. `todo show` displays workaround, expires, and flags expiry in both text and JSON.
	showExpired := run("show", wontfixStory.ID)
	if !strings.Contains(showExpired, "status    known") ||
		!strings.Contains(showExpired, "workaround use alternate path until fixed") ||
		!strings.Contains(showExpired, pastDate+" (EXPIRED)") {
		t.Fatalf("show expired known item missing fields:\n%s", showExpired)
	}
	showActive := run("show", activeID)
	if strings.Contains(showActive, "(EXPIRED)") || !strings.Contains(showActive, futureDate) {
		t.Fatalf("show active known item wrong:\n%s", showActive)
	}
	showExpiredJSON := run("show", wontfixStory.ID, "--json")
	var sj map[string]any
	if err := json.Unmarshal([]byte(showExpiredJSON), &sj); err != nil {
		t.Fatalf("parse show --json: %v", err)
	}
	if sj["expired"] != true || sj["expires"] != pastDate || sj["workaround"] != "use alternate path until fixed" {
		t.Fatalf("show --json wrong: %+v", sj)
	}

	// 5. `todo edit` updates workaround and expires; `list --all` includes all 3.
	newExp := time.Now().UTC().AddDate(0, 0, 14).Format("2006-01-02")
	run("edit", wontfixStory.ID, "--expires", newExp, "--workaround", "updated workaround")
	updated, err := ResolveRef(st, wontfixStory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Expires != newExp || updated.Workaround != "updated workaround" || IsExpired(updated) {
		t.Fatalf("edited known item wrong: %+v", updated)
	}
	allJSON := run("list", "--all", "--json")
	var allEnv envelopeShape
	if err := json.Unmarshal([]byte(allJSON), &allEnv); err != nil {
		t.Fatal(err)
	}
	if allEnv.Result.Count != 3 {
		t.Fatalf("list --all count = %d, want 3", allEnv.Result.Count)
	}

	// 6. Conflicting flags rejected.
	for _, bad := range [][]string{{"list", "--known", "--all"}, {"list", "--known", "--status", "todo"}} {
		cmd := NewTodoCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--base-dir", base}, bad...))
		if err := cmd.Execute(); err == nil {
			t.Errorf("todo %v should fail", bad)
		}
	}
}
