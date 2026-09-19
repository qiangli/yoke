package weave

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRepoKeyFoldsSpellings(t *testing.T) {
	want := "github.com/qiangli/bashy"
	for _, in := range []string{
		"git@github.com:qiangli/bashy.git",
		"ssh://git@github.com/qiangli/bashy.git",
		"https://github.com/qiangli/bashy",
		"https://github.com/qiangli/bashy.git/",
		"https://user:token@GitHub.com/qiangli/bashy.git",
		"  git@github.com:qiangli/bashy  ",
	} {
		got, err := NormalizeRepoKey(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "bashy"} {
		if _, err := NormalizeRepoKey(in); !errors.Is(err, ErrNoOrigin) {
			t.Fatalf("%q: want ErrNoOrigin, got %v", in, err)
		}
	}
	// A path remote (a bare repo on a shared filesystem) keys under `file/`,
	// and file:// spells the same key.
	for _, in := range []string{"/srv/git/team.git", "file:///srv/git/team.git", "/srv/git/team"} {
		got, err := NormalizeRepoKey(in)
		if err != nil || got != "file/srv/git/team" {
			t.Fatalf("%q → %q, %v", in, got, err)
		}
	}
}

// sessionTestEnv isolates the queue dir, the origin seam, the client seam and
// the identity env so the test never touches the operator's stores.
func sessionTestEnv(t *testing.T, origin string, fake *fakeSessionClient) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", "")
	t.Setenv("CLOUDBOX_TOKEN", "tok-test")
	t.Setenv("BASHY_CLOUDBOX_URL", "http://cloudbox.test")
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/plinth")
	repo := t.TempDir()
	oldOrigin := repoOriginURL
	repoOriginURL = func(string) (string, error) {
		if origin == "" {
			return "", ErrNoOrigin
		}
		return origin, nil
	}
	credCache.mu.Lock()
	credCache.m = map[string]credEntry{}
	credCache.mu.Unlock()
	oldClient := newSessionClient
	newSessionClient = func(base, token string) SessionClient {
		if base != "http://cloudbox.test" || token != "tok-test" {
			t.Fatalf("client built with base=%q token=%q", base, token)
		}
		return fake
	}
	t.Cleanup(func() { repoOriginURL = oldOrigin; newSessionClient = oldClient })
	return repo
}

func TestEnsureRepoSessionCreatesThenReuses(t *testing.T) {
	fake := &fakeSessionClient{}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)

	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.created || len(fake.creates) != 1 || fake.creates[0].TargetRepo != "github.com/qiangli/bashy" {
		t.Fatalf("expected one create keyed on the repo, got created=%v creates=%+v", sc.created, fake.creates)
	}
	if len(fake.joins) != 1 || fake.joins[0].Participant != "plinth@"+sessionHostName() || fake.joins[0].Role != "contributor" {
		t.Fatalf("join = %+v", fake.joins)
	}
	p, err := ReadSessionPointer(repo)
	if err != nil || p == nil || p.TaskID != "task-1" || p.RepoKey != "github.com/qiangli/bashy" {
		t.Fatalf("pointer = %+v err=%v", p, err)
	}

	// Second call: the pointer answers; no create, no join, no lookup.
	sc2, err := EnsureRepoSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if sc2.created || len(fake.creates) != 1 || len(fake.joins) != 1 || sc2.pointer.TaskID != "task-1" {
		t.Fatalf("second call must be a pointer read: created=%v creates=%d joins=%d", sc2.created, len(fake.creates), len(fake.joins))
	}
}

func TestEnsureRepoSessionJoinsTheExistingOne(t *testing.T) {
	fake := &fakeSessionClient{tasks: []TaskSummary{
		{ID: "t-old", TargetRepo: "https://github.com/qiangli/bashy.git", Status: "done"},
		{ID: "t-live", TargetRepo: "https://github.com/qiangli/bashy.git", Status: "active"},
		{ID: "t-other", TargetRepo: "github.com/qiangli/sh", Status: "active"},
	}}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if sc.created || len(fake.creates) != 0 || sc.pointer.TaskID != "t-live" {
		t.Fatalf("want join of t-live with no create; got created=%v task=%s creates=%d", sc.created, sc.pointer.TaskID, len(fake.creates))
	}
}

func TestEnsureRepoSessionRefusesAmbiguity(t *testing.T) {
	fake := &fakeSessionClient{tasks: []TaskSummary{
		{ID: "t-a", TargetRepo: "github.com/qiangli/bashy", Status: "active", Created: time.Unix(1, 0)},
		{ID: "t-b", TargetRepo: "git@github.com:qiangli/bashy.git", Status: "active", Created: time.Unix(2, 0)},
	}}
	repo := sessionTestEnv(t, "https://github.com/qiangli/bashy", fake)
	_, err := EnsureRepoSession(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "t-a, t-b") || len(fake.joins) != 0 {
		t.Fatalf("want an ambiguity error naming both and no join, got err=%v joins=%d", err, len(fake.joins))
	}
}

func TestEnsureRepoSessionNeedsAnOrigin(t *testing.T) {
	fake := &fakeSessionClient{}
	repo := sessionTestEnv(t, "", fake)
	if _, err := EnsureRepoSession(context.Background(), repo); !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("want ErrNoOrigin, got %v", err)
	}
}

func TestEnsureRepoSessionRefusesUnpaired(t *testing.T) {
	fake := &fakeSessionClient{}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	t.Setenv("CLOUDBOX_TOKEN", "")
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no `outpost` binary → no paired token
	credCache.mu.Lock()
	credCache.m = map[string]credEntry{}
	credCache.mu.Unlock()
	if _, err := EnsureRepoSession(context.Background(), repo); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("want ErrNotPaired, got %v", err)
	}
}

// The OS login is never part of an address: the same cloudbox account has
// different OS users on its hosts, so a participant signature that carried
// $USER would name the wrong thing on every other machine.
func TestSessionParticipantNeverCarriesTheOSLogin(t *testing.T) {
	t.Setenv("USER", "noviadmin")
	t.Setenv("LOGNAME", "noviadmin")
	t.Setenv("USERNAME", "noviadmin")
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/plinth")
	t.Setenv("BASHY_AGENT_ID", "")
	p, host := SessionParticipant()
	if strings.Contains(p, "noviadmin") || !strings.HasPrefix(p, "plinth@") || host == "" || !strings.HasSuffix(p, "@"+host) {
		t.Fatalf("participant %q host %q", p, host)
	}
	// With no agent identity the seat is an explicitly-marked person, still
	// at the host — never a bare login that could pass for an agent.
	for _, k := range []string{"BASHY_PRINCIPAL", "WEAVE_CONDUCTOR", "BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT"} {
		t.Setenv(k, "")
	}
	p, _ = SessionParticipant()
	if !strings.HasPrefix(p, "person:") {
		t.Fatalf("no-agent participant %q must be marked person:", p)
	}
}
