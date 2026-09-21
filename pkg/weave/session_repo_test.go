package weave

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
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
	oldOrigin := repoSessionRemote
	repoSessionRemote = func(string) (string, string, error) {
		if origin == "" {
			return "", "", ErrNoOrigin
		}
		return origin, "origin", nil
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
	t.Cleanup(func() { repoSessionRemote = oldOrigin; newSessionClient = oldClient })
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

// Slice B: a cloudbox that answers ?repo= decides the seat. A session the
// caller cannot reach but GitHub might seat it on is joined by repo; the
// role comes back on the pointer; GitHub refusing means "no session for
// you" and the caller creates its own.
func TestEnsureRepoSessionJoinsByRepoWhenGitHubSeatsTheCaller(t *testing.T) {
	theirs := TaskSummary{ID: "t-theirs", TargetRepo: "github.com/qiangli/bashy", Status: "active"}
	fake := &fakeSessionClient{
		repoSessions: &RepoSessions{Repo: "github.com/qiangli/bashy", Sessions: []RepoSession{{Task: theirs, Joinable: true}}},
		joinRole:     "observer",
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if sc.created || sc.pointer.TaskID != "t-theirs" || sc.pointer.Role != "observer" {
		t.Fatalf("pointer = %+v created=%v", sc.pointer, sc.created)
	}
	if len(fake.joinByRepo) != 1 || fake.joinByRepo[0].Repo != "github.com/qiangli/bashy" || fake.joinByRepo[0].Participant != "plinth@"+sessionHostName() {
		t.Fatalf("join-by-repo = %+v", fake.joinByRepo)
	}
	if len(fake.joins) != 0 || len(fake.creates) != 0 {
		t.Fatalf("join-by-repo already seated us: joins=%d creates=%d", len(fake.joins), len(fake.creates))
	}
}

func TestEnsureRepoSessionCreatesOwnWhenGitHubRefuses(t *testing.T) {
	theirs := TaskSummary{ID: "t-theirs", TargetRepo: "github.com/qiangli/bashy", Status: "active"}
	fake := &fakeSessionClient{
		repoSessions: &RepoSessions{Sessions: []RepoSession{{Task: theirs, Joinable: true}}},
		joinErr:      errors.New("cloudbox request failed: 404 Not Found: {}"),
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.created || sc.pointer.Role != "owner" || sc.pointer.TaskID == "t-theirs" {
		t.Fatalf("want an own session when GitHub refuses, got %+v created=%v", sc.pointer, sc.created)
	}
	// ...and that session is PRIVATE, so it never shows up as joinable to
	// the next account on the key.
	if len(fake.creates) != 1 || fake.creates[0].Discovery != "private" {
		t.Fatalf("a session created after a GitHub refusal must be private: %+v", fake.creates)
	}
}

func TestEnsureRepoSessionPrefersReachableOverJoinable(t *testing.T) {
	mine := TaskSummary{ID: "t-mine", TargetRepo: "github.com/qiangli/bashy", Status: "active"}
	theirs := TaskSummary{ID: "t-theirs", TargetRepo: "github.com/qiangli/bashy", Status: "active"}
	fake := &fakeSessionClient{repoSessions: &RepoSessions{Sessions: []RepoSession{{Task: theirs, Joinable: true}, {Task: mine}}}}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil || sc.pointer.TaskID != "t-mine" || len(fake.joinByRepo) != 0 || len(fake.joins) != 1 {
		t.Fatalf("pointer=%+v err=%v joinByRepo=%d joins=%d", sc.pointer, err, len(fake.joinByRepo), len(fake.joins))
	}
}

// A reachable session carries the seat cloudbox reports — on a GitHub-seated
// repo's second checkout that is the GitHub role, not a flat "member"; a
// cloudbox that does not say it still yields "member".
func TestEnsureRepoSessionReachableShowsReportedRole(t *testing.T) {
	for _, c := range []struct{ reported, want string }{{"observer", "observer"}, {"contributor", "contributor"}, {"owner", "owner"}, {"", "member"}} {
		mine := TaskSummary{ID: "t-mine", TargetRepo: "github.com/qiangli/bashy", Status: "active", Role: c.reported}
		fake := &fakeSessionClient{repoSessions: &RepoSessions{Sessions: []RepoSession{{Task: mine}}}}
		repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
		sc, err := EnsureRepoSession(context.Background(), repo)
		if err != nil || sc.pointer.TaskID != "t-mine" || sc.pointer.Role != c.want {
			t.Fatalf("reported %q: pointer=%+v err=%v want role %q", c.reported, sc.pointer, err, c.want)
		}
	}
}

// The fork layout `gh repo fork --clone` leaves behind — origin = the fork,
// upstream = the team's repo — must key the session on UPSTREAM, or every PR
// contributor opens a private session on their own fork instead of joining
// the team's (Sprint 226, GitHub team configurations b/c).
func TestRepoKeyPrefersUpstreamOverOrigin(t *testing.T) {
	repo := t.TempDir()
	r, err := gogit.PlainInit(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&gogitconfig.RemoteConfig{Name: "origin", URLs: []string{"git@github.com:erin/bashy.git"}}); err != nil {
		t.Fatal(err)
	}
	key, remote, err := RepoKeyRemote(repo)
	if err != nil || key != "github.com/erin/bashy" || remote != "origin" {
		t.Fatalf("plain clone: key=%q remote=%q err=%v", key, remote, err)
	}
	if _, err := r.CreateRemote(&gogitconfig.RemoteConfig{Name: "upstream", URLs: []string{"https://github.com/qiangli/bashy"}}); err != nil {
		t.Fatal(err)
	}
	key, remote, err = RepoKeyRemote(repo)
	if err != nil || key != "github.com/qiangli/bashy" || remote != "upstream" {
		t.Fatalf("fork layout: key=%q remote=%q err=%v", key, remote, err)
	}
	if _, _, err := RepoKeyRemote(t.TempDir()); err == nil {
		t.Fatal("a directory that is not a checkout must not yield a key")
	}
}

// A GitHub-seated pointer is a local VIEW of GitHub's answer, re-derived on
// every verb: a PR contributor granted push becomes a contributor on the next
// verb; a removed collaborator is told so by name; the owner pays nothing.
func TestEnsureRepoSessionResyncsAGitHubSeat(t *testing.T) {
	theirs := TaskSummary{ID: "t-theirs", TargetRepo: "github.com/qiangli/bashy", Status: "active"}
	fake := &fakeSessionClient{
		repoSessions: &RepoSessions{Repo: "github.com/qiangli/bashy", Sessions: []RepoSession{{Task: theirs, Joinable: true}}},
		joinRole:     "observer",
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil || sc.pointer.Role != "observer" || sc.pointer.SeatAsOf == "" || sc.pointer.RepoRemote != "origin" {
		t.Fatalf("first resolve: pointer=%+v err=%v", sc.pointer, err)
	}
	// Promoted on GitHub: the next verb sees contributor, through join-by-repo only.
	fake.joinRole = "contributor"
	sc, err = EnsureRepoSession(context.Background(), repo)
	if err != nil || sc.pointer.Role != "contributor" || sc.pointer.TaskID != "t-theirs" {
		t.Fatalf("after grant: pointer=%+v err=%v", sc.pointer, err)
	}
	if len(fake.joinByRepo) != 2 || len(fake.joins) != 0 || len(fake.creates) != 0 {
		t.Fatalf("resync must be ONE join-by-repo per verb and nothing else: joinByRepo=%d joins=%d creates=%d", len(fake.joinByRepo), len(fake.joins), len(fake.creates))
	}
	// Demoted: back to observer, same session.
	fake.joinRole = "observer"
	if sc, err = EnsureRepoSession(context.Background(), repo); err != nil || sc.pointer.Role != "observer" {
		t.Fatalf("after demotion: pointer=%+v err=%v", sc.pointer, err)
	}
	// Revoked: said by name, never a silent fallback to a private session.
	fake.joinErr = errors.New("cloudbox request failed: 404 Not Found: {}")
	_, err = EnsureRepoSession(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "GitHub no longer vouches") || !strings.Contains(err.Error(), "github.com/qiangli/bashy") || !strings.Contains(err.Error(), "seat was observer as of ") {
		t.Fatalf("revoked grant must be named: %v", err)
	}
	if len(fake.creates) != 0 {
		t.Fatalf("a revoked seat must not open a private session behind the caller's back: %+v", fake.creates)
	}
}

func TestEnsureRepoSessionOwnerNeverResyncs(t *testing.T) {
	fake := &fakeSessionClient{repoSessions: &RepoSessions{}}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil || sc.pointer.Role != "owner" || sc.pointer.SeatAsOf != "" {
		t.Fatalf("create: pointer=%+v err=%v", sc.pointer, err)
	}
	before := len(fake.joinByRepo)
	if _, err := EnsureRepoSession(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if len(fake.joinByRepo) != before {
		t.Fatalf("an owner's seat is cloudbox's own; no join-by-repo on the second verb (got %d)", len(fake.joinByRepo)-before)
	}
}
