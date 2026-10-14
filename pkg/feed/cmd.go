package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Env carries the seams `feed` reaches the outside world through. The zero
// value is the production wiring; tests override individual fields.
type Env struct {
	// Dir is the state directory (sinks.json, follows.json, spool, cursors,
	// key). Default KeyDir(), resolved at run time so BASHY_HOME is honoured.
	Dir func() string
	// Excerpter writes missing headline/dek. Default LocalLLM.
	Excerpter Excerpter
	// OpenSink builds a publish sink. Default OpenSink.
	OpenSink func(SinkConfig) (Sink, error)
	// RcloneRun overrides the rclone command runner for sink setup and publishing.
	RcloneRun func(context.Context, ...string) ([]byte, error)
	// OpenSource builds a follow reader. Default OpenSource.
	OpenSource func(ref string) (Source, error)
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Retention overrides RetentionPeriod (30 days) when positive. Tests
	// set a short window; production leaves it unset.
	Retention time.Duration
	// MaxAttachBytes caps one --attach file. Non-positive means
	// DefaultMaxAttachBytes (25 MiB).
	MaxAttachBytes int64
	// HTTP is the client for https follows. Default has a 30s timeout.
	HTTP *http.Client
	// Cloud resolves the registry base URL and auth credential. Default
	// fleet.ResolveCloud (the paired outpost token); an empty token means
	// the host is not paired. Tests override it to point at httptest.
	Cloud func() (base, token string)
	// serverRead returns post ids read elsewhere (the web inbox). nil = none.
	serverRead func(ctx context.Context) (map[string]bool, error)
	// inboxURL is the reader's web inbox, "" when not paired.
	inboxURL func(ctx context.Context) string
	// doctor holds the `feed doctor` probes. Zero = production wiring.
	doctor doctorProbes
}

func (e Env) dir() string {
	if e.Dir != nil {
		return e.Dir()
	}
	return KeyDir()
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) retention() time.Duration {
	if e.Retention > 0 {
		return e.Retention
	}
	return RetentionPeriod
}

func (e Env) openSink(c SinkConfig) (Sink, error) {
	if e.OpenSink != nil {
		return e.OpenSink(c)
	}
	if c.Kind == "rclone" && e.RcloneRun != nil {
		return openRcloneSinkWithRunner(c, e.RcloneRun)
	}
	return OpenSink(c)
}

func (e Env) rcloneRun(ctx context.Context, args ...string) ([]byte, error) {
	if e.RcloneRun != nil {
		return e.RcloneRun(ctx, args...)
	}
	return runRclone(ctx, args...)
}

func (e Env) openSource(ref string) (Source, error) {
	if e.OpenSource != nil {
		return e.OpenSource(ref)
	}
	return OpenSource(ref, e.HTTP)
}

func (e Env) excerpter() Excerpter {
	if e.Excerpter != nil {
		return e.Excerpter
	}
	return LocalLLM{}
}

// NewCmd returns the `feed` command tree wired to production dependencies.
func NewCmd() *cobra.Command { return NewCmdWith(Env{}) }

// NewCmdWith returns the `feed` command tree over env.
func NewCmdWith(env Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "feed",
		Short: "federated signed feed: post, follow, fetch, read",
		Long: `feed publishes signed markdown posts to sinks you own (git/GitHub, rclone
remotes) and reads posts from sources you follow. Nothing runs a server:
sinks are plain files, and a follower reads <base>/index.txt plus the posts it
lists. Fetched bodies are untrusted data and are only printed under --body,
inside a fence.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("as", "", "acting principal (default $BASHY_PRINCIPAL, then $USER)")
	root.AddCommand(
		newSinkCmd(env), newPostCmd(env),
		newFollowCmd(env), newUnfollowCmd(env), newFollowingCmd(env),
		newFetchCmd(env), newReadCmd(env),
		newChannelCmd(env), newDeliverCmd(env), newGroupCmd(env),
		newBridgeCmd(env), newKeyCmd(env), newDoctorCmd(env),
	)
	return root
}

func asFlag(c *cobra.Command) string {
	v, _ := c.Flags().GetString("as")
	return v
}

func ctxOf(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// ---- sink ----

func newSinkCmd(env Env) *cobra.Command {
	sink := &cobra.Command{Use: "sink", Short: "manage publish sinks (sinks.json)"}
	var branch, publicURL string
	add := &cobra.Command{
		Use:   "add github|rclone NAME TARGET",
		Short: "add a publish sink (github: git remote URL; rclone: remote:path)",
		Args:  cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, a []string) error {
			pub, err := normalizePublicURL(publicURL)
			if err != nil {
				return err
			}
			cfg := SinkConfig{Kind: a[0], Name: a[1], Target: a[2], Branch: branch, PublicURL: pub}
			if cfg.Kind != "github" && cfg.Kind != "rclone" {
				return fmt.Errorf("feed: sink kind must be github or rclone, got %q", cfg.Kind)
			}
			if cfg.Kind != "github" && branch != "" {
				return errors.New("feed: --branch applies to github sinks only")
			}
			if cfg.Kind == "rclone" {
				if err := checkRcloneRemote(ctxOf(c), cfg.Target, env.rcloneRun); err != nil {
					return err
				}
			}
			dir := env.dir()
			cfgs, err := LoadSinks(dir)
			if err != nil {
				return err
			}
			if _, err := env.openSink(cfg); err != nil {
				return err
			}
			if err := SaveSinks(dir, append(cfgs, cfg)); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added sink %s (%s)\n", cfg.Name, cfg.Kind)
			return nil
		},
	}
	add.Flags().StringVar(&branch, "branch", "", "git branch for github sinks (default main)")
	add.Flags().StringVar(&publicURL, "public-url", "", "http(s) base URL the sink's files are served from; posts are published as BASE/<path>")
	list := &cobra.Command{
		Use:   "list",
		Short: "list configured sinks",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfgs, err := LoadSinks(env.dir())
			if err != nil {
				return err
			}
			for _, s := range cfgs {
				line := s.Name + "\t" + s.Kind + "\t" + s.Target
				if s.Branch != "" {
					line += "\t" + s.Branch
				}
				if s.PublicURL != "" {
					line += "\tpublic_url=" + s.PublicURL
				}
				fmt.Fprintln(c.OutOrStdout(), line)
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "remove a sink",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			dir := env.dir()
			cfgs, err := LoadSinks(dir)
			if err != nil {
				return err
			}
			var keep []SinkConfig
			for _, s := range cfgs {
				if s.Name != a[0] {
					keep = append(keep, s)
				}
			}
			if len(keep) == len(cfgs) {
				return fmt.Errorf("feed: no sink named %q", a[0])
			}
			if err := SaveSinks(dir, keep); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed sink %s\n", a[0])
			return nil
		},
	}
	sink.AddCommand(add, list, rm)
	return sink
}

// ---- post ----

func newPostCmd(env Env) *cobra.Command {
	var headline, dek, replyTo, relPath string
	var labels, attaches []string
	var forceKey, noIndex bool
	cmd := &cobra.Command{
		Use:   "post FILE|-",
		Short: "sign a markdown body and publish it to every sink",
		Long: `post reads a markdown body from FILE (or stdin for "-"), writes the excerpt
(headline, dek, labels) with your LLM unless you pass them, signs the post, and
publishes it to every configured sink, appending it to each sink's index.txt.
It then registers the post with the feed registry, unless --no-index is set
or the host is not paired.

Budgets are weighted (CJK counts 2): headline <= 90, dek <= 240, up to 8
labels of <= 32. It exits non-zero only when EVERY sink failed.

Repeat --attach to publish files under a per-post directory named after
the post file (post posts/2026/<id>.md stores pic.png at
posts/2026/<id>/pic.png). Each attachment is listed in frontmatter as
"<id>/pic.png" with its sha256 and covered by the signature; link that
path from the body, and fetchers verify every listed attachment and mark
the post drifted on mismatch.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			return runPost(c, env, a[0], Excerpt{Headline: headline, Dek: dek, Labels: labels}, replyTo, relPath, attaches, forceKey, noIndex)
		},
	}
	f := cmd.Flags()
	f.StringVar(&headline, "headline", "", "headline (default: written by your LLM)")
	f.StringVar(&dek, "dek", "", "dek / standfirst (default: written by your LLM)")
	f.StringArrayVar(&labels, "label", nil, "label (repeatable)")
	f.StringArrayVar(&attaches, "attach", nil, "attachment file published under the post's own directory (repeatable)")
	f.StringVar(&replyTo, "reply-to", "", "id of the post this replies to")
	f.StringVar(&relPath, "path", "", "sink-relative path (default posts/<yyyy>/<id>.md)")
	f.BoolVar(&forceKey, "force-key", false, "replace the registry's stored public key for this host")
	f.BoolVar(&noIndex, "no-index", false, "skip registering the post with the feed registry")
	f.String("group", "", "group ID to publish into")
	return cmd
}

func runPost(c *cobra.Command, env Env, file string, have Excerpt, replyTo, relPath string, attaches []string, forceKey, noIndex bool) error {
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(c.InOrStdin())
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return fmt.Errorf("feed: reading post body: %w", err)
	}
	group, _ := c.Flags().GetString("group")
	res, pubErr := Publish(ctxOf(c), env, PublishRequest{
		Body:        string(raw),
		Headline:    have.Headline,
		Dek:         have.Dek,
		Labels:      have.Labels,
		ReplyTo:     replyTo,
		Path:        relPath,
		Group:       group,
		Attachments: attaches,
		Author:      subscriberName(asFlag(c)),
		ForceKey:    forceKey,
		NoIndex:     noIndex,
	})
	PrintPublishResult(c.OutOrStdout(), c.ErrOrStderr(), res)
	return pubErr
}

// indexPublishedPost registers a post that landed on at least one sink with
// the feed registry. Every outcome is a stderr note and a nil return: the
// post is live on the sinks whatever the registry says.
func indexPublishedPost(ctx context.Context, env Env, dir string, warn io.Writer, p *Post, rel string, data []byte, results []SinkResult, forceKey bool) {
	var urls []string
	for _, r := range results {
		if r.Err == nil {
			urls = append(urls, r.URL)
		}
	}
	if len(urls) == 0 {
		return
	}
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	base, token := cloud()
	if token == "" {
		fmt.Fprintln(warn, "index: skipped (host not paired)")
		return
	}
	urls = publicSinkURLs(urls)
	if len(urls) == 0 {
		fmt.Fprintln(warn, "index: skipped (no public sink URL)")
		return
	}
	req := buildIndexRequest(p, rel, data, urls, forceKey)
	if p.Group != "" {
		indexGroupedPublishedPost(ctx, env, dir, warn, base, token, p, req)
		return
	}
	resp, err := HTTPIndexer{Base: base, Token: token}.Index(ctx, req)
	if err == nil && resp.Indexed != "deferred" {
		noteRegistryKey(dir, base, p.Pubkey)
	}
	reportIndexResult(warn, resp, err)
}

// ---- follow ----

const followSyncTimeout = 10 * time.Second

func newFollowCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "follow URL",
		Short: "follow a sink: https base URL, rclone target (remote:path), or local dir",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			ref, err := NormalizeFollow(a[0])
			if err != nil {
				return err
			}
			dir := env.dir()
			cur, err := LoadFollows(dir)
			if err != nil {
				return err
			}
			if err := SaveFollows(dir, append(cur, ref)); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "following %s\n", ref)
			syncFollowIfNeeded(ctxOf(c), env, http.MethodPost, ref, c.ErrOrStderr())
			return nil
		},
	}
}

func newUnfollowCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "unfollow URL",
		Short: "stop following a sink",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			ref, err := NormalizeFollow(a[0])
			if err != nil {
				ref = strings.TrimRight(strings.TrimSpace(a[0]), "/")
			}
			dir := env.dir()
			cur, err := LoadFollows(dir)
			if err != nil {
				return err
			}
			var keep []string
			for _, f := range cur {
				if f != ref {
					keep = append(keep, f)
				}
			}
			if len(keep) == len(cur) {
				return fmt.Errorf("feed: not following %q", ref)
			}
			if err := SaveFollows(dir, keep); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "unfollowed %s\n", ref)
			syncFollowIfNeeded(ctxOf(c), env, http.MethodDelete, ref, c.ErrOrStderr())
			return nil
		},
	}
}

func syncFollowIfNeeded(ctx context.Context, env Env, method, ref string, warn io.Writer) {
	kind, target, ok := parseFollowSyncTarget(ref)
	if !ok {
		return
	}
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	base, token := cloud()
	if token == "" {
		fmt.Fprintln(warn, "warning: not paired; follow not synced to server")
		return
	}
	bodyMap := map[string]string{kind: target}
	data, err := json.Marshal(bodyMap)
	if err != nil {
		fmt.Fprintf(warn, "warning: follow sync failed: %v\n", oneLine(err.Error()))
		return
	}
	u := strings.TrimRight(base, "/") + "/api/v1/feed/follows"
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(warn, "warning: follow sync failed: %v\n", oneLine(err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := env.HTTP
	if client == nil {
		client = &http.Client{Timeout: followSyncTimeout}
	}
	cctx, cancel := context.WithTimeout(ctx, followSyncTimeout)
	defer cancel()
	req = req.WithContext(cctx)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(warn, "warning: follow sync failed: %v\n", oneLine(err.Error()))
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		reason := strings.TrimSpace(string(raw))
		if reason == "" {
			reason = resp.Status
		}
		var m struct {
			Reason  string `json:"reason"`
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &m) == nil {
			for _, s := range []string{m.Reason, m.Error, m.Message} {
				if strings.TrimSpace(s) != "" {
					reason = strings.TrimSpace(s)
					break
				}
			}
		}
		fmt.Fprintf(warn, "warning: follow sync failed: %s: %s\n", resp.Status, oneLine(reason))
		return
	}
}

func newFollowingCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "following",
		Short: "list followed sinks",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cur, err := LoadFollows(env.dir())
			if err != nil {
				return err
			}
			for _, f := range cur {
				fmt.Fprintln(c.OutOrStdout(), f)
			}
			return nil
		},
	}
}

// ---- fetch ----

func newFetchCmd(env Env) *cobra.Command {
	var unread, asJSON, showBody bool
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "pull new posts from every followed sink into the local spool and verify them",
		Long: `fetch reads each followed sink's index.txt, downloads the posts, and verifies
each signature over the body hash and excerpt: verified, drifted (the content
no longer matches its signature, or a known id changed) or unsigned.

By default it lists what this run added; --unread lists everything past your
read cursor instead. Bodies are untrusted: they print only under --body,
inside a fence naming the post's content hash.

Under --json, every entry carries "untrusted": true marking remote content
(headline, author, dek, labels, body) as untrusted data rather than
instructions; post bodies appear only under --body in the separate "body"
field.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dir := env.dir()
			added, err := fetchAll(ctxOf(c), env, dir, c.ErrOrStderr())
			if err != nil {
				return err
			}
			list := added
			if unread {
				cur, err := ReadCursor(dir, subscriberName(asFlag(c)))
				if err != nil {
					return err
				}
				all, err := ReadSpool(dir)
				if err != nil {
					return err
				}
				list = nil
				for _, e := range all {
					if e.Seq > cur {
						list = append(list, e)
					}
				}
			}
			list = decryptGroupEntries(ctxOf(c), env, dir, list)
			return printEntries(c.OutOrStdout(), list, asJSON, showBody)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&unread, "unread", false, "list every spooled post past your read cursor")
	f.BoolVar(&asJSON, "json", false, "emit a JSON array")
	f.BoolVar(&showBody, "body", false, "include post bodies (untrusted; fenced in text output)")
	return cmd
}

func fetchAll(ctx context.Context, env Env, dir string, warn io.Writer) ([]SpoolEntry, error) {
	if _, _, err := PruneRetention(dir, env.now(), env.retention()); err != nil {
		fmt.Fprintf(warn, "warning: pruning expired spool entries: %v\n", oneLine(err.Error()))
	}
	follows, err := LoadFollows(dir)
	if err != nil {
		return nil, err
	}
	if len(follows) == 0 {
		return nil, errors.New("feed: not following anything; use `feed follow URL`")
	}
	var added []SpoolEntry
	failedFollows := 0
	for _, ref := range follows {
		src, err := env.openSource(ref)
		if err != nil {
			failedFollows++
			fmt.Fprintf(warn, "warning: %s: %v\n", ref, err)
			continue
		}
		idx, err := src.Get(ctx, IndexName)
		if err != nil {
			failedFollows++
			fmt.Fprintf(warn, "warning: %s: reading %s: %v\n", ref, IndexName, oneLine(err.Error()))
			continue
		}
		rels, skipped := ParseIndex(idx)
		for _, s := range skipped {
			fmt.Fprintf(warn, "warning: %s: ignoring index entry %q\n", ref, s)
		}
		for _, rel := range rels {
			raw, err := src.Get(ctx, rel)
			if err != nil {
				fmt.Fprintf(warn, "warning: %s: %s: %v\n", ref, rel, oneLine(err.Error()))
				continue
			}
			e, err := ingest(dir, ref, rel, raw, env.now(), attachmentCheck(ctx, src, rel))
			if err != nil {
				fmt.Fprintf(warn, "warning: %s: %v\n", ref, oneLine(err.Error()))
				continue
			}
			if e != nil {
				cacheFetchedGroupPost(dir, e.ID, e.Hash, raw)
				added = append(added, *e)
			}
		}
	}
	if failedFollows == len(follows) {
		return nil, fmt.Errorf("feed: could not read any of the %d followed sinks", failedFollows)
	}
	return added, nil
}

func printEntries(w io.Writer, list []SpoolEntry, asJSON, showBody bool) error {
	if asJSON {
		out := make([]SpoolEntry, len(list))
		copy(out, list)
		for i := range out {
			out[i].Untrusted = true
			if !showBody {
				out[i].Body = ""
			}
		}
		if out == nil {
			out = []SpoolEntry{}
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	for _, e := range list {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", e.Seq, e.Status, oneLine(e.ID), oneLine(e.Author), oneLine(e.Headline))
		if showBody {
			if err := fenceBody(w, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// scrub removes terminal control characters from remote text. Newlines (and
// tabs) survive only when multiline is set.
func scrub(s string, multiline bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			if multiline {
				return r
			}
			return ' '
		case r == '\r':
			return -1
		case unicode.IsControl(r):
			return '?'
		}
		return r
	}, s)
}

func oneLine(s string) string { return scrub(s, false) }

// ---- read ----

func newReadCmd(env Env) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "read [ID|--all]",
		Short: "advance your read cursor past a post (or everything spooled)",
		Long: `read moves your per-subscriber cursor over the fetch spool. The cursor is one
number, so reading a post also marks every earlier spool entry read. ID may be
a unique prefix.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			if all == (len(a) == 1) {
				return errors.New("feed: give a post ID or --all")
			}
			dir := env.dir()
			sub := subscriberName(asFlag(c))
			entries, err := ReadSpool(dir)
			if err != nil {
				return err
			}
			var target int64
			if all {
				for _, e := range entries {
					if e.Seq > target {
						target = e.Seq
					}
				}
			} else {
				ids := map[string]bool{}
				for _, e := range entries {
					if strings.HasPrefix(e.ID, a[0]) {
						ids[e.ID] = true
						if e.Seq > target {
							target = e.Seq
						}
					}
				}
				if len(ids) == 0 {
					return fmt.Errorf("feed: no spooled post matches %q", a[0])
				}
				if len(ids) > 1 {
					return fmt.Errorf("feed: %q matches %d posts; use a longer prefix", a[0], len(ids))
				}
			}
			cur, err := ReadCursor(dir, sub)
			if err != nil {
				return err
			}
			if target > cur {
				oldCur := cur
				if err := WriteCursor(dir, sub, target); err != nil {
					return err
				}
				cur = target
				var covered []string
				seen := map[string]bool{}
				for _, e := range entries {
					if e.Seq > oldCur && e.Seq <= target && e.ID != "" && !seen[e.ID] {
						seen[e.ID] = true
						covered = append(covered, e.ID)
					}
				}
				if len(covered) > 0 {
					ctx := c.Context()
					if ctx == nil {
						ctx = context.Background()
					}
					sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					if err := SyncReadEnv(sctx, env, covered); err != nil {
						fmt.Fprintf(c.ErrOrStderr(), "feed: read sync failed: %v\n", err)
					}
					cancel()
				}
			}
			fmt.Fprintf(c.OutOrStdout(), "cursor %d\n", cur)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "mark everything spooled as read")
	return cmd
}

// ---- group ----

func newGroupCmd(env Env) *cobra.Command {
	group := &cobra.Command{Use: "group", Short: "manage feed groups"}
	var private, public bool
	var title string
	create := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, a []string) error {
		if private && public {
			return errors.New("feed: choose one group preset")
		}
		preset := "public"
		if private {
			preset = "private"
		}
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		var key []byte
		var wrapped string
		if private {
			priv, err := LoadOrCreateEncKey(env.dir())
			if err != nil {
				return err
			}
			if err := client.PutEncKey(ctxOf(c), EncPubkeyB64(priv)); err != nil {
				return err
			}
			key, err = freshGroupKey()
			if err != nil {
				return err
			}
			// The wrap binds the ID, so the client supplies it at creation.
			id, err := uuid.NewV7()
			if err != nil {
				return err
			}
			wrapped, err = Wrap(key, EncPubkeyB64(priv), id.String(), 1, nil)
			if err != nil {
				return err
			}
			g, err := client.CreateGroupWithID(ctxOf(c), id.String(), a[0], title, preset, wrapped)
			if err != nil {
				return err
			}
			if g.ID != id.String() {
				return errors.New("feed: server did not preserve private group ID; cannot use owner wrap")
			}
			if g.OwnerUID != "" {
				_, devices, err := client.WrapDevices(ctxOf(c), g.OwnerUID, key, g.ID, 1)
				if err != nil {
					return err
				}
				for pub, wrap := range devices {
					if err := client.PutDeviceWrap(ctxOf(c), g.ID, 1, g.OwnerUID, pub, wrap); err != nil {
						return err
					}
				}
			}
			if err := SaveGroupKey(env.dir(), g.ID, 1, key); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\n", g.ID, g.Name, preset)
			return nil
		}
		g, err := client.CreateGroup(ctxOf(c), a[0], title, preset, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\n", g.ID, g.Name, preset)
		return nil
	}}
	create.Flags().BoolVar(&private, "private", false, "create a private group")
	create.Flags().BoolVar(&public, "public", false, "create a public group (default)")
	create.Flags().StringVar(&title, "title", "", "display title")
	add := &cobra.Command{Use: "add GROUP UID", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, a []string) error {
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		g, err := client.ResolveGroup(ctxOf(c), a[0])
		if err != nil {
			return err
		}
		wrapped := ""
		var devices map[string]string
		if g.Read == "members" || g.Epoch > 0 {
			key, err := CurrentGroupKey(ctxOf(c), client, env.dir(), g.ID, g.Epoch)
			if err != nil {
				return err
			}
			wrapped, devices, err = client.WrapDevices(ctxOf(c), a[1], key, g.ID, g.Epoch)
			if err != nil {
				return err
			}
		}
		if err := client.AddMemberWithDevices(ctxOf(c), g.ID, a[1], wrapped, devices); err != nil {
			return err
		}
		fmt.Fprintf(c.OutOrStdout(), "added %s to %s\n", a[1], g.ID)
		return nil
	}}
	rm := &cobra.Command{Use: "rm GROUP UID", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, a []string) error {
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		g, err := client.ResolveGroup(ctxOf(c), a[0])
		if err != nil {
			return err
		}
		if g.Read != "members" && g.Epoch == 0 {
			if err := client.RemoveMember(ctxOf(c), g.ID, a[1], 0, nil); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %s from %s\n", a[1], g.ID)
			return nil
		}
		next := g.Epoch + 1
		key, err := freshGroupKey()
		if err != nil {
			return err
		}
		keys := map[string]string{}
		deviceKeys := map[string]map[string]string{}
		found := false
		for _, m := range g.Members {
			uid := m.userID()
			if uid == a[1] {
				found = true
				continue
			}
			if uid == "" {
				return errors.New("feed: member list has empty uid")
			}
			var devices map[string]string
			keys[uid], devices, err = client.WrapDevices(ctxOf(c), uid, key, g.ID, next)
			if err != nil {
				return err
			}
			deviceKeys[uid] = devices
		}
		if !found {
			return fmt.Errorf("feed: %s is not a group member", a[1])
		}
		if err := client.RemoveMemberWithDevices(ctxOf(c), g.ID, a[1], next, keys, deviceKeys); err != nil {
			return err
		}
		if err := SaveGroupKey(env.dir(), g.ID, next, key); err != nil {
			return err
		}
		fmt.Fprintf(c.OutOrStdout(), "removed %s from %s; epoch %d\n", a[1], g.ID, next)
		return nil
	}}
	ls := &cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		groups, err := client.ListGroups(ctxOf(c))
		if err != nil {
			return err
		}
		for _, g := range groups {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%d\n", g.ID, g.Name, g.Read, g.Epoch)
		}
		return nil
	}}
	key := &cobra.Command{Use: "key", Short: "register this host's group encryption key so owners can add you", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		priv, err := LoadOrCreateEncKey(env.dir())
		if err != nil {
			return err
		}
		client, err := NewGroupClient(env)
		if err != nil {
			return err
		}
		pub := EncPubkeyB64(priv)
		if err := client.PutEncKey(ctxOf(c), pub); err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), pub)
		return nil
	}}
	group.AddCommand(create, add, rm, ls, key)
	return group
}

func prepareGroupPost(ctx context.Context, c *cobra.Command, env Env, p *Post) error {
	id, _ := c.Flags().GetString("group")
	return prepareGroupPostFor(ctx, env, id, p)
}

// prepareGroupPostFor is the cobra-free form of prepareGroupPost: an empty
// group id is a no-op so programmatic publishers (Publish) share the path.
func prepareGroupPostFor(ctx context.Context, env Env, id string, p *Post) error {
	if id == "" {
		return nil
	}
	client, err := NewGroupClient(env)
	if err != nil {
		return err
	}
	g, err := client.ResolveGroup(ctx, id)
	if err != nil {
		return err
	}
	if g.ID == "" {
		return errors.New("feed: group response lacks ID")
	}
	if g.Read != "members" && g.Epoch == 0 {
		p.Group = g.ID
		return nil
	}
	key, err := CurrentGroupKey(ctx, client, env.dir(), g.ID, g.Epoch)
	if err != nil {
		return err
	}
	encrypted, err := EncryptPost(p, key, g.ID, g.Epoch, nil)
	if err != nil {
		return err
	}
	*p = *encrypted
	return nil
}
