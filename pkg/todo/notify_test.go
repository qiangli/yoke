// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/issue"
)

func newTestStoreFunc(t *testing.T) storeFunc {
	t.Helper()
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	// The outer shell running `go test` may itself be an agent session with
	// BASHY_PRINCIPAL/WEAVE_AGENT set (this repo's own dev loop runs inside
	// one). Clear it so ResolveAuthoredActor falls through to the plain
	// login-name path these tests assume, instead of inheriting an ambient
	// identity that is not a registered fleet agent here.
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "")
	t.Setenv("WEAVE_AGENT", "")
	// Lifecycle gates deliberately run with a minimal environment that omits
	// USER and LOGNAME. Establish the human author this test exercises instead
	// of borrowing it from whichever shell happened to start `go test`.
	t.Setenv("USER", "steward")
	t.Setenv("LOGNAME", "steward")
	st, err := UserStore("steward")
	if err != nil {
		t.Fatal(err)
	}
	return func() (*issue.Store, string, error) { return st, "user steward", nil }
}

// TestAddAssigneeDeliversThroughTheExistingBus is the DIRECTION-1 fix for Cell
// A ("nothing tells bob"): assigning a task to a reachable reader must land a
// real notification in that reader's inbox — the same inbox `bashy inbox
// --as bob` drains — not merely print something reassuring on the CLI. The
// property under test is DELIVERY, read back through bus.UnreadNotifications
// (the same read path inbox itself uses), never the command's own stdout.
func TestAddAssigneeDeliversThroughTheExistingBus(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	pinTodoAgents(t, "bob")
	sf := newTestStoreFunc(t)

	// Give "bob" prior inbox evidence (an existing drain cursor), which is
	// what pkg/bus's own resolver treats as proof a reader is addressable —
	// the same ladder `bashy notify` uses (pkg/bus/notify.go
	// resolveNotifyTarget). Nothing about todo's own resolution is special.
	if err := bus.MarkNotificationsRead("bob", 0); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newAddCmd(sf)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"fix the thing", "--owner", "bob"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("todo add: %v", err)
	}

	events, _, err := bus.UnreadNotifications("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("bob's inbox has %d events, want exactly 1: %+v", len(events), events)
	}
	if !strings.Contains(events[0].Body, "fix the thing") {
		t.Errorf("notification body = %q, want it to name the task", events[0].Body)
	}
	if events[0].To != "bob" {
		t.Errorf("notification To = %q, want %q", events[0].To, "bob")
	}
}

// An assignee is an identity, not free text. Refuse the write before a todo can
// enter assigned-looking state with nobody behind the address.
func TestAddAssigneeMustBeARegisteredAgent(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	sf := newTestStoreFunc(t)

	var out, errOut bytes.Buffer
	cmd := newAddCmd(sf)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"herd the cats", "--owner", "nobody-registered-anywhere-zz"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "assignee") || !strings.Contains(err.Error(), "bashy agent list") {
		t.Fatalf("unregistered assignee error = %v", err)
	}
	st, _, storeErr := sf()
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	items, listErr := List(st, "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(items) != 0 {
		t.Fatalf("refused assignment persisted %d todos", len(items))
	}

	events, _, err := bus.UnreadNotifications("nobody-registered-anywhere-zz")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("unreachable assignee got %d events, want 0 (nothing to deliver)", len(events))
	}
}

// TestEditReassignNotifies covers the second write path: an item assigned
// after creation via `todo edit --owner` must notify exactly like `add`
// does, not just the assignee set at filing time.
func TestEditReassignNotifies(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	pinTodoAgents(t, "carol")
	sf := newTestStoreFunc(t)
	if err := bus.MarkNotificationsRead("carol", 0); err != nil {
		t.Fatal(err)
	}

	st, _, err := sf()
	if err != nil {
		t.Fatal(err)
	}
	it, err := Add(st, "unassigned at filing", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newEditCmd(sf)
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{it.ID, "--owner", "carol"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("todo edit: %v", err)
	}

	events, _, err := bus.UnreadNotifications("carol")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("carol's inbox has %d events, want exactly 1: %+v", len(events), events)
	}
}
