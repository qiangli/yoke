package loom

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	gogitcfg "github.com/go-git/go-git/v5/config"
	"github.com/spf13/cobra"

	ygit "github.com/qiangli/yoke/git"
)

// One-way mirror primary -> replicas (docs/selvedge-self-hosted-git.md): the
// primary pushes every commit to read-only replicas through Gitea's standard
// push mirrors; clients read from a replica and push to the primary.

const (
	cfgSection = "loom"
	cfgReplica = "replica"
	cfgPrimary = "primary"
)

// giteaAPI is a minimal Gitea REST client.
type giteaAPI struct {
	base  string
	token string
	hc    *http.Client
}

func newGiteaAPI(base, token string) *giteaAPI {
	return &giteaAPI{base: strings.TrimRight(base, "/"), token: token, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (a *giteaAPI) do(ctx context.Context, method, p string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/api/v1"+p, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "token "+a.token)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, p, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

type repoInfo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "User" or "Organization"
	} `json:"owner"`
}

type pushMirror struct {
	RemoteName    string `json:"remote_name"`
	RemoteAddress string `json:"remote_address"`
	SyncOnCommit  bool   `json:"sync_on_commit"`
	Interval      string `json:"interval"`
	LastError     string `json:"last_error"`
}

// ReplicaOptions configures ReplicaAdd/List/Rm.
type ReplicaOptions struct {
	Primary      string // primary base URL
	Token        string // primary API token
	Replica      string // replica base URL
	ReplicaToken string // replica API token (also the mirror's push credential)
	ReplicaUser  string // username paired with ReplicaToken on mirror pushes
	Repo         string // owner/name; empty = every repo
}

func (o ReplicaOptions) repos(ctx context.Context, p *giteaAPI) ([]repoInfo, error) {
	if o.Repo != "" {
		var r repoInfo
		if _, err := p.do(ctx, "GET", "/repos/"+o.Repo, nil, &r); err != nil {
			return nil, err
		}
		return []repoInfo{r}, nil
	}
	var all []repoInfo
	for page := 1; ; page++ {
		var res struct {
			Data []repoInfo `json:"data"`
		}
		if _, err := p.do(ctx, "GET", fmt.Sprintf("/repos/search?limit=50&page=%d", page), nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Data...)
		if len(res.Data) < 50 {
			return all, nil
		}
	}
}

func replicaRepoURL(base, fullName string) string {
	return strings.TrimRight(base, "/") + "/" + fullName + ".git"
}

// randomPassword is a throwaway credential for a replica-side owner that never
// logs in: mirror pushes authenticate with the replica API token, not this.
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureReplicaOwner makes the repo's owner exist on the replica, as the SAME
// kind it is on the primary. A user-owned repo cannot be hung off an org: the
// replica needs the user, and `POST /orgs` with a name an account already holds
// fails 422 ("user already exists").
func ensureReplicaOwner(ctx context.Context, r *giteaAPI, owner string, isUser bool) error {
	if isUser {
		if _, err := r.do(ctx, "GET", "/users/"+owner, nil, nil); err == nil {
			return nil
		}
		pw, err := randomPassword()
		if err != nil {
			return err
		}
		_, err = r.do(ctx, "POST", "/admin/users", map[string]any{
			"username":             owner,
			"email":                owner + "@loom.invalid",
			"password":             pw,
			"must_change_password": false,
		}, nil)
		return err
	}
	if _, err := r.do(ctx, "GET", "/orgs/"+owner, nil, nil); err == nil {
		return nil
	}
	_, err := r.do(ctx, "POST", "/orgs", map[string]any{"username": owner}, nil)
	return err
}

func ensureReplicaRepo(ctx context.Context, r *giteaAPI, ri repoInfo) (created bool, err error) {
	code, err := r.do(ctx, "GET", "/repos/"+ri.FullName, nil, nil)
	if err == nil {
		return false, nil
	}
	if code != http.StatusNotFound {
		return false, err
	}
	owner, name, _ := strings.Cut(ri.FullName, "/")
	isUser := !strings.EqualFold(ri.Owner.Type, "Organization")
	if err := ensureReplicaOwner(ctx, r, owner, isUser); err != nil {
		return false, fmt.Errorf("replica owner %q: %w", owner, err)
	}
	body := map[string]any{"name": name, "private": ri.Private}
	// A user-owned repo is created on the owner's behalf through the admin API;
	// an org-owned one through the org.
	if isUser {
		_, err = r.do(ctx, "POST", "/admin/users/"+owner+"/repos", body, nil)
	} else {
		_, err = r.do(ctx, "POST", "/orgs/"+owner+"/repos", body, nil)
	}
	return err == nil, err
}

// syncMirror asks the primary to push now. Without it a freshly added mirror
// stays EMPTY until the next commit (sync_on_commit) or the 8h interval — so a
// client that clones the replica right after `replica add` gets nothing.
func syncMirror(ctx context.Context, p *giteaAPI, fullName string) error {
	_, err := p.do(ctx, "POST", "/repos/"+fullName+"/push_mirrors-sync", nil, nil)
	return err
}

func listMirrors(ctx context.Context, p *giteaAPI, fullName string) ([]pushMirror, error) {
	var ms []pushMirror
	_, err := p.do(ctx, "GET", "/repos/"+fullName+"/push_mirrors", nil, &ms)
	return ms, err
}

// ReplicaAdd ensures each repo exists on the replica and has a sync-on-commit
// push mirror to it. Idempotent. Returns one line per repo.
func ReplicaAdd(ctx context.Context, o ReplicaOptions) ([]string, error) {
	p, r := newGiteaAPI(o.Primary, o.Token), newGiteaAPI(o.Replica, o.ReplicaToken)
	repos, err := o.repos(ctx, p)
	if err != nil {
		return nil, err
	}
	user := o.ReplicaUser
	if user == "" {
		user = "oauth2"
	}
	var lines []string
	for _, ri := range repos {
		created, err := ensureReplicaRepo(ctx, r, ri)
		if err != nil {
			return lines, fmt.Errorf("%s: replica repo: %w", ri.FullName, err)
		}
		addr := replicaRepoURL(o.Replica, ri.FullName)
		ms, err := listMirrors(ctx, p, ri.FullName)
		if err != nil {
			return lines, fmt.Errorf("%s: list mirrors: %w", ri.FullName, err)
		}
		have := false
		for _, m := range ms {
			if m.RemoteAddress == addr {
				have = true
			}
		}
		state := "exists"
		if !have {
			status, err := p.do(ctx, "POST", "/repos/"+ri.FullName+"/push_mirrors", map[string]any{
				"remote_address":  addr,
				"remote_username": user,
				"remote_password": o.ReplicaToken,
				"interval":        "8h0m0s",
				"sync_on_commit":  true,
			}, nil)
			if err != nil {
				if status == http.StatusUnauthorized {
					return lines, fmt.Errorf("%s: create mirror: %w\n\n%s", ri.FullName, err, localNetworkHint(addr))
				}
				return lines, fmt.Errorf("%s: create mirror: %w", ri.FullName, err)
			}
			state = "mirrored"
		}
		if err := syncMirror(ctx, p, ri.FullName); err != nil {
			return lines, fmt.Errorf("%s: sync mirror: %w", ri.FullName, err)
		}
		state += " (sync requested)"
		if created {
			state += " (replica repo created)"
		}
		lines = append(lines, fmt.Sprintf("%s -> %s: %s", ri.FullName, addr, state))
	}
	return lines, nil
}

// localNetworkHint explains the 401 Gitea answers for a push mirror to a
// private or loopback replica. The status reads like a credential problem and
// is not one — HandleRemoteAddressError maps its
// ErrInvalidCloneAddr{IsPermissionDenied} to 401, and a correct token fails
// identically — so the error has to name the knob or the next hour goes into
// re-minting tokens. Printed for any 401 from this call: a genuinely bad token
// lands here too, and the first line says which to check.
func localNetworkHint(addr string) string {
	host := addr
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		host = u.Host
	}
	return fmt.Sprintf(`hint: if the primary's token is good, this 401 is not about credentials.
Gitea refuses a push mirror to a private or loopback address unless its
app.ini carries [migrations] ALLOW_LOCALNETWORKS = true, and reports that
refusal as 401 "Permission denied". Every mesh-local replica is such an
address. Set %s on the PRIMARY loom and restart it
(bashy loom stop && bashy loom start) — loom writes the key, and the
allow-list that bounds it, on start:

    %s=%s`, MirrorAllowedDomainsEnv, MirrorAllowedDomainsEnv, host)
}

// ReplicaList lists push mirrors (optionally only those to o.Replica).
func ReplicaList(ctx context.Context, o ReplicaOptions) ([]string, error) {
	p := newGiteaAPI(o.Primary, o.Token)
	repos, err := o.repos(ctx, p)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, ri := range repos {
		ms, err := listMirrors(ctx, p, ri.FullName)
		if err != nil {
			return lines, err
		}
		for _, m := range ms {
			if o.Replica != "" && !strings.HasPrefix(m.RemoteAddress, strings.TrimRight(o.Replica, "/")+"/") {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s -> %s sync_on_commit=%t interval=%s error=%q", ri.FullName, m.RemoteAddress, m.SyncOnCommit, m.Interval, m.LastError))
		}
	}
	return lines, nil
}

// ReplicaRm deletes the push mirrors pointing at o.Replica.
func ReplicaRm(ctx context.Context, o ReplicaOptions) ([]string, error) {
	p := newGiteaAPI(o.Primary, o.Token)
	repos, err := o.repos(ctx, p)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimRight(o.Replica, "/") + "/"
	var lines []string
	for _, ri := range repos {
		ms, err := listMirrors(ctx, p, ri.FullName)
		if err != nil {
			return lines, err
		}
		for _, m := range ms {
			if !strings.HasPrefix(m.RemoteAddress, prefix) {
				continue
			}
			if _, err := p.do(ctx, "DELETE", "/repos/"+ri.FullName+"/push_mirrors/"+url.PathEscape(m.RemoteName), nil, nil); err != nil {
				return lines, err
			}
			lines = append(lines, fmt.Sprintf("%s -> %s: removed", ri.FullName, m.RemoteAddress))
		}
	}
	return lines, nil
}

// replicaReachable is the fast liveness probe: http(s) bases answer
// /api/healthz (<500), file:// bases must be an existing directory.
func replicaReachable(ctx context.Context, base string, timeout time.Duration) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	if u.Scheme == "file" {
		st, err := os.Stat(fileURLPath(u))
		return err == nil && st.IsDir()
	}
	return healthy(ctx, strings.TrimRight(base, "/")+"/api/healthz", timeout)
}

func fileURLPath(u *url.URL) string {
	p := u.Path
	if len(p) > 2 && p[0] == '/' && p[2] == ':' { // /C:/dir on Windows
		p = p[1:]
	}
	return p
}

// repoSuffix is the trailing owner/name.git of a repo URL; a replica serves
// the same owner/name under its own base.
func repoSuffix(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", err
	}
	segs := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("loom: %q is not an owner/name repo URL", repoURL)
	}
	return strings.Join(segs[len(segs)-2:], "/"), nil
}

// pickRemote returns the first reachable replica's repo URL, else the primary.
func pickRemote(ctx context.Context, primary string, replicas []string) (string, error) {
	suf, err := repoSuffix(primary)
	if err != nil {
		return "", err
	}
	for _, r := range replicas {
		if replicaReachable(ctx, r, 2*time.Second) {
			return strings.TrimRight(r, "/") + "/" + suf, nil
		}
	}
	return primary, nil
}

func setRemote(dir, fetchURL, primary string, replicas []string) error {
	repo, err := ygit.OpenRepository(dir)
	if err != nil {
		return err
	}
	cfg, err := repo.Config()
	if err != nil {
		return err
	}
	rc, ok := cfg.Remotes["origin"]
	if !ok {
		rc = &gogitcfg.RemoteConfig{Name: "origin", Fetch: []gogitcfg.RefSpec{"+refs/heads/*:refs/remotes/origin/*"}}
		cfg.Remotes["origin"] = rc
	}
	rc.URLs = []string{fetchURL}
	sub := cfg.Raw.Section("remote").Subsection("origin")
	sub.SetOption("pushurl", primary)
	l := cfg.Raw.Section(cfgSection)
	l.SetOption(cfgPrimary, primary)
	l.RemoveOption(cfgReplica)
	for _, r := range replicas {
		l.AddOption(cfgReplica, r)
	}
	return repo.SetConfig(cfg)
}

func readLoomConfig(dir string) (primary string, replicas []string, err error) {
	repo, err := ygit.OpenRepository(dir)
	if err != nil {
		return "", nil, err
	}
	cfg, err := repo.Config()
	if err != nil {
		return "", nil, err
	}
	l := cfg.Raw.Section(cfgSection)
	primary = l.Option(cfgPrimary)
	for _, o := range l.Options {
		if o.Key == cfgReplica {
			replicas = append(replicas, o.Value)
		}
	}
	if primary == "" {
		return "", nil, fmt.Errorf("loom: %s has no loom.primary (not cloned with `loom clone`)", dir)
	}
	return primary, replicas, nil
}

// Clone clones primaryURL from the first reachable replica (else the primary),
// then points origin at that replica for fetch and at the primary for push.
func Clone(ctx context.Context, primaryURL string, replicas []string, dir string, auth ygit.AuthConfig) (string, error) {
	from, err := pickRemote(ctx, primaryURL, replicas)
	if err != nil {
		return "", err
	}
	if dir == "" {
		dir = strings.TrimSuffix(path.Base(strings.TrimRight(primaryURL, "/")), ".git")
	}
	if _, err := ygit.Clone(ygit.CloneOptions{URL: from, Path: dir, Auth: auth}); err != nil {
		return "", err
	}
	if err := setRemote(dir, from, primaryURL, replicas); err != nil {
		return "", err
	}
	return from, nil
}

// RemoteRefresh re-picks origin's fetch URL (replicas in order, then the
// primary); pushurl stays the primary.
func RemoteRefresh(ctx context.Context, dir string) (string, error) {
	primary, replicas, err := readLoomConfig(dir)
	if err != nil {
		return "", err
	}
	from, err := pickRemote(ctx, primary, replicas)
	if err != nil {
		return "", err
	}
	if err := setRemote(dir, from, primary, replicas); err != nil {
		return "", err
	}
	return from, nil
}

func newReplicaCmd() *cobra.Command {
	var o ReplicaOptions
	var data string
	root := &cobra.Command{Use: "replica", Short: "Manage read replicas (one-way Gitea push mirrors from this primary)"}
	prep := func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			o.Replica = args[0]
		}
		if o.Primary == "" {
			lo := Options{DataDir: data}
			lo.defaults()
			st, err := readState(lo.DataDir)
			if err != nil {
				return fmt.Errorf("loom: no --primary and no local loom state: %w", err)
			}
			o.Primary = st.URL
		}
		if o.ReplicaToken == "" {
			o.ReplicaToken = o.Token
		}
		return nil
	}
	emit := func(cmd *cobra.Command, lines []string, err error) error {
		for _, l := range lines {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		return err
	}
	mk := func(use, short string, nargs cobra.PositionalArgs, fn func(context.Context, ReplicaOptions) ([]string, error)) *cobra.Command {
		// SilenceUsage: a failure here is a runtime one (a forge said no), not a
		// mis-typed command, and cobra's usage block otherwise scrolls the
		// explanation — the 401 local-network hint — off the operator's screen.
		// Measured live in round 4 before this line existed.
		return &cobra.Command{Use: use, Short: short, Args: nargs, SilenceUsage: true,
			PreRunE: func(c *cobra.Command, a []string) error { return prep(c, a) },
			RunE: func(c *cobra.Command, _ []string) error {
				lines, err := fn(c.Context(), o)
				return emit(c, lines, err)
			}}
	}
	add := mk("add <replica-base-url>", "Ensure repos on the replica and create sync-on-commit push mirrors", cobra.ExactArgs(1), ReplicaAdd)
	list := mk("list [replica-base-url]", "List push mirrors", cobra.MaximumNArgs(1), ReplicaList)
	rm := mk("rm <replica-base-url>", "Remove push mirrors to a replica", cobra.ExactArgs(1), ReplicaRm)
	for _, c := range []*cobra.Command{add, list, rm} {
		c.Flags().StringVar(&o.Primary, "primary", "", "primary base URL (default: the local loom)")
		c.Flags().StringVar(&data, "data", "", "local loom work dir (default ~/.agents/bashy/loom)")
		c.Flags().StringVar(&o.Token, "token", os.Getenv("LOOM_TOKEN"), "primary Gitea API token ($LOOM_TOKEN)")
		c.Flags().StringVar(&o.ReplicaToken, "replica-token", os.Getenv("LOOM_REPLICA_TOKEN"), "replica Gitea API token (default: --token)")
		c.Flags().StringVar(&o.ReplicaUser, "replica-user", "", "username for mirror pushes to the replica (default oauth2)")
		c.Flags().StringVar(&o.Repo, "repo", "", "only this owner/name (default: every repo)")
	}
	root.AddCommand(add, list, rm)
	return root
}

func newCloneCmd() *cobra.Command {
	var replicas []string
	var token, user string
	c := &cobra.Command{
		Use:   "clone <primary-repo-url> [dir]",
		Short: "Clone from the first reachable replica; push goes to the primary",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) > 1 {
				dir = args[1]
			}
			auth := ygit.AuthConfig{}
			if token != "" {
				if user == "" {
					user = "oauth2"
				}
				auth = ygit.AuthConfig{Username: user, Password: token}
			}
			from, err := Clone(cmd.Context(), args[0], replicas, dir, auth)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cloned from %s (push -> %s)\n", from, args[0])
			return nil
		},
	}
	c.Flags().StringArrayVar(&replicas, "replica", nil, "replica base URL, in preference order (repeatable)")
	c.Flags().StringVar(&token, "token", os.Getenv("LOOM_TOKEN"), "API token for HTTP git auth")
	c.Flags().StringVar(&user, "user", "", "username for --token (default oauth2)")
	return c
}

func newRemoteCmd() *cobra.Command {
	root := &cobra.Command{Use: "remote", Short: "Replica-aware origin management"}
	var dir string
	refresh := &cobra.Command{
		Use:   "refresh",
		Short: "Re-pick origin's fetch URL: next reachable replica, then the primary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, err := RemoteRefresh(cmd.Context(), dir)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "origin fetch url: %s\n", from)
			return nil
		},
	}
	refresh.Flags().StringVarP(&dir, "dir", "C", ".", "repository directory")
	root.AddCommand(refresh)
	return root
}
