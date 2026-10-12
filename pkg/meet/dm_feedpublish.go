package meet

// DM-to-feed publishing is the minimum glue between three existing pieces:
// the DM front door (dm_frontdoor.go), the shared approval queue (pkg/ask)
// and the feed's post --attach + channel delivery path (pkg/feed). No new
// queue, no new channel type, no new transport.
//
// The flow: a paired sender's chat message arrives with downloaded photo
// paths (--attach, see runDMFrontDoor); the agent drafts a post and proposes
// it with `meet dm-post`, which raises ONE pkg/ask approval whose summary
// shows headline, dek, photo names and destinations. Nothing publishes until
// a sender with approve scope says /approve in the originating chat; the
// front door's /approve path then settles the approval by running the
// existing feed Publish path with the approved attachments, sending the
// headline plus viewer link to each named destination through that
// destination channel's own openclaw:send command, and printing the viewer
// link as the chat reply. /deny drops the draft and publishes nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/feed"
)

// FeedPublishSource is the pkg/ask approval source for a DM-proposed feed
// post. It shares the one approval queue, id space and reaper with every
// other approval; the source is what tells the /approve path that settling
// this item must also publish.
const FeedPublishSource = "feed-publish"

// feedPublishCmdName is the only channel transport this flow delivers
// through: the destination channel must be configured with exactly this
// registered tool command (the first-release chat set — Telegram, Discord,
// Slack, Matrix chat — is delivered this way).
const feedPublishCmdName = "openclaw:send"

// FeedDraft is a proposed cross-post: the body plus the excerpt the agent
// wrote, the photo files OpenClaw downloaded on this host, and the named
// feed channels to cross-post to after approval.
type FeedDraft struct {
	Channel      string
	Sender       string
	Body         string
	Headline     string
	Dek          string
	Labels       []string
	Attachments  []string
	Destinations []string
}

// human names the originating chat identity, the same way the front door
// does, so the published post's author traces back to the chat sender.
func (d FeedDraft) human() string { return dmChannelHuman(d.Channel, d.Sender) }

// photoNames reduces attachment host paths to bare file names for display.
// The publish path re-derives the same basenames from the same paths.
func (d FeedDraft) photoNames() []string {
	out := make([]string, 0, len(d.Attachments))
	for _, p := range d.Attachments {
		out = append(out, filepath.Base(filepath.FromSlash(p)))
	}
	return out
}

// feedDraftSummary is the human-readable draft: headline, dek, photo names
// and destinations, plus a short body lead. The propose command prints it
// (the agent shows it back in the chat) and proposeFeedPost folds it into
// the approval prompt.
func feedDraftSummary(d FeedDraft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "feed post draft from %s on %s\n", d.Sender, d.Channel)
	fmt.Fprintf(&b, "Headline: %s\n", d.Headline)
	fmt.Fprintf(&b, "Dek: %s\n", d.Dek)
	fmt.Fprintf(&b, "Photos: %s\n", strings.Join(d.photoNames(), ", "))
	fmt.Fprintf(&b, "To: %s (via %s)\n", strings.Join(d.Destinations, ", "), feedPublishCmdName)
	lead := strings.TrimSpace(d.Body)
	if len([]rune(lead)) > 300 {
		lead = string([]rune(lead)[:300]) + "…"
	}
	fmt.Fprintf(&b, "Body: %s", lead)
	return b.String()
}

// attachmentDigest binds one approved photo to its bytes: the hex sha256 of
// the file at propose time. Settle recomputes it and refuses to publish
// when a file changed under the pending approval.
func attachmentDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("meet: reading attachment %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// --- seams (production wiring; tests stub them) ----------------------------

// feedChannels reports every configured feed channel as name -> tool command.
var feedChannels = func() (map[string]feed.Channel, error) {
	cs, err := feed.LoadChannels(feed.KeyDir())
	if err != nil {
		return nil, err
	}
	out := make(map[string]feed.Channel, len(cs))
	for _, c := range cs {
		out[c.Name] = c
	}
	return out, nil
}

// feedPublishPost runs the existing feed post path with the approved plan
// and returns the post id plus the viewer link (the first sink URL).
var feedPublishPost = func(ctx context.Context, d FeedDraft) (postID, viewerURL string, err error) {
	res, err := feed.Publish(ctx, feed.Env{}, feed.PublishRequest{
		Body: d.Body, Headline: d.Headline, Dek: d.Dek, Labels: d.Labels,
		Attachments: d.Attachments, Author: d.human(),
	})
	if err != nil {
		return "", "", err
	}
	if len(res.URLs) > 0 {
		viewerURL = res.URLs[0]
	}
	return res.Post.ID, viewerURL, nil
}

// feedSendToChannel delivers one cross-post through the named channel.
var feedSendToChannel = func(ctx context.Context, dest, message string) error {
	return feed.SendToChannel(ctx, feed.KeyDir(), dest, message)
}

// --- propose ---------------------------------------------------------------

// proposeFeedPost validates a draft and raises it as one pkg/ask approval.
// The approval's argv IS the approved plan (headline, dek, labels, exact
// host paths, destinations, body), so its token binds the later /approve to
// exactly this plan; Effects carries the destinations and per-photo digests
// for the human summary line.
func proposeFeedPost(d FeedDraft) (ask.Request, error) {
	d.Channel = strings.TrimSpace(d.Channel)
	d.Sender = strings.TrimSpace(d.Sender)
	if d.Channel == "" || d.Sender == "" {
		return ask.Request{}, fmt.Errorf("meet: dm-post needs both --channel and --sender")
	}
	if strings.TrimSpace(d.Body) == "" {
		return ask.Request{}, fmt.Errorf("meet: dm-post needs a post body")
	}
	if err := feed.ValidateExcerpt(d.Headline, d.Dek, d.Labels); err != nil {
		return ask.Request{}, err
	}
	if len(d.Attachments) == 0 {
		return ask.Request{}, fmt.Errorf("meet: dm-post needs at least one --attach photo downloaded on this host")
	}
	if len(d.Destinations) == 0 {
		return ask.Request{}, fmt.Errorf("meet: dm-post needs at least one --to destination channel")
	}
	chans, err := feedChannels()
	if err != nil {
		return ask.Request{}, err
	}
	for _, dest := range d.Destinations {
		ch, ok := chans[dest]
		if !ok {
			return ask.Request{}, fmt.Errorf("meet: no feed channel named %q; add one with `feed channel add`", dest)
		}
		if ch.Off {
			return ask.Request{}, fmt.Errorf("meet: feed channel %q is off", dest)
		}
		if ch.Cmd != feedPublishCmdName {
			return ask.Request{}, fmt.Errorf("meet: feed channel %q delivers via %q, not %q; cross-posts go through %s only",
				dest, ch.Cmd, feedPublishCmdName, feedPublishCmdName)
		}
	}
	effects := []string{"to: " + strings.Join(d.Destinations, ",")}
	for i, p := range d.Attachments {
		p = strings.TrimSpace(p)
		if p == "" {
			return ask.Request{}, fmt.Errorf("meet: attachment %d is empty", i+1)
		}
		d.Attachments[i] = p
		sum, err := attachmentDigest(p)
		if err != nil {
			return ask.Request{}, err
		}
		effects = append(effects, "photo "+filepath.Base(filepath.FromSlash(p))+" sha256:"+sum)
	}
	prompt := "feed post draft from " + d.human() + ": \u201c" + d.Headline + "\u201d / " + d.Dek +
		" / photos: " + strings.Join(d.photoNames(), ", ") + " / to: " + strings.Join(d.Destinations, ", ")
	return ask.Raise(ask.ApprovalSpec{
		Source:  FeedPublishSource,
		Argv:    feedPublishArgv(d),
		Effects: effects,
		Prompt:  prompt,
	})
}

// feedPublishArgv encodes the approved plan as the approval's argv. The
// shape mirrors the dm-post flags so the summary reads like the command
// that was approved and the token binds the exact plan.
func feedPublishArgv(d FeedDraft) []string {
	a := []string{"meet", "dm-post", "--channel", d.Channel, "--sender", d.Sender,
		"--headline", d.Headline, "--dek", d.Dek}
	for _, l := range d.Labels {
		a = append(a, "--label", l)
	}
	for _, p := range d.Attachments {
		a = append(a, "--attach", p)
	}
	for _, t := range d.Destinations {
		a = append(a, "--to", t)
	}
	return append(a, "--body", d.Body)
}

// parseFeedPublishArgv decodes an approval's argv back into the draft
// exactly as approved. Anything off-shape is refused: the publish side
// never guesses a plan.
func parseFeedPublishArgv(argv []string) (FeedDraft, error) {
	var d FeedDraft
	if len(argv) < 2 || argv[0] != "meet" || argv[1] != "dm-post" {
		return d, fmt.Errorf("meet: approval is not a feed publish (argv %q)", argv)
	}
	value := func(flag string) (string, bool) {
		for i := 2; i+1 < len(argv); i += 2 {
			if argv[i] == flag {
				return argv[i+1], true
			}
		}
		return "", false
	}
	multi := func(flag string) []string {
		var out []string
		for i := 2; i+1 < len(argv); i += 2 {
			if argv[i] == flag {
				out = append(out, argv[i+1])
			}
		}
		return out
	}
	// Every token must be a known flag/value pair: a stray token means the
	// plan is not the one propose wrote.
	known := map[string]bool{
		"--channel": true, "--sender": true, "--headline": true, "--dek": true,
		"--label": true, "--attach": true, "--to": true, "--body": true,
	}
	if (len(argv)-2)%2 != 0 {
		return d, fmt.Errorf("meet: approval argv has a flag without a value: %q", argv)
	}
	for i := 2; i < len(argv); i += 2 {
		if !known[argv[i]] {
			return d, fmt.Errorf("meet: approval argv has an unknown flag %q", argv[i])
		}
	}
	var ok bool
	if d.Channel, ok = value("--channel"); !ok {
		return d, fmt.Errorf("meet: approval argv has no --channel")
	}
	if d.Sender, ok = value("--sender"); !ok {
		return d, fmt.Errorf("meet: approval argv has no --sender")
	}
	if d.Headline, ok = value("--headline"); !ok {
		return d, fmt.Errorf("meet: approval argv has no --headline")
	}
	if d.Dek, ok = value("--dek"); !ok {
		return d, fmt.Errorf("meet: approval argv has no --dek")
	}
	if d.Body, ok = value("--body"); !ok {
		return d, fmt.Errorf("meet: approval argv has no --body")
	}
	d.Labels = multi("--label")
	d.Attachments = multi("--attach")
	d.Destinations = multi("--to")
	if len(d.Attachments) == 0 || len(d.Destinations) == 0 {
		return d, fmt.Errorf("meet: approval argv names no photos or no destinations")
	}
	return d, nil
}

// --- settle ----------------------------------------------------------------

// settleFeedPublish runs the approved plan: publish the post with its
// attachments, cross-post the headline plus viewer link to each named
// destination, and return the chat reply. It publishes nothing until every
// check passes, and it sends nothing until the post is published.
func settleFeedPublish(ctx context.Context, r ask.Request) (string, error) {
	if !r.IsApproval() || r.Approval == nil || r.Approval.Source != FeedPublishSource {
		return "", fmt.Errorf("meet: approval %s is not a feed publish", r.ID)
	}
	d, err := parseFeedPublishArgv(r.Approval.Argv)
	if err != nil {
		return "", err
	}
	want := map[string]string{}
	for _, e := range r.Approval.Effects {
		rest, ok := strings.CutPrefix(e, "photo ")
		if !ok {
			continue
		}
		if i := strings.LastIndex(rest, " sha256:"); i >= 0 {
			want[rest[:i]] = rest[i+len(" sha256:"):]
		}
	}
	for _, p := range d.Attachments {
		sum, err := attachmentDigest(p)
		if err != nil {
			return "", err
		}
		if w, ok := want[filepath.Base(filepath.FromSlash(p))]; ok && w != sum {
			return "", fmt.Errorf("meet: attachment %q changed since the draft was approved; nothing was published", p)
		}
	}
	postID, viewerURL, err := feedPublishPost(ctx, d)
	if err != nil {
		return "", err
	}
	link := viewerURL
	if link == "" {
		link = postID
	}
	message := d.Headline + "\n" + d.Dek + "\n" + link
	var sent []string
	for _, dest := range d.Destinations {
		if err := feedSendToChannel(ctx, dest, message); err != nil {
			return "", fmt.Errorf("meet: post %s published but %q failed: %w (sent to: %s)", postID, dest, err, strings.Join(sent, ", "))
		}
		sent = append(sent, dest)
	}
	return fmt.Sprintf("published %s\n%s\nsent to: %s", postID, link, strings.Join(sent, ", ")), nil
}

// newDMPostCmd is `bashy meet dm-post`: the agent's propose step. It checks
// the sender is paired for messaging (ask or control scope — proposing only
// raises an approval, it publishes nothing), raises the approval, and prints
// the draft plus its id for the chat reply.
func newDMPostCmd() *cobra.Command {
	var channel, sender, headline, dek string
	var labels, attaches, dests []string
	cmd := &cobra.Command{
		Use:   "dm-post",
		Short: "propose a feed post with photos from a DM chat (publishes only after /approve)",
		Long: "Propose a feed post drafted from an external chat message: `meet dm-post --channel C --sender S --to DEST... --headline H --dek D --attach FILE... [BODY...]`\n" +
			"raises one shared approval-queue item showing headline, dek, photo names and destinations, and prints the draft with its id.\n" +
			"Nothing publishes until a sender with approve scope settles it with /approve in the originating chat; /deny drops it.\n" +
			"Attachments are photo files already downloaded on this host; destinations are feed channels delivering via openclaw:send.",
		RunE: func(cmd *cobra.Command, args []string) error {
			pairing, err := pairingStore()
			if err != nil {
				return err
			}
			peer, paired, err := pairing.Paired(channel, sender)
			if err != nil {
				return err
			}
			if !paired {
				return fmt.Errorf("meet: %s on channel %s is not paired; nothing was proposed", sender, channel)
			}
			if !slices.Contains(peer.Scope, PeerScopeAsk) && !slices.Contains(peer.Scope, PeerScopeControl) {
				return dmScopeRefusal(cmd.OutOrStdout(), sender, channel, "a post draft", PeerScopeAsk)
			}
			draft := FeedDraft{
				Channel: channel, Sender: sender, Body: strings.TrimSpace(strings.Join(args, " ")),
				Headline: headline, Dek: dek, Labels: labels,
				Attachments: attaches, Destinations: dests,
			}
			req, err := proposeFeedPost(draft)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "draft %s\n", req.ID)
			fmt.Fprintln(out, feedDraftSummary(draft))
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "external channel the request came from")
	cmd.Flags().StringVar(&sender, "sender", "", "external sender the draft is for")
	cmd.Flags().StringVar(&headline, "headline", "", "post headline (single line, LLM-written by the agent)")
	cmd.Flags().StringVar(&dek, "dek", "", "post dek / standfirst (LLM-written by the agent)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "label (repeatable)")
	cmd.Flags().StringArrayVar(&attaches, "attach", nil, "photo file downloaded on this host (repeatable)")
	cmd.Flags().StringArrayVar(&dests, "to", nil, "destination feed channel delivering via openclaw:send (repeatable)")
	return cmd
}
