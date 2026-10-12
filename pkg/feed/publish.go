package feed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PublishRequest is the programmatic form of `feed post`: a markdown body
// plus the excerpt and attachments the CLI flags carry. Body, Headline and
// Dek are required; Labels, ReplyTo, Path, Group and ForceKey are optional.
// NoIndex skips the feed-registry registration, exactly like --no-index.
type PublishRequest struct {
	Body        string
	Headline    string
	Dek         string
	Labels      []string
	ReplyTo     string
	Path        string
	Group       string
	Attachments []string
	Author      string
	ForceKey    bool
	NoIndex     bool
}

// UploadOutcome is one attachment's delivery outcome across every sink.
type UploadOutcome struct {
	Entry   Attachment
	Rel     string
	Results []SinkResult
}

// PublishResult carries everything `feed post` prints: the signed post and
// its sink-relative path, one outcome per sink for the post itself, one
// outcome per attachment, the successful post URLs in sink order, and the
// stderr-grade warning lines in the order the CLI prints them.
type PublishResult struct {
	Post     *Post
	Rel      string
	Results  []SinkResult
	URLs     []string
	Uploads  []UploadOutcome
	Warnings []string
}

// Publish signs a markdown body and publishes it to every configured sink,
// running the same steps as `feed post --attach`: excerpt resolution,
// per-post attachment directory, signing, PutAll, index append, attachment
// upload, and registry indexing. Callers that need the CLI's printed lines
// render them from the returned result; the returned error uses the same
// messages as the CLI (all sinks failed, an attachment reached no sink).
func Publish(ctx context.Context, env Env, req PublishRequest) (*PublishResult, error) {
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, errors.New("feed: post body is empty")
	}
	dir := env.dir()
	cfgs, err := LoadSinks(dir)
	if err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return nil, errors.New("feed: no sinks configured; add one with `feed sink add github|rclone NAME TARGET`")
	}
	sinks := make([]Sink, 0, len(cfgs))
	for _, cfg := range cfgs {
		s, err := env.openSink(cfg)
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, s)
	}

	ex, err := ResolveExcerpt(ctx, env.excerpter(), body, Excerpt{Headline: req.Headline, Dek: req.Dek, Labels: req.Labels})
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("feed: generating post id: %w", err)
	}
	now := env.now().UTC()
	p := &Post{
		ID:       id.String(),
		Author:   subscriberName(req.Author),
		Created:  now.Format(time.RFC3339),
		Headline: ex.Headline,
		Dek:      ex.Dek,
		Labels:   ex.Labels,
		ReplyTo:  req.ReplyTo,
		Body:     body,
	}
	relPath := req.Path
	if relPath == "" {
		relPath = fmt.Sprintf("posts/%04d/%s.md", now.Year(), p.ID)
	}
	rel, err := validateRelPath(relPath)
	if err != nil {
		return nil, err
	}
	if rel == IndexName {
		return nil, fmt.Errorf("feed: %s is reserved for the post index", IndexName)
	}
	attachEntries, uploads, err := collectAttachments(req.Attachments, rel, env.maxAttachBytes())
	if err != nil {
		return nil, err
	}
	p.Attachments = attachEntries
	if err := prepareGroupPostFor(ctx, env, req.Group, p); err != nil {
		return nil, err
	}
	priv, err := LoadOrCreateKey(dir)
	if err != nil {
		return nil, err
	}
	if err := SignPost(priv, p); err != nil {
		return nil, err
	}
	data, err := p.Marshal()
	if err != nil {
		return nil, err
	}

	res := &PublishResult{Post: p, Rel: rel}
	byName := map[string]Sink{}
	for _, s := range sinks {
		byName[s.Name()] = s
	}
	results := PutAll(ctx, sinks, rel, data)
	failed := 0
	for i, r := range results {
		if r.Err == nil {
			if ierr := appendIndex(ctx, byName[r.Name], rel); ierr != nil {
				results[i].Err = fmt.Errorf("posted to %s but not indexed: %w", r.URL, ierr)
				r.Err = results[i].Err
			}
		}
		if r.Err != nil {
			failed++
			continue
		}
		res.URLs = append(res.URLs, r.URL)
	}
	res.Results = results
	if failed == len(results) {
		return res, fmt.Errorf("feed: post %s was not published: all %d sinks failed", p.ID, failed)
	}
	for _, u := range uploads {
		ures := PutAll(ctx, sinks, u.rel, u.data)
		ou := UploadOutcome{Entry: u.entry, Rel: u.rel, Results: ures}
		res.Uploads = append(res.Uploads, ou)
		ufailed := 0
		for _, r := range ures {
			if r.Err != nil {
				ufailed++
			}
		}
		if ufailed == len(ures) {
			return res, fmt.Errorf("feed: post %s published but attachment %q reached no sink", p.ID, u.entry.Path)
		}
		if ufailed > 0 {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("warning: attachment %q missed %d of %d sinks; fetchers there will see drift", u.entry.Path, ufailed, len(ures)))
		}
	}
	if failed > 0 {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("warning: %d of %d sinks failed; the post is live on the rest", failed, len(results)))
	}
	if !req.NoIndex {
		var notes bytes.Buffer
		indexPublishedPost(ctx, env, dir, &notes, p, rel, data, results, req.ForceKey)
		if s := strings.TrimSpace(notes.String()); s != "" {
			res.Warnings = append(res.Warnings, s)
		}
	}
	return res, nil
}

// PrintPublishResult writes a PublishResult the way `feed post` does: one
// ok/FAILED line per sink and attachment on out, the "post ID -> rel" line
// and the warning lines on errw.
func PrintPublishResult(out, errw io.Writer, res *PublishResult) {
	if res == nil || res.Post == nil {
		return
	}
	for _, r := range res.Results {
		if r.Err != nil {
			fmt.Fprintf(out, "FAILED\t%s\t%s\n", r.Name, oneLine(r.Err.Error()))
			continue
		}
		fmt.Fprintf(out, "ok\t%s\t%s\n", r.Name, r.URL)
	}
	fmt.Fprintf(errw, "post %s -> %s\n", res.Post.ID, res.Rel)
	for _, u := range res.Uploads {
		for _, r := range u.Results {
			if r.Err != nil {
				fmt.Fprintf(out, "FAILED\t%s\t%s\t%s\n", r.Name, u.Entry.Path, oneLine(r.Err.Error()))
				continue
			}
			fmt.Fprintf(out, "ok\t%s\t%s\n", r.Name, r.URL)
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(errw, w)
	}
}

// channelSender runs one channel's registered tool command. It is a var so
// tests can observe cross-posts without a real tool backend.
var channelSender = func(ctx context.Context, ch Channel, message string) error {
	return sendChannel(ctx, ch, message)
}

// SendToChannel delivers message through the named channel's registered tool
// command — the same sendChannel path `feed deliver` uses — without touching
// delivery state. It is the immediate cross-post primitive: the caller owns
// the message (for example a freshly published post's headline plus its
// viewer link), and the channel owns the transport.
func SendToChannel(ctx context.Context, dir, name, message string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("feed: channel name is required")
	}
	if strings.TrimSpace(message) == "" {
		return errors.New("feed: channel message is empty")
	}
	chans, err := LoadChannels(dir)
	if err != nil {
		return err
	}
	for _, ch := range chans {
		if ch.Name != name {
			continue
		}
		if ch.Off {
			return fmt.Errorf("feed: channel %q is off", name)
		}
		return channelSender(ctx, ch, message)
	}
	return fmt.Errorf("feed: no channel named %q", name)
}
