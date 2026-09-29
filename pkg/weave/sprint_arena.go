package weave

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/external/loom"
	"github.com/spf13/cobra"
)

type arenaRepo struct {
	Repo string `json:"repo"`
	Base string `json:"base"`
}
type sprintArena struct {
	Repos  []arenaRepo `json:"repos"`
	Digest string      `json:"digest"`
}
type arenaSource struct{ Name, Root, SHA string }
type arenaBackend interface {
	EnsureOrg(string) error
	EnsureRepo(string, string) error
	PushBase(string, string, string, string) error
	Bundle(string, string, string) error
	DeleteRepo(string, string) error
	DeleteOrg(string) error
}

func arenaOrg(id int64) string { return fmt.Sprintf("sprint-%d", id) }

func arenaDigest(repos []arenaRepo) string {
	lines := make([]string, 0, len(repos))
	for _, r := range repos {
		lines = append(lines, r.Repo+" "+r.Base+"\n")
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
}

func arenaUp(s *weaveStory, sources []arenaSource, backend arenaBackend, rebase bool) error {
	if len(sources) == 0 {
		return fmt.Errorf("sprint #%d has no tracked or linked repositories", s.ID)
	}
	repos := make([]arenaRepo, 0, len(sources))
	seen := map[string]string{}
	for _, source := range sources {
		if source.Name == "" || source.Name != filepath.Base(source.Name) || source.Name == "." || source.Name == ".." {
			return fmt.Errorf("invalid arena repo name %q", source.Name)
		}
		if len(source.SHA) != 40 {
			return fmt.Errorf("invalid base SHA for %s", source.Name)
		}
		if old, ok := seen[source.Name]; ok {
			if old != source.Root {
				return fmt.Errorf("multiple checkouts named %s", source.Name)
			}
			continue
		}
		seen[source.Name] = source.Root
		repos = append(repos, arenaRepo{Repo: source.Name, Base: source.SHA})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Repo < repos[j].Repo })
	digest := arenaDigest(repos)
	if s.Arena != nil {
		if s.Arena.Digest == digest {
			return nil
		}
		if !rebase {
			return fmt.Errorf("arena base changed; use --rebase to pin a new base")
		}
	}
	org := arenaOrg(s.ID)
	if err := backend.EnsureOrg(org); err != nil {
		return err
	}
	for _, r := range repos {
		if err := backend.EnsureRepo(org, r.Repo); err != nil {
			return err
		}
		if err := backend.PushBase(org, r.Repo, seen[r.Repo], r.Base); err != nil {
			return err
		}
	}
	s.Arena = &sprintArena{Repos: repos, Digest: digest}
	weaveStoryAppend(s, weaveConductorName(""), "arena", digest)
	return nil
}

func arenaDown(s *weaveStory, store string, backend arenaBackend) error {
	if s.Arena == nil {
		return nil
	}
	org := arenaOrg(s.ID)
	var paths []string
	for _, r := range s.Arena.Repos {
		rel := filepath.Join("arena", strconv.FormatInt(s.ID, 10), r.Repo+".bundle")
		path := filepath.Join(store, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := backend.Bundle(org, r.Repo, path); err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
	}
	for _, r := range s.Arena.Repos {
		if err := backend.DeleteRepo(org, r.Repo); err != nil {
			return err
		}
	}
	if err := backend.DeleteOrg(org); err != nil {
		return err
	}
	weaveStoryAppend(s, weaveConductorName(""), "arena-down", strings.Join(paths, "\n"))
	s.Arena = nil
	return nil
}

func arenaWriteStatus(w io.Writer, s *weaveStory, baseURL string, running, asJSON bool) error {
	orgURL := strings.TrimRight(baseURL, "/") + "/" + arenaOrg(s.ID)
	var repos []arenaRepo
	var digest string
	if s.Arena != nil {
		repos = s.Arena.Repos
		digest = s.Arena.Digest
	}
	if asJSON {
		return json.NewEncoder(w).Encode(map[string]any{"sprint": s.ID, "org_url": orgURL, "repos": repos, "digest": digest, "running": running})
	}
	_, err := fmt.Fprintf(w, "arena %s running=%t digest=%s\n", orgURL, running, digest)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if _, err := fmt.Fprintf(w, "  %s %s\n", r.Repo, r.Base); err != nil {
			return err
		}
	}
	return nil
}

func arenaDefaultSHA(root string) (string, error) {
	r, err := gogit.PlainOpen(root)
	if err != nil {
		return "", err
	}
	for _, name := range []plumbing.ReferenceName{"refs/remotes/origin/HEAD", "refs/heads/main", "refs/heads/master", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
		ref, err := r.Reference(name, true)
		if err == nil && !ref.Hash().IsZero() {
			return ref.Hash().String(), nil
		}
	}
	return "", fmt.Errorf("default branch unknown in %s", root)
}

func arenaSources(s *weaveStory) ([]arenaSource, error) {
	roots := map[string]bool{}
	for _, root := range s.StoryRoots {
		roots[root] = true
	}
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			return nil, err
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return nil, err
		}
		if q.Root == "" {
			return nil, fmt.Errorf("run %s#%d has no repo root", run.Repo, run.ID)
		}
		roots[q.Root] = true
	}
	var out []arenaSource
	for root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		sha, err := arenaDefaultSHA(abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", abs, err)
		}
		out = append(out, arenaSource{Name: filepath.Base(abs), Root: abs, SHA: sha})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func arenaCard(id int64) (*weaveStory, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	s := findWeaveStory(q, id)
	if s == nil {
		return nil, fmt.Errorf("sprint #%d not found", id)
	}
	return s, nil
}

func newSprintArenaCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "arena", Short: "Manage a sprint's loom arena"}
	up := &cobra.Command{Use: "up N", Args: cobra.ExactArgs(1)}
	var rebase bool
	up.Flags().BoolVar(&rebase, "rebase", false, "pin a changed base")
	up.Flags().Bool("override", false, "operator override")
	up.Flags().String("reason", "", "override reason")
	up.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		s, err := arenaCard(id)
		if err != nil {
			return err
		}
		sources, err := arenaSources(s)
		if err != nil {
			return err
		}
		st, running, err := loom.ArenaState(cmd.Context())
		if err != nil {
			return err
		}
		if !running {
			st, err = loom.StartDaemon(cmd.Context(), loom.Options{})
			if err != nil {
				return err
			}
		}
		client := loom.ArenaClient{URL: st.URL, Context: cmd.Context()}
		dir, err := sprintStoreDir()
		if err != nil {
			return err
		}
		err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
			card := findWeaveStory(q, id)
			if card == nil {
				return fmt.Errorf("sprint #%d not found", id)
			}
			if err := authorizeSprintLeaseToken(cmd, card, "arena up"); err != nil {
				return err
			}
			return arenaUp(card, sources, client, rebase)
		})
		if err != nil {
			return err
		}
		s, err = arenaCard(id)
		if err != nil {
			return err
		}
		return arenaWriteStatus(cmd.OutOrStdout(), s, st.RootURL, true, false)
	}
	status := &cobra.Command{Use: "status N", Args: cobra.ExactArgs(1)}
	var jsonOutput bool
	status.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	status.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		s, err := arenaCard(id)
		if err != nil {
			return err
		}
		st, running, err := loom.ArenaState(cmd.Context())
		if err != nil {
			return err
		}
		base := st.RootURL
		if base == "" {
			base = st.URL
		}
		if base == "" {
			base = fmt.Sprintf("http://%s:%d", loom.DefaultAddr, loom.DefaultPort)
		}
		return arenaWriteStatus(cmd.OutOrStdout(), s, base, running, jsonOutput)
	}
	down := &cobra.Command{Use: "down N", Args: cobra.ExactArgs(1)}
	down.Flags().Bool("override", false, "operator override")
	down.Flags().String("reason", "", "override reason")
	down.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		store, err := sprintStoreDir()
		if err != nil {
			return err
		}
		st, running, err := loom.ArenaState(cmd.Context())
		if err != nil {
			return err
		}
		if !running {
			return fmt.Errorf("loom is not running")
		}
		client := loom.ArenaClient{URL: st.URL, Context: cmd.Context()}
		err = withWeaveQueueLock(store, func(q *weaveQueue) error {
			card := findWeaveStory(q, id)
			if card == nil {
				return fmt.Errorf("sprint #%d not found", id)
			}
			if err := authorizeSprintLeaseToken(cmd, card, "arena down"); err != nil {
				return err
			}
			return arenaDown(card, store, client)
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "arena %s archived under arena/%d and removed\n", arenaOrg(id), id)
		return err
	}
	cmd.AddCommand(up, status, down)
	return cmd
}
