package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPnpmFloorSatisfiedByDefaultNode pins a coupling that is invisible from
// either side of it.
//
// `bashy pnpm` routes through corepack, and corepack honours the
// `packageManager` field in the project's package.json. So the pnpm that runs is
// chosen by pkg/meet/web, while the Node it runs ON is chosen by DefaultVersion
// here — two pins, two trees, no link between them.
//
// They drifted apart in production: DefaultVersion was 22.11.0 while the SPA
// asked for pnpm@11.17.0, which requires Node >= 22.13. Every clean provision of
// `bashy pnpm` then failed with "This version of pnpm requires at least Node.js
// v22.13", so the meet SPA could not be built on a machine with no system Node —
// precisely the machine this provisioner exists to serve. It was found only when
// a remote host built a silently UI-less binary.
//
// This test cannot know pnpm's floor (it is not published in a machine-readable
// form next to the version), so it asserts the part that IS checkable and that
// actually broke: DefaultVersion must not regress below the floor we know pnpm
// currently requires. Raising KNOWN_FLOOR is a deliberate edit, which is the
// point — it forces whoever bumps the SPA's packageManager to look here.
const pnpmKnownNodeFloor = "22.13.0"

func TestPnpmFloorSatisfiedByDefaultNode(t *testing.T) {
	if cmp(DefaultVersion, pnpmKnownNodeFloor) < 0 {
		t.Fatalf("DefaultVersion %s is below pnpm's known Node floor %s — `bashy pnpm` will fail on a clean provision with "+
			"\"This version of pnpm requires at least Node.js v%s\", and the meet SPA cannot be built on a host with no system Node",
			DefaultVersion, pnpmKnownNodeFloor, pnpmKnownNodeFloor)
	}
}

// TestEmbeddedSumsCoverDefaultVersion catches the other half of a bump: the
// embedded digests must name the version actually being provisioned. Bumping
// DefaultVersion without regenerating the sums leaves every platform falling
// back to a live SHASUMS fetch, quietly losing the offline trust anchor the
// embedded file exists to provide.
func TestEmbeddedSumsCoverDefaultVersion(t *testing.T) {
	if strings.TrimSpace(embeddedSums) == "" {
		t.Fatal("embeddedSums is empty")
	}
	want := "node-v" + DefaultVersion + "-"
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(embeddedSums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("malformed sums line %q", line)
		}
		if !strings.HasPrefix(f[1], want) {
			t.Errorf("sums entry %q does not belong to DefaultVersion %s", f[1], DefaultVersion)
		}
		if len(f[0]) != 64 {
			t.Errorf("entry %q has a %d-char digest, want 64", f[1], len(f[0]))
		}
		n++
	}
	// Every platform the provisioner can be asked for must be pinned offline,
	// or that platform silently degrades to the live fetch.
	if n < 6 {
		t.Errorf("embedded sums pin %d archives, want at least 6 (darwin/linux/win x arm64/x64)", n)
	}
}

// TestSPAPackageManagerIsPinned guards the other end: corepack resolves the
// LATEST pnpm when package.json names no version, and latest is free to raise
// its Node floor at any time. An unpinned package manager against a pinned
// runtime is guaranteed to drift.
func TestSPAPackageManagerIsPinned(t *testing.T) {
	path := filepath.Join("..", "..", "pkg", "meet", "web", "package.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("meet SPA package.json not readable here: %v", err)
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	pm := strings.TrimSpace(pkg.PackageManager)
	if pm == "" {
		t.Fatal("meet SPA package.json declares no packageManager — corepack would fetch LATEST pnpm, whose Node floor can rise at any time and break `bashy pnpm` on a clean provision")
	}
	if !strings.Contains(pm, "@") {
		t.Errorf("packageManager %q names no version", pm)
	}
}

// cmp compares dotted numeric versions. Returns -1, 0 or 1.
func cmp(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := 0, 0
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
