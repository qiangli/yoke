package lens

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestScriptArtifact pins the deterministic content-addressed script artifact store
// (Sprint 446 S22): compiled script bytes turn into an atomic write under caller-provided
// directory, reuse matching bytes, reject colliding content or symlinks fail-closed,
// and return the sha256 digest. Scripts are never executed and no effects are inferred.
func TestScriptArtifact(t *testing.T) {
	script := []byte("#!/bin/sh\nset -e\necho 'hello world'\n")
	sum := sha256.Sum256(script)
	hexsum := hex.EncodeToString(sum[:])
	wantName := hexsum + ".sh"
	wantDigest := "sha256:" + hexsum

	// 1. Deterministic pure helpers.
	if got := ScriptArtifactName(script); got != wantName {
		t.Fatalf("ScriptArtifactName = %q, want %q", got, wantName)
	}
	if got := ScriptArtifactDigest(script); got != wantDigest {
		t.Fatalf("ScriptArtifactDigest = %q, want %q", got, wantDigest)
	}

	// 2. Initial atomic write in a fresh directory.
	dir := t.TempDir()
	name, digest, err := StoreScriptArtifact(dir, script)
	if err != nil {
		t.Fatalf("StoreScriptArtifact: unexpected error: %v", err)
	}
	if name != wantName {
		t.Errorf("name = %q, want %q", name, wantName)
	}
	if digest != wantDigest {
		t.Errorf("digest = %q, want %q", digest, wantDigest)
	}

	target := filepath.Join(dir, name)
	if gotPath := ScriptArtifactPath(dir, script); gotPath != target {
		t.Errorf("ScriptArtifactPath = %q, want %q", gotPath, target)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read stored artifact: %v", err)
	}
	if string(data) != string(script) {
		t.Fatalf("stored content = %q, want %q", string(data), string(script))
	}

	// Verify file mode permissions include executable bit.
	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat stored artifact: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("stored artifact mode %v is not executable", fi.Mode())
	}

	// 3. Idempotency: reuses matching preexisting bytes without rewriting.
	name2, digest2, err2 := StoreScriptArtifact(dir, script)
	if err2 != nil {
		t.Fatalf("StoreScriptArtifact (repeat): unexpected error: %v", err2)
	}
	if name2 != wantName || digest2 != wantDigest {
		t.Fatalf("StoreScriptArtifact (repeat): got (%q, %q), want (%q, %q)", name2, digest2, wantName, wantDigest)
	}

	// 4. Aliases: ScriptArtifact and WriteScriptArtifact behave identically.
	nameAlias, digestAlias, errAlias := ScriptArtifact(dir, script)
	if errAlias != nil || nameAlias != wantName || digestAlias != wantDigest {
		t.Fatalf("ScriptArtifact alias failed: got (%q, %q, %v)", nameAlias, digestAlias, errAlias)
	}
	nameAlias2, digestAlias2, errAlias2 := WriteScriptArtifact(dir, script)
	if errAlias2 != nil || nameAlias2 != wantName || digestAlias2 != wantDigest {
		t.Fatalf("WriteScriptArtifact alias failed: got (%q, %q, %v)", nameAlias2, digestAlias2, errAlias2)
	}

	// 5. Rejection: preexisting regular file with different content fails closed.
	collideDir := t.TempDir()
	collidePath := filepath.Join(collideDir, wantName)
	if err := os.WriteFile(collidePath, []byte("different body content\n"), 0o755); err != nil {
		t.Fatalf("write collide file: %v", err)
	}
	_, _, errCollide := StoreScriptArtifact(collideDir, script)
	if errCollide == nil {
		t.Fatal("StoreScriptArtifact on differing content: want error, got nil")
	}
	wantExit(t, errCollide, ExitInvalid)
	if !strings.Contains(errCollide.Error(), "differing content") {
		t.Errorf("collide error %q should mention differing content", errCollide.Error())
	}
	// Verify original file was not overwritten.
	afterCollide, _ := os.ReadFile(collidePath)
	if string(afterCollide) != "different body content\n" {
		t.Fatal("preexisting colliding file was modified")
	}

	// 6. Rejection: preexisting symlink is rejected fail-closed, even if target matches.
	symlinkDir := t.TempDir()
	realFile := filepath.Join(symlinkDir, "real_script.sh")
	if err := os.WriteFile(realFile, script, 0o755); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	symlinkPath := filepath.Join(symlinkDir, wantName)
	if err := os.Symlink(realFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	_, _, errSymlink := StoreScriptArtifact(symlinkDir, script)
	if errSymlink == nil {
		t.Fatal("StoreScriptArtifact on symlink with matching target: want error, got nil")
	}
	wantExit(t, errSymlink, ExitInvalid)
	if !strings.Contains(errSymlink.Error(), "symlink") {
		t.Errorf("symlink error %q should mention symlink", errSymlink.Error())
	}

	// Dangling symlink also rejected.
	danglingDir := t.TempDir()
	danglingSymlink := filepath.Join(danglingDir, wantName)
	if err := os.Symlink(filepath.Join(danglingDir, "nonexistent"), danglingSymlink); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}
	_, _, errDangling := StoreScriptArtifact(danglingDir, script)
	if errDangling == nil {
		t.Fatal("StoreScriptArtifact on dangling symlink: want error, got nil")
	}
	wantExit(t, errDangling, ExitInvalid)

	// 7. Rejection: preexisting directory at artifact path fails closed.
	subDirColl := t.TempDir()
	if err := os.Mkdir(filepath.Join(subDirColl, wantName), 0o755); err != nil {
		t.Fatalf("mkdir directory: %v", err)
	}
	_, _, errDir := StoreScriptArtifact(subDirColl, script)
	if errDir == nil {
		t.Fatal("StoreScriptArtifact on preexisting directory: want error, got nil")
	}
	wantExit(t, errDir, ExitInvalid)

	// 8. Rejection: empty or whitespace-only dir fails with ExitUsage.
	for _, badDir := range []string{"", "   ", "\t\n"} {
		_, _, errBadDir := StoreScriptArtifact(badDir, script)
		if errBadDir == nil {
			t.Errorf("dir %q: want error, got nil", badDir)
			continue
		}
		wantExit(t, errBadDir, ExitUsage)
	}

	// 9. Auto-create nested caller directory.
	nestedDir := filepath.Join(t.TempDir(), "deep", "nested", "store")
	nameNested, digestNested, errNested := StoreScriptArtifact(nestedDir, script)
	if errNested != nil {
		t.Fatalf("StoreScriptArtifact into nested dir: %v", errNested)
	}
	if nameNested != wantName || digestNested != wantDigest {
		t.Fatalf("nested dir result mismatch: (%q, %q)", nameNested, digestNested)
	}
	if _, err := os.Stat(filepath.Join(nestedDir, wantName)); err != nil {
		t.Fatalf("nested artifact not created: %v", err)
	}

	// 10. Empty script bytes.
	emptyScript := []byte("")
	emptySum := sha256.Sum256(emptyScript)
	wantEmptyName := hex.EncodeToString(emptySum[:]) + ".sh"
	wantEmptyDigest := "sha256:" + hex.EncodeToString(emptySum[:])
	emptyDir := t.TempDir()
	nameEmpty, digestEmpty, errEmpty := StoreScriptArtifact(emptyDir, emptyScript)
	if errEmpty != nil {
		t.Fatalf("StoreScriptArtifact(empty): %v", errEmpty)
	}
	if nameEmpty != wantEmptyName || digestEmpty != wantEmptyDigest {
		t.Fatalf("empty script result mismatch: got (%q, %q), want (%q, %q)", nameEmpty, digestEmpty, wantEmptyName, wantEmptyDigest)
	}
	emptyData, err := os.ReadFile(filepath.Join(emptyDir, nameEmpty))
	if err != nil || len(emptyData) != 0 {
		t.Fatalf("empty artifact file content: len=%d err=%v", len(emptyData), err)
	}

	// 11. Concurrent writers converge safely.
	concDir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, d, e := StoreScriptArtifact(concDir, script)
			if e != nil {
				errs <- e
				return
			}
			if n != wantName || d != wantDigest {
				errs <- errorf(ExitInvalid, "unexpected name/digest (%q, %q)", n, d)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent StoreScriptArtifact error: %v", e)
	}

	// 12. Purity: never executes scripts.
	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	harmfulScript := []byte("echo pwned > " + sentinel + "\n")
	harmfulDir := t.TempDir()
	_, _, errHarmful := StoreScriptArtifact(harmfulDir, harmfulScript)
	if errHarmful != nil {
		t.Fatalf("storing harmless text failed: %v", errHarmful)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatalf("script was executed during artifact storage: sentinel %v exists", sentinel)
	}
}
