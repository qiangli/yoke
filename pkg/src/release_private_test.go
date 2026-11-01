package src_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/src"
	"golang.org/x/mod/semver"
)

func TestPrivateRepairReleases(t *testing.T) {
	home := t.TempDir()
	dir, base := s526b8Fixture(t, home)
	if _, err := src.Release(home, s526b8App, base, "operator", src.ReleaseOptions{Shared: true}); err != nil {
		t.Fatal(err)
	}
	alice := "device:alice"
	for i := 1; i <= 2; i++ {
		s526b8Write(t, dir, map[string]string{"app.txt": strings.Repeat("fix\n", i)})
		rev := s526b8Commit(t, dir, "repair")
		rel, err := src.Release(home, s526b8App, rev, alice, src.ReleaseOptions{Repair: true})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(rel.Version, fmt.Sprintf("v1.0.%d-device-alice-", i)) || !semver.IsValid(rel.Version) {
			t.Fatalf("private release: %+v", rel)
		}
		if version, released, err := src.Version(home, s526b8App, rev, alice); err != nil || !released || version != rel.Version {
			t.Fatalf("alice version = %q, %v, %v", version, released, err)
		}
		if version, released, err := src.Version(home, s526b8App, rev, "device:bob"); err != nil || released || version == rel.Version {
			t.Fatalf("bob version = %q, %v, %v", version, released, err)
		}
		if _, _, err := src.Version(home, s526b8App, rel.Version, "device:bob"); err == nil {
			t.Fatalf("bob resolved alice's private tag %s", rel.Version)
		}
		for _, tc := range []struct {
			principal string
			count     int
		}{{alice, i + 1}, {"device:bob", 1}, {"", 1}} {
			tags, err := src.Releases(home, s526b8App, tc.principal)
			if err != nil || len(tags) != tc.count {
				t.Fatalf("%q releases: %+v, %v", tc.principal, tags, err)
			}
			for _, tag := range tags {
				if tc.principal != alice && tag.Version == rel.Version {
					t.Fatalf("%q saw alice's tag", tc.principal)
				}
			}
		}
		bob, err := src.History(home, s526b8App, "device:bob")
		if err != nil || len(bob.Releases) != 1 {
			t.Fatalf("bob history: %+v, %v", bob.Releases, err)
		}
		for _, release := range bob.Releases {
			if release.Version == rel.Version {
				t.Fatalf("bob history exposed %s", rel.Version)
			}
		}
	}
}
