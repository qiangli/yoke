package src_test

import (
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/qiangli/yoke/pkg/src"
)

func TestReleaseActorKeepsPrincipalNamespace(t *testing.T) {
	for _, actor := range []string{"", "authorized-agent"} {
		t.Run(actor, func(t *testing.T) {
			home := t.TempDir()
			dir, base := s526b8Fixture(t, home)
			const principal = "host"
			got, err := src.Release(home, s526b8App, base, principal, src.ReleaseOptions{Actor: actor})
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != "v1.0.0-host" {
				t.Fatalf("actor changed release namespace: %s", got.Version)
			}
			repo, err := gogit.PlainOpen(dir)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := repo.Tag(got.Version)
			if err != nil {
				t.Fatal(err)
			}
			tag, err := repo.TagObject(ref.Hash())
			if err != nil {
				t.Fatal(err)
			}
			want := actor
			if want == "" {
				want = principal
			}
			if tag.Tagger.Name != want || !strings.Contains(tag.Message, "Principal: host\n") {
				t.Fatalf("scope/actor provenance: %#v", tag)
			}
			if actor == "" {
				if strings.Contains(tag.Message, "\nActor:") {
					t.Fatal("default tag message changed")
				}
			} else if !strings.Contains(tag.Message, "Actor: "+actor+"\n") {
				t.Fatalf("missing actor trailer: %s", tag.Message)
			}
			visible, err := src.Releases(home, s526b8App, principal)
			if err != nil || len(visible) != 1 {
				t.Fatalf("host cannot see its release: %#v %v", visible, err)
			}
			other, err := src.Releases(home, s526b8App, "other-principal")
			if err != nil || len(other) != 0 {
				t.Fatalf("scope escaped to another principal: %#v %v", other, err)
			}
		})
	}
}
