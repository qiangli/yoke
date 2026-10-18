package lens

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

// TestScriptPipeline_DeterministicOutput pins that valid pure-output skills
// produce byte-identical script extraction, artifact naming, sha256 digests,
// and stored files across multiple runs.
func TestScriptPipeline_DeterministicOutput(t *testing.T) {
	doc := []byte("# Status Reporter\n\nRuns a pure report.\n\n```bsh\necho 'system ready'\nprintf 'code=%d\n' 0\ntrue\npwd\n```\n\nFooter note.\n")

	wantScript := "echo 'system ready'\nprintf 'code=%d\n' 0\ntrue\npwd\n"
	sum := sha256.Sum256([]byte(wantScript))
	wantName := hex.EncodeToString(sum[:]) + ".sh"
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	// 1. Direct ExtractSkillScript check.
	extracted, err := ExtractSkillScript(doc)
	if err != nil {
		t.Fatalf("ExtractSkillScript failed: %v", err)
	}
	if string(extracted) != wantScript {
		t.Fatalf("ExtractSkillScript got %q, want %q", string(extracted), wantScript)
	}

	// 2. CompileAndValidateSkill check.
	validated, err := CompileAndValidateSkill(doc)
	if err != nil {
		t.Fatalf("CompileAndValidateSkill failed: %v", err)
	}
	if string(validated) != wantScript {
		t.Fatalf("CompileAndValidateSkill got %q, want %q", string(validated), wantScript)
	}

	// 3. CompileSkillScriptPipeline run 1.
	dir1 := t.TempDir()
	art1, err := CompileSkillScriptPipeline(dir1, doc)
	if err != nil {
		t.Fatalf("CompileSkillScriptPipeline run 1 failed: %v", err)
	}
	if art1.Name != wantName || art1.Digest != wantDigest || string(art1.Script) != wantScript {
		t.Fatalf("run 1 result mismatch: got (%q, %q, %q), want (%q, %q, %q)",
			art1.Name, art1.Digest, string(art1.Script), wantName, wantDigest, wantScript)
	}
	content1, err := os.ReadFile(art1.Path)
	if err != nil || string(content1) != wantScript {
		t.Fatalf("run 1 stored content: err=%v, content=%q", err, string(content1))
	}

	// 4. CompileSkillScriptPipeline run 2 (different dir).
	dir2 := t.TempDir()
	art2, err := CompileSkillScriptPipeline(dir2, doc)
	if err != nil {
		t.Fatalf("CompileSkillScriptPipeline run 2 failed: %v", err)
	}
	content2, err := os.ReadFile(art2.Path)
	if err != nil || string(content2) != wantScript {
		t.Fatalf("run 2 stored content: err=%v, content=%q", err, string(content2))
	}
	if art1.Name != art2.Name || art1.Digest != art2.Digest || string(content1) != string(content2) {
		t.Fatal("pipeline is not deterministic across runs")
	}

	// 5. Convenience wrappers return matching name and digest.
	name3, digest3, err3 := CompileSkillScript(dir1, doc)
	if err3 != nil || name3 != wantName || digest3 != wantDigest {
		t.Fatalf("CompileSkillScript: got (%q, %q, %v)", name3, digest3, err3)
	}
	name4, digest4, err4 := CompileScriptPipeline(dir1, doc)
	if err4 != nil || name4 != wantName || digest4 != wantDigest {
		t.Fatalf("CompileScriptPipeline: got (%q, %q, %v)", name4, digest4, err4)
	}
	name5, digest5, err5 := CompileScript(dir1, doc)
	if err5 != nil || name5 != wantName || digest5 != wantDigest {
		t.Fatalf("CompileScript: got (%q, %q, %v)", name5, digest5, err5)
	}
}

// TestScriptPipeline_RejectedAmbiguous tests that ambiguous or malformed skill
// markdown inputs fail-closed with ExitInvalid before storage or execution.
func TestScriptPipeline_RejectedAmbiguous(t *testing.T) {
	cases := []struct {
		name string
		in   string
		msg  string
	}{
		{
			name: "no fences",
			in:   "# Skill prose\n\nThere are no code blocks here.\n",
			msg:  "no ```bsh fence",
		},
		{
			name: "only non-shell fence",
			in:   "# Config\n\n```json\n{\"not\": \"shell\"}\n```\n",
			msg:  "no ```bsh fence",
		},
		{
			name: "unterminated fence",
			in:   "# Broken\n\n```bsh\necho hi\n",
			msg:  "unterminated",
		},
		{
			name: "multiple shell fences (bsh + bash)",
			in:   "```bsh\necho hi\n```\n\n```bash\necho there\n```\n",
			msg:  "exactly one shell fence",
		},
		{
			name: "multiple shell fences (sh + posix)",
			in:   "```sh\necho hi\n```\n\n```posix\necho there\n```\n",
			msg:  "exactly one shell fence",
		},
		{
			name: "empty script body",
			in:   "```bsh\n```\n",
			msg:  "empty script body",
		},
		{
			name: "whitespace-only script body",
			in:   "```bsh\n   \n\t\n```\n",
			msg:  "empty script body",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, _, err := CompileSkillScript(dir, []byte(tc.in))
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			wantExit(t, err, ExitInvalid)
			if !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("error %q does not contain expected substring %q", err.Error(), tc.msg)
			}

			// Verify no side effects: nothing was written to dir.
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("store dir has %d files after rejected input: %v", len(entries), entries)
			}
		})
	}
}

// TestScriptPipeline_RejectedUnsafe tests that shell scripts with unsafe
// constructs (non-pure output, redirections, pipelines, loops, etc.) fail-closed
// with ExitInvalid and are never stored or executed.
func TestScriptPipeline_RejectedUnsafe(t *testing.T) {
	cases := []struct {
		name string
		body string
		msg  string
	}{
		{
			name: "pipeline",
			body: "echo hi | wc -l\n",
			msg:  "pipeline",
		},
		{
			name: "redirection",
			body: "echo hi > /dev/null\n",
			msg:  "redirection",
		},
		{
			name: "command substitution",
			body: "echo $(whoami)\n",
			msg:  "command substitution",
		},
		{
			name: "backtick substitution",
			body: "echo `pwd`\n",
			msg:  "command substitution",
		},
		{
			name: "unauthorized command (rm)",
			body: "rm -rf /tmp/test\n",
			msg:  `unknown command "rm"`,
		},
		{
			name: "unauthorized command (curl)",
			body: "curl http://example.com\n",
			msg:  `unknown command "curl"`,
		},
		{
			name: "for loop",
			body: "for i in 1 2; do echo $i; done\n",
			msg:  "for loop",
		},
		{
			name: "while loop",
			body: "while true; do echo hi; done\n",
			msg:  "while loop",
		},
		{
			name: "if clause",
			body: "if true; then echo ok; fi\n",
			msg:  "if statement",
		},
		{
			name: "variable assignment",
			body: "FOO=bar\necho $FOO\n",
			msg:  "assignment",
		},
		{
			name: "background job",
			body: "echo bg &\n",
			msg:  "background",
		},
		{
			name: "syntax error",
			body: "echo 'unclosed string\n",
			msg:  "syntax error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "```bsh\n" + tc.body + "```\n"
			dir := t.TempDir()
			_, _, err := CompileSkillScript(dir, []byte(doc))
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			wantExit(t, err, ExitInvalid)
			if !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("error %q does not contain expected substring %q", err.Error(), tc.msg)
			}

			// Verify no side effects: nothing was written to dir.
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("store dir has %d files after rejected unsafe script", len(entries))
			}
		})
	}
}

// TestScriptPipeline_NoSideEffects explicitly verifies that compile/validate
// never executes the script body and never leaves temporary or sentinel files.
func TestScriptPipeline_NoSideEffects(t *testing.T) {
	sentinelDir := t.TempDir()
	sentinelFile := filepath.Join(sentinelDir, "sentinel.txt")

	// Script attempts to write a sentinel file if executed.
	hostileDoc := "```bsh\necho 'pwned' > " + sentinelFile + "\n```\n"
	storeDir := t.TempDir()

	_, _, err := CompileSkillScript(storeDir, []byte(hostileDoc))
	if err == nil {
		t.Fatal("expected hostile script to be rejected, but succeeded")
	}
	wantExit(t, err, ExitInvalid)

	// 1. Sentinel file must NOT exist (script was never executed).
	if _, statErr := os.Stat(sentinelFile); !os.IsNotExist(statErr) {
		t.Fatalf("hostile script was executed! sentinel file %s exists", sentinelFile)
	}

	// 2. Artifact store dir must remain completely empty.
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("store dir has %d files, expected 0", len(entries))
	}
}

// TestScriptPipeline_DigestReuse verifies that identical scripts reuse existing
// artifacts idempotently, while conflicting preexisting objects fail-closed.
func TestScriptPipeline_DigestReuse(t *testing.T) {
	doc := "```bsh\necho 'hello reuse'\npwd\n```\n"
	storeDir := t.TempDir()

	// First compile: writes new artifact.
	name1, digest1, err1 := CompileSkillScript(storeDir, []byte(doc))
	if err1 != nil {
		t.Fatalf("first compile failed: %v", err1)
	}
	path1 := filepath.Join(storeDir, name1)
	info1, err := os.Lstat(path1)
	if err != nil {
		t.Fatal(err)
	}

	// Second compile: reuses existing artifact without re-creating or failing.
	name2, digest2, err2 := CompileSkillScript(storeDir, []byte(doc))
	if err2 != nil {
		t.Fatalf("second compile failed: %v", err2)
	}
	if name1 != name2 || digest1 != digest2 {
		t.Fatalf("digest mismatch: (%q, %q) vs (%q, %q)", name1, digest1, name2, digest2)
	}
	info2, err := os.Lstat(path1)
	if err != nil {
		t.Fatal(err)
	}
	if info1.ModTime() != info2.ModTime() {
		t.Error("existing artifact was modified instead of reused")
	}

	// Preexisting colliding file: fails closed.
	collideDir := t.TempDir()
	collidePath := filepath.Join(collideDir, name1)
	if err := os.WriteFile(collidePath, []byte("different content\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, errCollide := CompileSkillScript(collideDir, []byte(doc))
	if errCollide == nil {
		t.Fatal("expected collision error, got nil")
	}
	wantExit(t, errCollide, ExitInvalid)
	if !strings.Contains(errCollide.Error(), "differing content") {
		t.Fatalf("expected 'differing content' error, got %v", errCollide)
	}

	// Preexisting symlink: fails closed.
	symlinkDir := t.TempDir()
	realTarget := filepath.Join(symlinkDir, "target.sh")
	if err := os.WriteFile(realTarget, []byte("echo 'hello reuse'\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(symlinkDir, name1)
	if err := os.Symlink(realTarget, symlinkPath); err != nil {
		t.Fatal(err)
	}
	_, _, errSymlink := CompileSkillScript(symlinkDir, []byte(doc))
	if errSymlink == nil {
		t.Fatal("expected symlink error, got nil")
	}
	wantExit(t, errSymlink, ExitInvalid)
	if !strings.Contains(errSymlink.Error(), "symlink") {
		t.Fatalf("expected 'symlink' error, got %v", errSymlink)
	}
}

// TestScriptPipeline_CustomValidator verifies that callers can provide a custom
// validator / effects checker through PipelineOptions.
func TestScriptPipeline_CustomValidator(t *testing.T) {
	doc := "```bsh\necho harmless\n```\n"
	storeDir := t.TempDir()

	customErr := errorf(ExitInvalid, "custom effect limit exceeded")
	opts := PipelineOptions{
		StoreDir: storeDir,
		Validator: func(script []byte) error {
			return customErr
		},
	}

	_, err := CompilePipeline([]byte(doc), opts)
	if err == nil {
		t.Fatal("expected custom validator error, got nil")
	}
	wantExit(t, err, ExitInvalid)
	if !strings.Contains(err.Error(), "custom effect limit exceeded") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify no file written on custom validator failure.
	entries, _ := os.ReadDir(storeDir)
	if len(entries) != 0 {
		t.Fatalf("store dir has %d files, expected 0", len(entries))
	}
}

// TestScriptPipeline_DeniedCaps verifies that missing or denied capabilities or effects
// fail closed with ExitInvalid BEFORE any worker execution or storage.
func TestScriptPipeline_DeniedCaps(t *testing.T) {
	docWithPwd := "```bsh\npwd\n```\n"
	docWithEcho := "```bsh\necho hello\n```\n"

	// 1. Script requires "read-only" capability; allowed caps does not contain it.
	t.Run("denied capability (missing from allowed)", func(t *testing.T) {
		storeDir := t.TempDir()
		opts := PipelineOptions{
			StoreDir:    storeDir,
			AllowedCaps: []string{"json"}, // missing "read-only"
		}
		_, err := PlanPipeline([]byte(docWithEcho), opts)
		if err == nil {
			t.Fatal("expected denied capability error, got nil")
		}
		wantExit(t, err, ExitInvalid)
		if !strings.Contains(err.Error(), `capability "read-only" which is denied`) {
			t.Fatalf("unexpected error message: %v", err)
		}
		// Verify no files written.
		entries, _ := os.ReadDir(storeDir)
		if len(entries) != 0 {
			t.Fatalf("expected 0 files, got %d", len(entries))
		}
	})

	// 2. Allowed caps is empty slice: all caps are denied.
	t.Run("empty allowed caps denies all", func(t *testing.T) {
		storeDir := t.TempDir()
		opts := PipelineOptions{
			StoreDir:    storeDir,
			AllowedCaps: []string{},
		}
		_, err := PlanPipeline([]byte(docWithEcho), opts)
		if err == nil {
			t.Fatal("expected denied capability error, got nil")
		}
		wantExit(t, err, ExitInvalid)
		if !strings.Contains(err.Error(), `capability "read-only" which is denied`) {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	// 3. Explicitly denied capability in DeniedCaps.
	t.Run("explicitly denied capability", func(t *testing.T) {
		storeDir := t.TempDir()
		opts := PipelineOptions{
			StoreDir:   storeDir,
			DeniedCaps: []string{"read-only"},
		}
		_, err := PlanPipeline([]byte(docWithEcho), opts)
		if err == nil {
			t.Fatal("expected denied capability error, got nil")
		}
		wantExit(t, err, ExitInvalid)
		if !strings.Contains(err.Error(), `script requires denied capability "read-only"`) {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	// 4. Allowed effects: script with pwd needs "read", allowed is only "pure".
	t.Run("denied effect under AllowedEffects", func(t *testing.T) {
		storeDir := t.TempDir()
		opts := PipelineOptions{
			StoreDir:       storeDir,
			AllowedEffects: []string{atlas.EffPure},
		}
		_, err := PlanPipeline([]byte(docWithPwd), opts)
		if err == nil {
			t.Fatal("expected denied effect error, got nil")
		}
		wantExit(t, err, ExitInvalid)
		if !strings.Contains(err.Error(), `script requires effect "read" which is denied`) {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	// 5. Guard check using advice.Cap: guard only allows "pure", script uses pwd (effect "read").
	t.Run("denied effect under advice.Cap guard", func(t *testing.T) {
		storeDir := t.TempDir()
		guard, err := advice.ParseCap(atlas.EffPure)
		if err != nil {
			t.Fatal(err)
		}
		opts := PipelineOptions{
			StoreDir: storeDir,
			Guard:    &guard,
		}
		_, err = PlanPipeline([]byte(docWithPwd), opts)
		if err == nil {
			t.Fatal("expected guard denial error, got nil")
		}
		wantExit(t, err, ExitInvalid)
		if !strings.Contains(err.Error(), "guard denied effects [read]") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	// 6. RequireCaps is true but AllowedCaps is nil.
	t.Run("require caps fails closed if unset", func(t *testing.T) {
		storeDir := t.TempDir()
		opts := PipelineOptions{
			StoreDir:    storeDir,
			RequireCaps: true,
		}
		_, err := PlanPipeline([]byte(docWithEcho), opts)
		if err == nil {
			t.Fatal("expected missing caps error, got nil")
		}
		wantExit(t, err, ExitInvalid)
		if !strings.Contains(err.Error(), "missing required capability declaration") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	// 7. Allowed caps matches: succeeds.
	t.Run("allowed caps satisfied succeeds", func(t *testing.T) {
		storeDir := t.TempDir()
		opts := PipelineOptions{
			StoreDir:       storeDir,
			AllowedCaps:    []string{atlas.CapReadOnly},
			AllowedEffects: []string{atlas.EffPure},
		}
		plan, err := PlanPipeline([]byte(docWithEcho), opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Contains(plan.Capabilities, atlas.CapReadOnly) {
			t.Errorf("plan missing read-only capability: %v", plan.Capabilities)
		}
	})
}

// TestScriptPipeline_DryRunNoSideEffects verifies that dry-run planning performs
// full extraction, validation, analysis, and digest calculation without creating
// any files or executing any script commands.
func TestScriptPipeline_DryRunNoSideEffects(t *testing.T) {
	sentinelDir := t.TempDir()
	sentinelFile := filepath.Join(sentinelDir, "ran.txt")
	storeDir := filepath.Join(t.TempDir(), "artifacts")

	doc := "```bsh\necho 'dry run test'\npwd\n```\n"

	plan, err := DryRunPipeline([]byte(doc), PipelineOptions{
		StoreDir: storeDir,
	})
	if err != nil {
		t.Fatalf("DryRunPipeline failed: %v", err)
	}

	if !plan.DryRun {
		t.Error("plan.DryRun should be true")
	}
	if plan.Artifact == nil || plan.Artifact.Name == "" || plan.Artifact.Digest == "" {
		t.Fatalf("plan artifact details missing: %+v", plan.Artifact)
	}
	if !slices.Contains(plan.Capabilities, atlas.CapReadOnly) {
		t.Errorf("plan capabilities %v missing read-only", plan.Capabilities)
	}
	if !slices.Contains(plan.Effects, atlas.EffRead) {
		t.Errorf("plan effects %v missing read", plan.Effects)
	}

	// Verify NO files exist in storeDir (storeDir should not even have been created or should be empty).
	if entries, err := os.ReadDir(storeDir); err == nil && len(entries) > 0 {
		t.Fatalf("storeDir has %d files after dry-run, expected 0", len(entries))
	}

	// Hostile script in dry-run mode: must be rejected with zero execution.
	hostileDoc := "```bsh\necho 'pwned' > " + sentinelFile + "\n```\n"
	_, hostileErr := DryRunPipeline([]byte(hostileDoc), PipelineOptions{StoreDir: storeDir})
	if hostileErr == nil {
		t.Fatal("expected hostile script to be rejected in dry-run, got nil")
	}
	wantExit(t, hostileErr, ExitInvalid)

	// Sentinel file must NOT exist.
	if _, err := os.Stat(sentinelFile); !os.IsNotExist(err) {
		t.Fatalf("sentinel file was created! Script was executed: %s", sentinelFile)
	}
}

// TestScriptPipeline_DigestVerification verifies that dry-run and planning
// verify digests against preexisting artifacts in the store directory.
func TestScriptPipeline_DigestVerification(t *testing.T) {
	doc := "```bsh\necho 'verify digest'\npwd\n```\n"
	storeDir := t.TempDir()

	// First: write valid artifact using non-dry-run compile.
	art, err := CompileSkillScriptPipeline(storeDir, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}

	// 1. DryRun against matching existing file: succeeds and verifies digest.
	plan, err := DryRunPipeline([]byte(doc), PipelineOptions{StoreDir: storeDir})
	if err != nil {
		t.Fatalf("dry-run against valid existing artifact failed: %v", err)
	}
	if plan.Artifact.Digest != art.Digest || plan.Artifact.Name != art.Name {
		t.Fatalf("digest mismatch in dry-run: %s vs %s", plan.Artifact.Digest, art.Digest)
	}

	// 2. DryRun against corrupted/differing file at the artifact path: fails closed.
	badStoreDir := t.TempDir()
	badPath := filepath.Join(badStoreDir, art.Name)
	if err := os.WriteFile(badPath, []byte("tampered content\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, badErr := DryRunPipeline([]byte(doc), PipelineOptions{StoreDir: badStoreDir})
	if badErr == nil {
		t.Fatal("expected dry-run error on tampered existing artifact, got nil")
	}
	wantExit(t, badErr, ExitInvalid)
	if !strings.Contains(badErr.Error(), "differing content") {
		t.Fatalf("expected 'differing content' error, got %v", badErr)
	}

	// 3. DryRun against symlink at the artifact path: fails closed.
	symlinkStoreDir := t.TempDir()
	realTarget := filepath.Join(symlinkStoreDir, "real.sh")
	if err := os.WriteFile(realTarget, art.Script, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(symlinkStoreDir, art.Name)
	if err := os.Symlink(realTarget, symlinkPath); err != nil {
		t.Fatal(err)
	}
	_, symErr := DryRunPipeline([]byte(doc), PipelineOptions{StoreDir: symlinkStoreDir})
	if symErr == nil {
		t.Fatal("expected dry-run error on symlink artifact, got nil")
	}
	wantExit(t, symErr, ExitInvalid)
	if !strings.Contains(symErr.Error(), "symlink") {
		t.Fatalf("expected 'symlink' error, got %v", symErr)
	}
}

// TestScriptPipeline_Explain verifies the human-readable and JSON explain output
// format of the ScriptPlan.
func TestScriptPipeline_Explain(t *testing.T) {
	doc := "```bsh\necho 'system ready'\npwd\n```\n"
	storeDir := t.TempDir()

	plan, err := DryRunPipeline([]byte(doc), PipelineOptions{StoreDir: storeDir})
	if err != nil {
		t.Fatal(err)
	}

	exp := plan.Explain()
	for _, expected := range []string{
		"artifact: ",
		"digest: sha256:",
		"required_caps: read-only",
		"effects: read",
		"commands: echo, pwd",
		"reversibility: readonly",
		"execution: zero-execution dry-run (no worker executed)",
	} {
		if !strings.Contains(exp, expected) {
			t.Errorf("Explain() output %q does not contain %q", exp, expected)
		}
	}

	jsonBytes, err := plan.ExplainJSON()
	if err != nil {
		t.Fatalf("ExplainJSON failed: %v", err)
	}
	var unmarshaled map[string]any
	if err := json.Unmarshal(jsonBytes, &unmarshaled); err != nil {
		t.Fatalf("invalid JSON from ExplainJSON: %v", err)
	}
	if unmarshaled["artifact"] == nil || unmarshaled["capabilities"] == nil || unmarshaled["effects"] == nil {
		t.Fatalf("missing required JSON keys in %s", string(jsonBytes))
	}
}
