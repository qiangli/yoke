package weave

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// The repo IS the session key.
//
// A team session (a cloudbox Task with its append-only feed and roster) is
// looked up by the repo the caller is standing in — the way an employee's
// Jira project is implied by the code they are working on — so no id is ever
// typed, copied between hosts, or handed to a colleague. The key is the
// normalized origin URL: `github.com/<org>/<repo>`, ssh/https/`.git` folded.
//
// Everything cross-host (`mb send name@host`, `inbox`, `sprint start/take`)
// calls EnsureRepoSession lazily; `sprint session open|join|status` are the
// explicit spellings of the same derivation.

// ErrNoOrigin is returned when the checkout has no origin remote, so no key
// can be derived. `sprint session join <task-id>` is the escape hatch.
var ErrNoOrigin = errors.New("session: repo has no origin remote; join with an explicit task id")

// ErrNotPaired is returned when no cloudbox credential resolves. Pairing is
// the ONLY setup this feature asks for, so the message says exactly that.
var ErrNotPaired = errors.New("session: this host is not paired with cloudbox (run `bashy login`), and no $BASHY_FLEET_TOKEN / $BASHY_API_KEY / $CLOUDBOX_TOKEN is set")

// repoOriginURL is a seam so tests can derive a key without a git checkout.
//
// The key must not depend on a git BINARY: a Windows host running bashy has
// MinGit only inside bashy's own cache (`bashy git`), not on PATH, and the
// first live run on such a host failed here with "no origin remote". So the
// remote is read from the checkout itself (pure-Go git), and exec'ing git
// is only the fallback for a layout go-git cannot open.
var repoOriginURL = func(repoRoot string) (string, error) {
	if r, err := gogit.PlainOpenWithOptions(repoRoot, &gogit.PlainOpenOptions{DetectDotGit: true}); err == nil {
		if rem, err := r.Remote("origin"); err == nil && rem != nil && len(rem.Config().URLs) > 0 {
			return strings.TrimSpace(rem.Config().URLs[0]), nil
		}
		return "", ErrNoOrigin
	}
	out, err := gitOutput(repoRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", ErrNoOrigin
	}
	return strings.TrimSpace(out), nil
}

var scpLikeRemote = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9.-]+):(.+)$`)

// NormalizeRepoKey folds the spellings of one remote into one key:
//
//	git@github.com:org/repo.git
//	ssh://git@github.com/org/repo.git
//	https://github.com/org/repo
//	https://user:token@github.com/org/repo.git
//
// all become `github.com/org/repo`. Host is lowercased; the path keeps its
// case (GitHub is case-insensitive on paths but preserves them, and a key
// that changes case on every host would fork the session).
func NormalizeRepoKey(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", ErrNoOrigin
	}
	var host, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("session: cannot parse origin %q", remote)
		}
		if u.Scheme == "file" {
			return NormalizeRepoKey(u.Path)
		}
		if u.Host == "" {
			return "", fmt.Errorf("session: cannot parse origin %q", remote)
		}
		host, path = u.Hostname(), u.Path
	} else if m := scpLikeRemote.FindStringSubmatch(remote); m != nil {
		host, path = m[1], m[2]
	} else if filepath.IsAbs(remote) || strings.HasPrefix(remote, "~") || strings.HasPrefix(remote, ".") {
		// A path remote: a bare repo on a shared filesystem is a legitimate
		// origin for a team (and the stand-in the gate uses). Key it under a
		// pseudo-host so every clone of that path agrees, and it can never
		// collide with a URL's host.
		abs, err := filepath.Abs(remote)
		if err != nil {
			return "", ErrNoOrigin
		}
		return "file/" + strings.TrimSuffix(strings.Trim(filepath.ToSlash(abs), "/"), ".git"), nil
	} else {
		return "", ErrNoOrigin
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" {
		return "", fmt.Errorf("session: cannot derive a repo key from %q", remote)
	}
	return strings.ToLower(host) + "/" + path, nil
}

// RepoKey derives the session key for a checkout.
func RepoKey(repoRoot string) (string, error) {
	remote, err := repoOriginURL(repoRoot)
	if err != nil {
		return "", err
	}
	return NormalizeRepoKey(remote)
}

// sessionCredentials walks the one cloudbox ladder (fleet.ResolveCloud) with
// the pointer's own values first, so a session joined with an explicit base or
// token keeps working, and a host that only ever paired needs nothing else.
// The pointer's TokenRef stays an env-var NAME (compatibility with the shipped
// pointer shape); $CLOUDBOX_TOKEN is its default.
func sessionCredentials(p *SessionPointer) (base, token string, err error) {
	key := ""
	if p != nil {
		key = p.CloudboxBase + "\x00" + p.TokenRef
	}
	credCache.mu.Lock()
	if c, ok := credCache.m[key]; ok && time.Since(c.at) < credCacheTTL {
		credCache.mu.Unlock()
		return c.base, c.token, c.err
	}
	credCache.mu.Unlock()
	base, token, err = resolveSessionCredentials(p)
	credCache.mu.Lock()
	credCache.m[key] = credEntry{base: base, token: token, err: err, at: time.Now()}
	credCache.mu.Unlock()
	return base, token, err
}

// The ladder may exec `outpost token print`; an inbox watch polls every two
// seconds and a turn preamble runs every turn, so the answer is memoized per
// process for a few minutes. A pairing that changes mid-process is picked up
// at the next expiry; an error is cached too, so an unpaired host does not
// spawn a process per poll to learn the same thing.
type credEntry struct {
	base, token string
	err         error
	at          time.Time
}

var (
	credCacheTTL = 5 * time.Minute
	credCache    = struct {
		mu sync.Mutex
		m  map[string]credEntry
	}{m: map[string]credEntry{}}
)

func resolveSessionCredentials(p *SessionPointer) (base, token string, err error) {
	var urlOverride, tokenOverride string
	if p != nil {
		urlOverride = p.CloudboxBase
		ref := strings.TrimSpace(p.TokenRef)
		if ref == "" {
			ref = "CLOUDBOX_TOKEN"
		}
		tokenOverride = os.Getenv(ref)
	} else {
		tokenOverride = os.Getenv("CLOUDBOX_TOKEN")
	}
	base, token = fleet.ResolveCloud(urlOverride, tokenOverride)
	if token == "" {
		return "", "", ErrNotPaired
	}
	return base, token, nil
}

// SessionParticipant is how this process signs the roster: the agent's NAME
// (the runtime key an inbox, a kb attribution and a bus cursor hang off) at
// the name cloudbox knows this host by. The OS login is deliberately absent —
// one cloudbox account has different OS users on its hosts, and an address
// that carried the login would name the wrong thing on every other machine.
//
// A caller with no agent identity gets the account-neutral `person` handle of
// the local user, marked so it is never confused with a registered agent.
func SessionParticipant() (participant, host string) {
	host = sessionHostName()
	if name, ok := weaveConductorIdentity(""); ok {
		return name + "@" + host, host
	}
	return "person:" + principalLocalUser() + "@" + host, host
}

// sessionHostName is the cloudbox host name when paired (the outpost agent
// name), else the OS hostname — the same rule principal.Ref.Host follows.
func sessionHostName() string {
	env := principal.DefaultEnv()
	if env.Paired && env.PairedName != "" {
		return env.PairedName
	}
	if env.Hostname != "" {
		return env.Hostname
	}
	return "localhost"
}

func principalLocalUser() string {
	if u := principal.DefaultEnv().LocalUser; u != "" {
		return u
	}
	return "unknown"
}

// EnsureRepoSession resolves the team session for a checkout, creating it
// when the repo has none, and joins it as this process. It is idempotent: a
// pointer that already names a task is returned as is (one GET at most).
//
// Returns the client bound to that task. Callers that only need the key use
// RepoKey.
func EnsureRepoSession(ctx context.Context, repoRoot string) (*sessionRepoClient, error) {
	pointer, err := ReadSessionPointer(repoRoot)
	if err != nil {
		return nil, err
	}
	base, token, err := sessionCredentials(pointer)
	if err != nil {
		return nil, err
	}
	client := newSessionClient(base, token)
	if pointer != nil && pointer.TaskID != "" {
		return &sessionRepoClient{repoRoot: repoRoot, pointer: pointer, client: client}, nil
	}
	key, err := RepoKey(repoRoot)
	if err != nil {
		return nil, err
	}
	participant, host := SessionParticipant()
	taskID, role, joined, refused, err := resolveRepoSession(ctx, client, key, participant, host)
	if err != nil {
		return nil, err
	}
	created := false
	if taskID == "" {
		req := CreateTaskReq{
			Name:       key,
			Display:    "team session · " + key,
			Goal:       "team session for " + key,
			TargetRepo: key,
		}
		if refused {
			// GitHub declined this account on the repo, so its session is
			// nobody's business but its own: private, never discoverable —
			// otherwise every stranger's fallback session would pollute the
			// key and make the team's session ambiguous for the next joiner.
			req.Discovery = "private"
		}
		task, err := client.CreateTask(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("session: create for %s: %w", key, err)
		}
		taskID = task.ID
		role = "owner"
		created = true
	}
	if !joined {
		if _, err := client.Join(ctx, taskID, JoinReq{Participant: participant, Host: host, Tool: "bashy", Role: "contributor"}); err != nil {
			return nil, fmt.Errorf("session: join %s: %w", key, err)
		}
	}
	if pointer == nil {
		pointer = &SessionPointer{}
	}
	pointer.TaskID = taskID
	pointer.RepoKey = key
	pointer.Role = role
	if pointer.CloudboxBase == "" {
		pointer.CloudboxBase = base
	}
	if pointer.TokenRef == "" {
		pointer.TokenRef = "CLOUDBOX_TOKEN"
	}
	if err := WriteSessionPointer(repoRoot, pointer); err != nil {
		return nil, err
	}
	return &sessionRepoClient{repoRoot: repoRoot, pointer: pointer, client: client, created: created}, nil
}

// resolveRepoSession finds the session for a key. A cloudbox that answers
// `?repo=` decides itself: a reachable session is joined by id; a session
// GitHub might seat the caller on is joined through join-by-repo, which
// records the seat (or refuses — a refusal is "no session", so the caller
// may create its own); several of either is an ambiguity, named and
// refused. A cloudbox without the query is filtered client-side as before.
// Returns the task id ("" = none), the seat, whether the join already
// happened, and whether GitHub declined the caller on a session it saw.
func resolveRepoSession(ctx context.Context, client SessionClient, key, participant, host string) (taskID, role string, joined, refused bool, err error) {
	rs, err := client.ListTasksByRepo(ctx, key)
	if errors.Is(err, ErrRepoQueryUnsupported) {
		id, lerr := lookupRepoSession(ctx, client, key)
		if lerr != nil {
			return "", "", false, false, lerr
		}
		if id == "" {
			return "", "", false, false, nil
		}
		return id, "member", false, false, nil
	}
	if err != nil {
		return "", "", false, false, err
	}
	var reachable, joinable []TaskSummary
	for _, s := range rs.Sessions {
		if !sessionTaskActive(s.Task) {
			continue
		}
		if s.Joinable {
			joinable = append(joinable, s.Task)
		} else {
			reachable = append(reachable, s.Task)
		}
	}
	if len(reachable) > 1 {
		return "", "", false, false, ambiguousSessions(key, reachable)
	}
	if len(reachable) == 1 {
		return reachable[0].ID, "member", false, false, nil
	}
	if len(joinable) > 1 {
		return "", "", false, false, ambiguousSessions(key, joinable)
	}
	if len(joinable) == 1 {
		resp, jerr := client.JoinByRepo(ctx, JoinByRepoReq{Repo: key, Participant: participant, Host: host, Tool: "bashy"})
		if jerr != nil {
			// GitHub does not vouch for this account on that repo: the
			// session exists but is not ours to join. Not an error — the
			// caller gets its own (private) session on the key, as any
			// registered user may.
			if strings.Contains(strings.ToLower(jerr.Error()), "404") || strings.Contains(strings.ToLower(jerr.Error()), "not found") {
				return "", "", false, true, nil
			}
			return "", "", false, false, fmt.Errorf("session: join %s by repo: %w", key, jerr)
		}
		return resp.Task.ID, resp.Role, true, false, nil
	}
	return "", "", false, false, nil
}

func ambiguousSessions(key string, ts []TaskSummary) error {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Created.Before(ts[j].Created) })
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return fmt.Errorf("session: %d active sessions are keyed on %s (%s); join one explicitly: bashy sprint session join <task-id>", len(ts), key, strings.Join(ids, ", "))
}

// lookupRepoSession finds the ONE active session keyed on the repo among the
// tasks the caller can reach (owned ∪ shared with me). Several active sessions
// on one key is a state to report, not to guess through — name them and stop.
func lookupRepoSession(ctx context.Context, client SessionClient, key string) (string, error) {
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		return "", err
	}
	var hits []TaskSummary
	for _, t := range tasks {
		if !sessionTaskActive(t) {
			continue
		}
		k := strings.TrimSpace(t.TargetRepo)
		if k == "" {
			continue
		}
		if nk, err := NormalizeRepoKey(k); err == nil {
			k = nk
		}
		if strings.EqualFold(k, key) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return "", nil
	case 1:
		return hits[0].ID, nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Created.Before(hits[j].Created) })
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	return "", fmt.Errorf("session: %d active sessions are keyed on %s (%s); join one explicitly: bashy sprint session join <task-id>", len(hits), key, strings.Join(ids, ", "))
}
