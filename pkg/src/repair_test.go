package src_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/src"
)

const brokenHandler = "package atlas\nfunc Handle(s string) string { return s + \"!\" }\n"
const fixedHandler = "package atlas\nfunc Handle(s string) string { return s }\n"
const handlerTest = "package atlas\nimport \"testing\"\nfunc TestHandleKeepsInput(t *testing.T) { if got := Handle(\"ok\"); got != \"ok\" { t.Fatalf(\"got %q\", got) } }\n"

func repairFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	home := t.TempDir()
	_, _ = seedFixture(t, home)
	files := baseFiles()
	files["go.mod"] = "module example.com/core\n\ngo 1.24\n"
	files["pkg/atlas/atlas.go"] = brokenHandler
	sha := pushSeedCommit(t, home, files, "seed broken Go handler and contract")
	fetchAt(t, home, sha)
	if err := src.EnsureLocal(home, "yoke"); err != nil {
		t.Fatal(err)
	}
	dir := destOf(home)
	base := resolve(t, dir, "local")
	upstream := filepath.Join(home, "seed", "yoke.git")
	if err := src.SetUpstream(home, "yoke", upstream); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(home, "profile.json")
	b, _ := json.Marshal(map[string]any{"commands": []map[string]string{{"name": "go-all", "command": "GOWORK=off go test ./...", "dir": "."}}})
	if err := os.WriteFile(profile, b, 0600); err != nil {
		t.Fatal(err)
	}
	return home, dir, base, profile
}

func repairLedger(t *testing.T, home, kind, id string) string {
	t.Helper()
	path := filepath.Join(home, id+".jsonl")
	b, _ := json.Marshal(map[string]any{"id": id, "app": "core", "incident": "inc-1", "time": "2026-10-31T00:00:00Z", "rung": "code", "kind": kind, "result": "reported", "evidence": map[string]any{"status": 500, "body_tail": "handler changed reply", "target": "/handle"}})
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeFix(t *testing.T, wantRed bool) func(context.Context, string, string, string) error {
	t.Helper()
	return func(_ context.Context, dir, agent, brief string) error {
		if agent != "codex:coder" || !strings.Contains(brief, "handler changed reply") || !strings.Contains(brief, "Do not commit") {
			t.Errorf("delegate brief/agent: %q, %q", agent, brief)
		}
		writeWork(t, dir, "pkg/atlas/atlas_test.go", handlerTest)
		if wantRed {
			c := exec.Command("go", "test", "./...")
			c.Dir = dir
			c.Env = append(os.Environ(), "GOWORK=off")
			if out, err := c.CombinedOutput(); err == nil || !strings.Contains(string(out), "TestHandleKeepsInput") {
				t.Errorf("regression test did not start red: %v %s", err, out)
			}
		}
		writeWork(t, dir, "pkg/atlas/atlas.go", fixedHandler)
		return nil
	}
}

func TestRepairIntakeAndPipelineTempUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake reviewer uses sh")
	}
	t.Setenv("BASHY_PRINCIPAL", testPrincipal)
	home, dir, base, profile := repairFixture(t)
	bad := repairLedger(t, home, "config", "heal-not-code")
	if _, err := src.Repair(context.Background(), home, "yoke", src.RepairOptions{FromHeal: bad, Agent: "codex:coder"}); err == nil || !strings.Contains(err.Error(), "only kind code") {
		t.Fatalf("non-code intake: %v", err)
	}
	if got := resolve(t, dir, "local"); got != base {
		t.Fatalf("non-code intake moved local: %s", got)
	}

	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "bashy"), []byte("#!/bin/sh\necho '{\"outcome\":\"held\"}'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	ledger := repairLedger(t, home, "code", "heal-500-a")
	res, err := src.Repair(context.Background(), home, "yoke", src.RepairOptions{FromHeal: ledger, ID: "heal-500-a", Agent: "codex:coder", ReviewAgent: "claude:reviewer", Profile: profile, Release: true, Delegate: fakeFix(t, true), Runner: interpreter{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version == "" || res.PR.State != src.PRStateMerged || res.PR.Trigger != "heal-500-a" || res.PR.Reviewer != "claude:reviewer" || !res.PR.SuiteComplete {
		t.Fatalf("pipeline result: %+v", res)
	}
	upstream := filepath.Join(home, "seed", "yoke.git")
	refs, err := exec.Command("git", "ls-remote", upstream, "refs/heads/local", "refs/tags/"+res.Version).Output()
	if err != nil || !strings.Contains(string(refs), "refs/heads/local") || !strings.Contains(string(refs), "refs/tags/"+res.Version) {
		t.Fatalf("upstream refs: %v %s", err, refs)
	}
	bare, err := gogit.PlainOpen(upstream)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bare.Reference(plumbing.NewTagReferenceName(res.Version), true)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := bare.TagObject(ref.Hash())
	if err != nil || !strings.Contains(tag.Message, "Trigger: heal-500-a") || !strings.Contains(tag.Message, "PR: "+res.PR.ID) || !strings.Contains(tag.Message, "Reviewer: claude:reviewer") || !strings.Contains(tag.Message, "Suite-Evidence: "+profile) {
		t.Fatalf("release tag metadata: %v %+v", err, tag)
	}
	t.Setenv("BASHY_HOME", home)
	cmd := src.NewSrcCmd()
	var history bytes.Buffer
	cmd.SetOut(&history)
	cmd.SetArgs([]string{"log", "yoke"})
	if err := cmd.Execute(); err != nil || !strings.Contains(history.String(), "trigger=heal-500-a") || !strings.Contains(history.String(), "suite_evidence="+profile) || !strings.Contains(history.String(), "reviewer=claude:reviewer") {
		t.Fatalf("src log release history: %v %s", err, history.String())
	}
	if _, err := src.Revert(home, "yoke", true); err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, dir, "local"); got != base {
		t.Fatalf("revert local = %s, want %s", got, base)
	}
}

func TestRepairRefusalsDoNotMergeOrPush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake reviewer uses sh")
	}
	t.Setenv("BASHY_PRINCIPAL", testPrincipal)
	for _, tc := range []struct {
		name, reviewer string
		fix            bool
	}{{"same-vendor", "codex:reviewer", true}, {"failing-suite", "claude:reviewer", false}} {
		t.Run(tc.name, func(t *testing.T) {
			home, dir, base, profile := repairFixture(t)
			fake := t.TempDir()
			if err := os.WriteFile(filepath.Join(fake, "bashy"), []byte("#!/bin/sh\necho '{\"outcome\":\"held\"}'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
			ledger := repairLedger(t, home, "code", "heal-refuse")
			delegate := fakeFix(t, false)
			if !tc.fix {
				delegate = func(_ context.Context, dir, _, _ string) error {
					writeWork(t, dir, "pkg/atlas/atlas_test.go", handlerTest)
					return nil
				}
			}
			res, err := src.Repair(context.Background(), home, "yoke", src.RepairOptions{FromHeal: ledger, Agent: "codex:coder", ReviewAgent: tc.reviewer, Profile: profile, Release: true, Delegate: delegate, Runner: interpreter{}})
			if err == nil {
				t.Fatal("refusal pipeline passed")
			}
			if res.PR.State != src.PRStateOpen {
				t.Fatalf("PR state after refusal: %+v", res.PR)
			}
			if got := resolve(t, dir, "local"); got != base {
				t.Fatalf("local moved: %s", got)
			}
			upstream := filepath.Join(home, "seed", "yoke.git")
			if refs, e := exec.Command("git", "ls-remote", upstream, "refs/heads/local", "refs/tags/*").Output(); e != nil || len(refs) != 0 {
				t.Fatalf("upstream changed: %v %s", e, refs)
			}
		})
	}
}
