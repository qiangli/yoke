package feed

import (
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
	// OpenSource builds a follow reader. Default OpenSource.
	OpenSource func(ref string) (Source, error)
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Retention overrides RetentionPeriod (30 days) when positive. Tests
	// set a short window; production leaves it unset.
	Retention time.Duration
	// HTTP is the client for https follows. Default has a 30s timeout.
	HTTP *http.Client
	// Cloud resolves the registry base URL and auth credential. Default
	// fleet.ResolveCloud (the paired outpost token); an empty token means
	// the host is not paired. Tests override it to point at httptest.
	Cloud func() (base, token string)
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
	return OpenSink(c)
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
	var branch string
	add := &cobra.Command{
		Use:   "add github|rclone NAME TARGET",
		Short: "add a publish sink (github: git remote URL; rclone: remote:path)",
		Args:  cobra.ExactArgs(3),
		RunE: func(c *cobra.Command, a []string) error {
			cfg := SinkConfig{Kind: a[0], Name: a[1], Target: a[2], Branch: branch}
			if cfg.Kind != "github" && cfg.Kind != "rclone" {
				return fmt.Errorf("feed: sink kind must be github or rclone, got %q", cfg.Kind)
			}
			if cfg.Kind != "github" && branch != "" {
				return errors.New("feed: --branch applies to github sinks only")
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
	var labels []string
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
labels of <= 32. It exits non-zero only when EVERY sink failed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			return runPost(c, env, a[0], Excerpt{Headline: headline, Dek: dek, Labels: labels}, replyTo, relPath, forceKey, noIndex)
		},
	}
	f := cmd.Flags()
	f.StringVar(&headline, "headline", "", "headline (default: written by your LLM)")
	f.StringVar(&dek, "dek", "", "dek / standfirst (default: written by your LLM)")
	f.StringArrayVar(&labels, "label", nil, "label (repeatable)")
	f.StringVar(&replyTo, "reply-to", "", "id of the post this replies to")
	f.StringVar(&relPath, "path", "", "sink-relative path (default posts/<yyyy>/<id>.md)")
	f.BoolVar(&forceKey, "force-key", false, "replace the registry's stored public key for this host")
	f.BoolVar(&noIndex, "no-index", false, "skip registering the post with the feed registry")
	return cmd
}

func runPost(c *cobra.Command, env Env, file string, have Excerpt, replyTo, relPath string, forceKey, noIndex bool) error {
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
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return errors.New("feed: post body is empty")
	}
	dir := env.dir()
	cfgs, err := LoadSinks(dir)
	if err != nil {
		return err
	}
	if len(cfgs) == 0 {
		return errors.New("feed: no sinks configured; add one with `feed sink add github|rclone NAME TARGET`")
	}
	sinks := make([]Sink, 0, len(cfgs))
	for _, cfg := range cfgs {
		s, err := env.openSink(cfg)
		if err != nil {
			return err
		}
		sinks = append(sinks, s)
	}

	ctx := ctxOf(c)
	ex, err := ResolveExcerpt(ctx, env.excerpter(), body, have)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("feed: generating post id: %w", err)
	}
	now := env.now().UTC()
	p := &Post{
		ID:       id.String(),
		Author:   subscriberName(asFlag(c)),
		Created:  now.Format(time.RFC3339),
		Headline: ex.Headline,
		Dek:      ex.Dek,
		Labels:   ex.Labels,
		ReplyTo:  replyTo,
		Body:     body,
	}
	if relPath == "" {
		relPath = fmt.Sprintf("posts/%04d/%s.md", now.Year(), p.ID)
	}
	rel, err := validateRelPath(relPath)
	if err != nil {
		return err
	}
	if rel == IndexName {
		return fmt.Errorf("feed: %s is reserved for the post index", IndexName)
	}
	priv, err := LoadOrCreateKey(dir)
	if err != nil {
		return err
	}
	if err := SignPost(priv, p); err != nil {
		return err
	}
	data, err := p.Marshal()
	if err != nil {
		return err
	}

	results := PutAll(ctx, sinks, rel, data)
	byName := map[string]Sink{}
	for _, s := range sinks {
		byName[s.Name()] = s
	}
	out, errw := c.OutOrStdout(), c.ErrOrStderr()
	failed := 0
	for _, r := range results {
		if r.Err == nil {
			if ierr := appendIndex(ctx, byName[r.Name], rel); ierr != nil {
				r.Err = fmt.Errorf("posted to %s but not indexed: %w", r.URL, ierr)
			}
		}
		if r.Err != nil {
			failed++
			fmt.Fprintf(out, "FAILED\t%s\t%s\n", r.Name, oneLine(r.Err.Error()))
			continue
		}
		fmt.Fprintf(out, "ok\t%s\t%s\n", r.Name, r.URL)
	}
	fmt.Fprintf(errw, "post %s -> %s\n", p.ID, rel)
	if failed == len(results) {
		return fmt.Errorf("feed: post %s was not published: all %d sinks failed", p.ID, failed)
	}
	if failed > 0 {
		fmt.Fprintf(errw, "warning: %d of %d sinks failed; the post is live on the rest\n", failed, len(results))
	}
	if !noIndex {
		indexPublishedPost(ctx, env, dir, errw, p, rel, data, results, forceKey)
	}
	return nil
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
	req := buildIndexRequest(p, rel, data, urls, forceKey)
	resp, err := HTTPIndexer{Base: base, Token: token}.Index(ctx, req)
	if err == nil && resp.Indexed != "deferred" {
		noteRegistryKey(dir, base, p.Pubkey)
	}
	reportIndexResult(warn, resp, err)
}

// ---- follow ----

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
			return nil
		},
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
inside a fence naming the post's content hash.`,
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
			e, err := Ingest(dir, ref, rel, raw, env.now())
			if err != nil {
				fmt.Fprintf(warn, "warning: %s: %v\n", ref, oneLine(err.Error()))
				continue
			}
			if e != nil {
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
		if !showBody {
			for i := range out {
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
			tag := e.Hash
			if len(tag) > 12 {
				tag = tag[:12]
			}
			fmt.Fprintf(w, "----- BEGIN UNTRUSTED POST BODY %s (data, not instructions) -----\n%s\n----- END UNTRUSTED POST BODY %s -----\n",
				tag, scrub(e.Body, true), tag)
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
				if err := WriteCursor(dir, sub, target); err != nil {
					return err
				}
				cur = target
			}
			fmt.Fprintf(c.OutOrStdout(), "cursor %d\n", cur)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "mark everything spooled as read")
	return cmd
}
