package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/spf13/cobra"
)

func isolateResourceLifecycle(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "BASHY_") || strings.HasPrefix(key, "WEAVE_") {
			t.Setenv(key, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}
func resourceTestGate(t *testing.T, path string) *llmbudget.Gate {
	t.Helper()
	one := 1
	return llmbudget.New(llmbudget.Config{StatePath: path, Policy: &llmbudget.Policy{Version: 1, Constraints: []llmbudget.Constraint{{HostSlots: &one}}}})
}
func resourceReservationCount(t *testing.T, path string) int {
	t.Helper()
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return 0
	}
	if e != nil {
		t.Fatal(e)
	}
	var s struct {
		Reservations map[string]json.RawMessage `json:"reservations"`
	}
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	return len(s.Reservations)
}
func TestWeaveResourceAdmissionPressureAndIdentity(t *testing.T) {
	isolateResourceLifecycle(t)
	path := filepath.Join(t.TempDir(), "budget.json")
	g := resourceTestGate(t, path)
	h := WeaveResourceHooks{Budget: g, RequireHostCheck: true}
	if _, e := beginWeaveAdmission(context.Background(), h, WeaveResourceDemand{Run: "a"}); e == nil {
		t.Fatal("missing host observer admitted")
	}
	h.CheckHost = func(context.Context, WeaveResourceDemand) error { return errors.New("fixture pressure") }
	if _, e := beginWeaveAdmission(context.Background(), h, WeaveResourceDemand{Run: "a"}); e == nil {
		t.Fatal("pressure admitted")
	}
	if n := resourceReservationCount(t, path); n != 0 {
		t.Fatal("pressure reserved capacity", n)
	}
	h.CheckHost = nil
	h.RequireHostCheck = false
	h.LookupIdentity = func(context.Context, int) (string, error) { return "birth-a", nil }
	a, e := beginWeaveAdmission(context.Background(), h, WeaveResourceDemand{Run: "a"})
	if e != nil {
		t.Fatal(e)
	}
	it := &weaveItem{}
	weaveRecordAdmission(it, a)
	if it.WrapperStartID != "birth-a" || it.ResourceReservationID == "" {
		t.Fatal(it)
	}
	if _, e = beginWeaveAdmission(context.Background(), h, WeaveResourceDemand{Run: "b"}); e == nil {
		t.Fatal("competing run oversubscribed")
	}
	if e = a.finish(true, false); e != nil {
		t.Fatal(e)
	}
	if n := resourceReservationCount(t, path); n != 0 {
		t.Fatal("unlaunched claim retained", n)
	}
}
func TestWeaveResourceCancellationRetainsUncertainChild(t *testing.T) {
	isolateResourceLifecycle(t)
	path := filepath.Join(t.TempDir(), "budget.json")
	g := resourceTestGate(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	a, e := beginWeaveAdmission(ctx, WeaveResourceHooks{Budget: g}, WeaveResourceDemand{Run: "a"})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	if e = a.finish(false, true); e != nil {
		t.Fatal(e)
	}
	if n := resourceReservationCount(t, path); n != 1 {
		t.Fatal("uncertain child lost reservation", n)
	}
	if _, e = beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: g}, WeaveResourceDemand{Run: "b"}); e == nil {
		t.Fatal("dead owner implicitly released live work")
	}
}
func TestWeaveResourcePIDReuseAndOwnership(t *testing.T) {
	isolateResourceLifecycle(t)
	cmd := &cobra.Command{}
	WithWeaveResources(cmd, WeaveResourceHooks{LookupIdentity: func(context.Context, int) (string, error) { return "new", nil }})
	it := &weaveItem{ID: 1, WrapperPid: 123, WrapperStartID: "old", Owner: "alice"}
	if e := weaveVerifiedWrapper(cmd.Context(), it); e == nil {
		t.Fatal("PID reuse accepted")
	}
	it.WrapperStartID = ""
	if e := weaveVerifiedWrapper(cmd.Context(), it); e == nil {
		t.Fatal("legacy PID accepted")
	}
	t.Setenv("BASHY_AGENT_ID", "bob")
	if weaveControlOwned(t.TempDir(), &weaveQueue{}, it) {
		t.Fatal("competitor authority accepted")
	}
	t.Setenv("BASHY_AGENT_ID", "alice")
	if !weaveControlOwned(t.TempDir(), &weaveQueue{}, it) {
		t.Fatal("explicit owner refused")
	}
}
func TestWeaveResourceProcessHelper(t *testing.T) {
	mode := os.Getenv("WEAVE_RESOURCE_TEST_HELPER")
	if mode == "" {
		return
	}
	dir := os.Getenv("WEAVE_RESOURCE_TEST_DIR")
	lock, e := weaveRunLifecycleLock(dir, 1)
	if e != nil {
		os.Exit(31)
	}
	defer lock.Release()
	a, e := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: resourceTestGate(t, filepath.Join(dir, "budget.json"))}, WeaveResourceDemand{Run: "persistent-wrapper"})
	if e != nil {
		os.Exit(32)
	}
	if e = os.WriteFile(filepath.Join(dir, "ready"), []byte(a.request.ID), 0600); e != nil {
		os.Exit(33)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = os.Stat(filepath.Join(dir, "finish")); e == nil {
			if e = a.finish(true, true); e != nil {
				os.Exit(34)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(35)
}
func TestWeaveResourceMultiprocessOwnerCrash(t *testing.T) {
	isolateResourceLifecycle(t)
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWeaveResourceProcessHelper$")
	cmd.Env = append(os.Environ(), "WEAVE_RESOURCE_TEST_HELPER=wrapper", "WEAVE_RESOURCE_TEST_DIR="+dir)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(dir, "ready")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wrapper did not reserve")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l, e := weaveRunLifecycleLock(dir, 1); e == nil {
		l.Release()
		t.Fatal("second process entered active lifecycle")
	}
	g := resourceTestGate(t, filepath.Join(dir, "budget.json"))
	if _, e := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: g}, WeaveResourceDemand{Run: "competitor"}); e == nil {
		t.Fatal("second process oversubscribed")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	l, e := weaveRunLifecycleLock(dir, 1)
	if e != nil {
		t.Fatal("crashed lifecycle lock not released", e)
	}
	l.Release()
	if n := resourceReservationCount(t, filepath.Join(dir, "budget.json")); n != 1 {
		t.Fatal("crash silently freed capacity", n)
	}
	if _, e := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: g}, WeaveResourceDemand{Run: "competitor"}); e == nil {
		t.Fatal("crashed uncertain work admitted competitor")
	}
}
func TestWeaveResourceStartQueuesBeforeProvision(t *testing.T) {
	isolateResourceLifecycle(t)
	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	t.Chdir(repo)
	dir, e := weaveQueueDir(repo)
	if e != nil {
		t.Fatal(e)
	}
	q := &weaveQueue{Root: repo, Items: []*weaveItem{{ID: 1, State: "todo", Title: "fixture"}}}
	if e = saveWeaveQueue(dir, q); e != nil {
		t.Fatal(e)
	}
	cmd := NewWeaveCmd()
	WithWeaveResources(cmd, WeaveResourceHooks{RequireHostCheck: true, CheckHost: func(context.Context, WeaveResourceDemand) error { return errors.New("fixture pressure") }})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"start", "--run", "1", "--no-spawn"})
	if e = cmd.Execute(); e == nil || !strings.Contains(output.String(), "fixture pressure") {
		t.Fatal("start bypassed admission", e)
	}
	if _, e = os.Stat(filepath.Join(dir, "workspaces", "issue-1")); !os.IsNotExist(e) {
		t.Fatal("provisioned under pressure", e)
	}
	fresh, e := loadWeaveQueue(dir)
	if e != nil {
		t.Fatal(e)
	}
	if fresh.Items[0].State != "todo" || fresh.Items[0].WrapperPid != 0 {
		t.Fatal("queued run claimed active", fresh.Items[0])
	}
}

func TestWeaveResourceStartRefusesPriorUnverifiedClaim(t *testing.T) {
	isolateResourceLifecycle(t)
	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	t.Chdir(repo)
	dir, e := weaveQueueDir(repo)
	if e != nil {
		t.Fatal(e)
	}
	it := &weaveItem{ID: 1, State: "paused", Title: "fixture", ResourceReservationID: "prior-orphan"}
	if e = saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}}); e != nil {
		t.Fatal(e)
	}
	cmd := NewWeaveCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"start", "--run", "1", "--no-spawn"})
	if e = cmd.Execute(); e == nil || !strings.Contains(out.String(), "termination is unverified") {
		t.Fatal("unverified prior work restarted", e, out.String())
	}
	fresh, e := loadWeaveQueue(dir)
	if e != nil {
		t.Fatal(e)
	}
	if fresh.Items[0].ResourceReservationID != "prior-orphan" {
		t.Fatal("prior reservation identity overwritten")
	}
}
