package src_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/src"
)

func TestCoreRepairPipelineLocalUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake pair reviewer uses sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "codex:coder")
	t.Setenv("BASHY_PRINCIPAL", testPrincipal)
	_, sha := seedFixture(t, home)
	fetchAt(t, home, sha)
	dir, base := shapeBase(t, home)
	pr := candidatePR(t, home, "repair-core", map[string]string{"pkg/atlas/atlas.go": "package atlas\n// fixed bug\n"})
	if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(err.Error(), "full-suite") || !strings.Contains(err.Error(), "review") || !strings.Contains(err.Error(), "sealed verify") {
		t.Fatalf("missing gates: %v", err)
	}
	if _, err := src.ReviewPR(home, pr.ID, "codex:other"); err == nil || !strings.Contains(err.Error(), "different-vendor") {
		t.Fatalf("same vendor: %v", err)
	}
	profile := filepath.Join(t.TempDir(), "profile.json")
	writeProfile := func(command string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"commands": []map[string]string{{"name": "all", "command": command, "dir": "."}}})
		if err := os.WriteFile(profile, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile("false")
	if _, err := src.RunPRSuite(home, pr.ID, profile); err == nil {
		t.Fatal("false suite passed")
	}
	writeProfile("kill -9 $$")
	if _, err := src.RunPRSuite(home, pr.ID, profile); err == nil {
		t.Fatal("killed suite passed")
	}
	writeProfile("true")
	if _, err := src.RunPRSuite(home, pr.ID, profile); err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(err.Error(), "review") {
		t.Fatalf("review gate: %v", err)
	}
	fake := t.TempDir()
	script := filepath.Join(fake, "bashy")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"outcome\":\"held\"}'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := src.ReviewPR(home, pr.ID, "claude:reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(err.Error(), "sealed verify") {
		t.Fatalf("verify gate: %v", err)
	}
	rep := runVerifier(t, home, dir, base, pr, testPrincipal)
	if rep.Record.Verdict != "pass" {
		t.Fatalf("verify: %s", rep.Record.Verdict)
	}
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ReleaseCore(home, "yoke", "incident-1"); err == nil || !strings.Contains(err.Error(), "configured upstream") {
		t.Fatalf("unconfigured push: %v", err)
	}
	upstream := filepath.Join(home, "seed", "yoke.git")
	if err := src.SetUpstream(home, "yoke", upstream); err != nil {
		t.Fatal(err)
	}
	v, err := src.ReleaseCore(home, "yoke", "incident-1")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := gogit.PlainOpen(upstream)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := bare.Reference(plumbing.NewTagReferenceName(v), true)
	if err != nil {
		t.Fatal(err)
	}
	if tag.Hash().IsZero() {
		t.Fatal("no tag")
	}
	branch, err := bare.Reference(plumbing.NewBranchReferenceName("local"), true)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Hash().String() != pr.HeadSHA {
		t.Fatalf("pushed branch %s, want %s", branch.Hash(), pr.HeadSHA)
	}
	tags, err := src.Releases(home, "yoke")
	if err != nil || len(tags) == 0 || tags[0].Version != v {
		t.Fatalf("version history: %v %v", tags, err)
	}
}

func TestConfiguredUpstreamOverridesSeed(t *testing.T) {
	home := t.TempDir()
	_, sha := seedFixture(t, home)
	t.Setenv("BASHY_SRC_COMMIT", sha)
	missing := filepath.Join(home, "missing.git")
	if err := src.SetUpstream(home, "yoke", missing); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fetch(home, "yoke"); err == nil {
		t.Fatal("fetch silently used seed instead of configured upstream")
	}
	seed := filepath.Join(home, "seed", "yoke.git")
	if err := src.SetUpstream(home, "yoke", seed); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatal(err)
	}
	got, err := src.ConfiguredUpstream(home, "yoke")
	if err != nil || got != seed {
		t.Fatalf("configured upstream = %q, %v", got, err)
	}
}

func TestSuiteEvidenceRejectsPartialAndTruncated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "codex:coder")
	_, sha := seedFixture(t, home)
	fetchAt(t, home, sha)
	pr := openSkillPR(t, home, "evidence-profile", "Suite evidence", "")
	path := filepath.Join(t.TempDir(), "evidence.json")
	for _, tc := range []struct {
		name      string
		complete  bool
		truncated bool
		pass      bool
	}{{"partial", false, false, false}, {"truncated", true, true, false}, {"complete", true, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"candidate": pr.HeadSHA, "complete": tc.complete, "results": []src.SuiteResult{{Name: "all", Command: "true", Status: "pass", ExitCode: 0, Complete: true, Truncated: tc.truncated}}})
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := src.RunPRSuite(home, pr.ID, path)
			if (err == nil) != tc.pass {
				t.Fatalf("pass=%v err=%v", tc.pass, err)
			}
		})
	}
}
