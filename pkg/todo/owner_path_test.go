package todo

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerSubdirWindowsUnsafeIdentity(t *testing.T) {
	seen := map[string]string{}
	for _, owner := range []string{"cloud:alice@example.test", "CLOUD:alice@example.test", "cloud-alice@example.test?", `team\alice`, "team/alice", "../alice", "CON", "con.txt", "CON .txt", "LPT9", "COM¹", "CONIN$", "name.", "name ", "name\x00", "name\x1f", strings.Repeat("a", 256), strings.Repeat("😀", 128), windowsOwnerNamespace, strings.ToUpper(windowsOwnerNamespace)} {
		got := ownerSubdirForOS(owner, "windows")
		sum := sha256.Sum256([]byte(owner))
		want := filepath.Join(windowsOwnerNamespace, hex.EncodeToString(sum[:]))
		if got != want {
			t.Fatalf("%q: got %q want %q", owner, got, want)
		}
		key := strings.ToLower(got)
		if previous, ok := seen[key]; ok {
			t.Fatalf("identities %q/%q collide", previous, owner)
		}
		seen[key] = owner
	}
	// A lossy sanitized actor must not alias an unsafe raw identity.
	if ownerSubdirForOS("cloud:alice@example.test", "windows") == ownerSubdirForOS("cloud-alice@example.test", "windows") {
		t.Fatal("raw identity aliased readable owner")
	}
}

func TestOwnerSubdirPreservesExistingStores(t *testing.T) {
	for _, owner := range []string{"", "   ", "alice", "Agent-A_1", "safe.name", "COM10", "COMLPT1", "LPTCOM1", "normal user", strings.Repeat("a", 255)} {
		if got := ownerSubdirForOS(owner, "windows"); got != SanitizeOwner(owner) {
			t.Fatalf("safe %q moved to %q", owner, got)
		}
	}
	for _, owner := range []string{"cloud:alice@example.test", "CON", "../alice", windowsOwnerNamespace} {
		for _, goos := range []string{"linux", "darwin"} {
			if got := ownerSubdirForOS(owner, goos); got != SanitizeOwner(owner) {
				t.Fatalf("%s store moved: %q => %q", goos, owner, got)
			}
		}
	}
}

func TestOwnerSubdirUserStoreAndResolveStoreAgree(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_TODO_DIR", root)
	for _, owner := range []string{"alice", "cloud:alice@example.test", "CON", windowsOwnerNamespace} {
		user, err := UserStore(owner)
		if err != nil {
			t.Fatal(err)
		}
		resolved, _, err := ResolveStore(owner, false, true, "")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, OwnerSubdir(owner))
		if user.Dir() != want || resolved.Dir() != want {
			t.Fatalf("%q stores disagree: %q %q want %q", owner, user.Dir(), resolved.Dir(), want)
		}
		repo, _, err := ResolveStore(owner, false, false, root)
		if err != nil {
			t.Fatal(err)
		}
		if repo.Dir() != filepath.Join(root, RepoSub) {
			t.Fatalf("owner changed explicit repo store: %s", repo.Dir())
		}
	}
}
