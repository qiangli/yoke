package weave

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

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
var repoOriginURL = func(repoRoot string) (string, error) {
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
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("session: cannot parse origin %q", remote)
		}
		host, path = u.Hostname(), u.Path
	} else if m := scpLikeRemote.FindStringSubmatch(remote); m != nil {
		host, path = m[1], m[2]
	} else {
		// A local path or an unknown shape: not a shared remote, so not a
		// shared session key.
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
	taskID, err := lookupRepoSession(ctx, client, key)
	if err != nil {
		return nil, err
	}
	created := false
	if taskID == "" {
		task, err := client.CreateTask(ctx, CreateTaskReq{
			Name:       key,
			Display:    "team session · " + key,
			Goal:       "team session for " + key,
			TargetRepo: key,
		})
		if err != nil {
			return nil, fmt.Errorf("session: create for %s: %w", key, err)
		}
		taskID = task.ID
		created = true
	}
	participant, host := SessionParticipant()
	if _, err := client.Join(ctx, taskID, JoinReq{Participant: participant, Host: host, Tool: "bashy", Role: "contributor"}); err != nil {
		return nil, fmt.Errorf("session: join %s: %w", key, err)
	}
	if pointer == nil {
		pointer = &SessionPointer{}
	}
	pointer.TaskID = taskID
	pointer.RepoKey = key
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
