package src

import (
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

func TestReleaseSlug(t *testing.T) {
	for _, principal := range []string{
		"cloud:alice@example.com", "12345", strings.Repeat("a", 100),
		"alice:bob", "alice@bob", "ALICE", "alice", "...",
	} {
		slug := releaseSlug(principal)
		if !semver.IsValid("v1.2.3-"+slug) || len(slug) > 70 {
			t.Fatalf("invalid slug for %q: %q", principal, slug)
		}
	}
	if slug := releaseSlug("cloud:alice@example.com"); !strings.HasPrefix(slug, "cloud-alice-example-com-") {
		t.Fatalf("colon/at/dot slug: %q", slug)
	}
	if slug := releaseSlug("12345"); slug != "u12345" {
		t.Fatalf("numeric slug: %q", slug)
	}
	for _, pair := range [][2]string{{"alice:bob", "alice@bob"}, {"ALICE", "alice"}, {strings.Repeat("a", 100) + "x", strings.Repeat("a", 100) + "y"}} {
		if releaseSlug(pair[0]) == releaseSlug(pair[1]) {
			t.Fatalf("colliding principals: %q and %q", pair[0], pair[1])
		}
	}
}

func TestReleaseSemverPrecedence(t *testing.T) {
	tags := []ReleaseTag{{Version: "v1.0.1-device-alice"}, {Version: "v1.0.0"}, {Version: "v1.0.1"}}
	sortReleases(tags)
	want := []string{"v1.0.1", "v1.0.1-device-alice", "v1.0.0"}
	for i := range tags {
		if tags[i].Version != want[i] {
			t.Fatalf("sort = %+v", tags)
		}
	}
}
