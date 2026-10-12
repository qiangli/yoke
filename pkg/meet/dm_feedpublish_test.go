package meet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/feed"
)

// feedPublishStubs replaces the feed seams with fakes and returns recorders
// for published posts and channel sends. Nothing touches a sink, a tool
// backend, or the network.
type feedPublishStubs struct {
	channels map[string]feed.Channel
	posts    []FeedDraft
	sends    []feedSend
	postID   string
	link     string
	publish  error
	sendErr  map[string]error
}

type feedSend struct {
	Dest    string
	Message string
}

func stubFeedPublish(t *testing.T, channels map[string]feed.Channel) *feedPublishStubs {
	t.Helper()
	sb := &feedPublishStubs{channels: channels, postID: "post-1", link: "https://sink.example/posts/2026/post-1.md"}
	oldChans, oldPost, oldSend := feedChannels, feedPublishPost, feedSendToChannel
	feedChannels = func() (map[string]feed.Channel, error) { return sb.channels, nil }
	feedPublishPost = func(_ context.Context, d FeedDraft) (string, string, error) {
		sb.posts = append(sb.posts, d)
		return sb.postID, sb.link, sb.publish
	}
	feedSendToChannel = func(_ context.Context, dest, message string) error {
		sb.sends = append(sb.sends, feedSend{Dest: dest, Message: message})
		return sb.sendErr[dest]
	}
	t.Cleanup(func() { feedChannels, feedPublishPost, feedSendToChannel = oldChans, oldPost, oldSend })
	return sb
}

func openclawChannels(names ...string) map[string]feed.Channel {
	out := map[string]feed.Channel{}
	for _, n := range names {
		out[n] = feed.Channel{Name: n, Cmd: "openclaw:send", Args: "--channel " + n}
	}
	return out
}

func writePhoto(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// runDMPostCmd runs `meet dm-post`, returning stdout and the error: propose
// failures are the point of several cases, so they must not fatal here.
func runDMPostCmd(stdin string, args ...string) (string, error) {
	cmd := newDMPostCmd()
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return out.String(), err
}

func pendingFeedDrafts(t *testing.T) []ask.Request {
	t.Helper()
	pending, err := ask.PendingApprovals()
	if err != nil {
		t.Fatal(err)
	}
	var out []ask.Request
	for _, r := range pending {
		if r.IsApproval() && r.Approval != nil && r.Approval.Source == FeedPublishSource {
			out = append(out, r)
		}
	}
	return out
}

// A paired sender's photos become one draft approval item showing headline,
// dek, photo names and destinations — and nothing is published.
func TestDMPostProposesDraftApprovalWithoutPublishing(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["telegram:alice"] = []string{PeerScopeAsk, PeerScopeControl, PeerScopeApprove}
	sb := stubFeedPublish(t, openclawChannels("telegram", "matrix"))
	calls := stubInvoke(t, "drafted")

	p1 := writePhoto(t, "one.png", "photo-bytes-1")
	p2 := writePhoto(t, "two.png", "photo-bytes-2")
	out, err := runDMPostCmd("", "--channel", "telegram", "--sender", "alice",
		"--to", "telegram", "--to", "matrix",
		"--headline", "Harbor sunset", "--dek", "An evening walk with two photos.",
		"--attach", p1, "--attach", p2, "sunset over the harbor")
	if err != nil {
		t.Fatalf("dm-post: %v\noutput: %s", err, out)
	}
	for _, want := range []string{"draft ", "Harbor sunset", "An evening walk with two photos.", "one.png", "two.png", "telegram", "matrix"} {
		if !strings.Contains(out, want) {
			t.Fatalf("draft output lacks %q:\n%s", want, out)
		}
	}
	drafts := pendingFeedDrafts(t)
	if len(drafts) != 1 {
		t.Fatalf("pending drafts = %d", len(drafts))
	}
	r := drafts[0]
	if !strings.Contains(r.Prompt, "Harbor sunset") || !strings.Contains(r.Prompt, "one.png") ||
		!strings.Contains(r.Prompt, "telegram:alice") {
		t.Fatalf("approval prompt = %q", r.Prompt)
	}
	summary := r.ApprovalSummary()
	for _, want := range []string{"Harbor sunset", "An evening walk with two photos.", "one.png", "two.png", "telegram", "matrix"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("approval summary lacks %q: %q", want, summary)
		}
	}
	if len(sb.posts) != 0 || len(sb.sends) != 0 || len(*calls) != 0 {
		t.Fatalf("proposing published or sent: posts=%d sends=%d agentCalls=%d", len(sb.posts), len(sb.sends), len(*calls))
	}
	if _, decided := ask.LoadDecision(r.ID); decided {
		t.Fatal("a fresh draft must have no decision")
	}
}

// Propose-time validation is a table: every bad draft fails loudly with no
// approval raised and nothing published.
func TestDMPostValidation(t *testing.T) {
	chans := openclawChannels("telegram")
	chans["sms"] = feed.Channel{Name: "sms", Cmd: "twilio:send"}
	chans["quiet"] = feed.Channel{Name: "quiet", Cmd: "openclaw:send", Off: true}

	for _, tc := range []struct {
		name       string
		sender     string
		devices    map[string][]string
		args       []string
		needPhotos bool
		want       string
	}{
		{"unpaired sender", "mallory", nil, []string{"body"}, true, "is not paired"},
		{"approve-only scope cannot draft", "ops", map[string][]string{"telegram:ops": {PeerScopeApprove}}, []string{"body"}, true, "needs scope"},
		{"unknown channel", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "pigeon", "body"}, true, "no feed channel named"},
		{"non-openclaw channel", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "sms", "body"}, true, "not \"openclaw:send\""},
		{"off channel", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "quiet", "body"}, true, "is off"},
		{"no destination", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"body"}, true, "at least one --to"},
		{"no photos", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "telegram", "body"}, false, "at least one --attach"},
		{"missing photo file", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "telegram", "body"}, false, "attachment"},
		{"empty body", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "telegram"}, true, "needs a post body"},
		{"headline ends with period", "alice", map[string][]string{"telegram:alice": {PeerScopeAsk}}, []string{"--to", "telegram", "--headline", "Bad headline.", "body"}, true, "must not end with a period"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDMFrontDoorEnv(t)
			for k, v := range tc.devices {
				f.devices[k] = v
			}
			sb := stubFeedPublish(t, chans)
			args := []string{"--channel", "telegram", "--sender", tc.sender, "--dek", "A short standfirst."}
			args = append(args, tc.args...)
			if tc.needPhotos {
				args = append(args, "--attach", writePhoto(t, "p.png", "bytes"))
			}
			if tc.name == "missing photo file" {
				args = append(args, "--attach", filepath.Join(t.TempDir(), "gone.png"))
			}
			// A usable headline unless the case overrides it.
			if !strings.Contains(strings.Join(args, " "), "--headline") {
				args = append(args, "--headline", "Harbor light")
			}
			out, err := runDMPostCmd("", args...)
			if err == nil && !strings.Contains(out, tc.want) {
				t.Fatalf("want refusal containing %q, got err=%v out=%q", tc.want, err, out)
			}
			if err != nil && !strings.Contains(err.Error()+out, tc.want) {
				t.Fatalf("want failure containing %q, got err=%v out=%q", tc.want, err, out)
			}
			if len(pendingFeedDrafts(t)) != 0 {
				t.Fatal("a rejected draft raised an approval")
			}
			if len(sb.posts) != 0 || len(sb.sends) != 0 {
				t.Fatal("a rejected draft published or sent")
			}
		})
	}
}

// /approve from an approve-scope sender publishes the approved plan — post
// with attachments, one openclaw:send per destination — and the reply carries
// the viewer link.
func TestDMApprovePublishesDraftAndRepliesWithViewerLink(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["telegram:alice"] = []string{PeerScopeAsk, PeerScopeControl, PeerScopeApprove}
	sb := stubFeedPublish(t, openclawChannels("telegram", "matrix"))
	stubInvoke(t, "drafted")

	p1 := writePhoto(t, "one.png", "photo-bytes-1")
	p2 := writePhoto(t, "two.png", "photo-bytes-2")
	if _, err := runDMPostCmd("", "--channel", "telegram", "--sender", "alice",
		"--to", "telegram", "--to", "matrix",
		"--headline", "Harbor sunset", "--dek", "An evening walk with two photos.",
		"--attach", p1, "--attach", p2, "sunset over the harbor"); err != nil {
		t.Fatalf("dm-post: %v", err)
	}
	drafts := pendingFeedDrafts(t)
	if len(drafts) != 1 {
		t.Fatalf("pending drafts = %d", len(drafts))
	}

	out := runDMFrontDoorCmd(t, "", "", "--channel", "telegram", "--sender", "alice", "/approve", drafts[0].ID)
	if !strings.Contains(out, "allow "+drafts[0].ID) {
		t.Fatalf("approve output = %q", out)
	}
	if !strings.Contains(out, "published post-1") || !strings.Contains(out, sb.link) {
		t.Fatalf("reply lacks the publish note or viewer link:\n%s", out)
	}
	if len(sb.posts) != 1 {
		t.Fatalf("posts = %d", len(sb.posts))
	}
	got := sb.posts[0]
	if got.Body != "sunset over the harbor" || got.Headline != "Harbor sunset" ||
		len(got.Attachments) != 2 || got.Attachments[0] != p1 || got.Attachments[1] != p2 ||
		len(got.Destinations) != 2 {
		t.Fatalf("published plan = %+v", got)
	}
	if len(sb.sends) != 2 || sb.sends[0].Dest != "telegram" || sb.sends[1].Dest != "matrix" {
		t.Fatalf("sends = %+v", sb.sends)
	}
	for _, s := range sb.sends {
		if !strings.Contains(s.Message, "Harbor sunset") || !strings.Contains(s.Message, sb.link) {
			t.Fatalf("channel message lacks headline or viewer link: %q", s.Message)
		}
	}
	if d, ok := ask.LoadDecision(drafts[0].ID); !ok || !d.Allowed() || d.By != "telegram:alice" {
		t.Fatalf("decision = %+v ok=%v", d, ok)
	}
}

// /deny drops the draft: the stubs stay silent and the reply says so.
func TestDMDenyDropsDraftWithoutPublishing(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["telegram:alice"] = []string{PeerScopeAsk, PeerScopeControl, PeerScopeApprove}
	sb := stubFeedPublish(t, openclawChannels("telegram"))
	stubInvoke(t, "drafted")

	p1 := writePhoto(t, "one.png", "photo-bytes-1")
	if _, err := runDMPostCmd("", "--channel", "telegram", "--sender", "alice",
		"--to", "telegram", "--headline", "Harbor light", "--dek", "A short standfirst.",
		"--attach", p1, "evening body"); err != nil {
		t.Fatalf("dm-post: %v", err)
	}
	drafts := pendingFeedDrafts(t)
	if len(drafts) != 1 {
		t.Fatalf("pending drafts = %d", len(drafts))
	}
	out := runDMFrontDoorCmd(t, "", "", "--channel", "telegram", "--sender", "alice", "/deny", drafts[0].ID)
	if !strings.Contains(out, "deny "+drafts[0].ID) || !strings.Contains(out, "nothing was published") {
		t.Fatalf("deny output = %q", out)
	}
	if len(sb.posts) != 0 || len(sb.sends) != 0 {
		t.Fatal("a denied draft published or sent")
	}
	if d, ok := ask.LoadDecision(drafts[0].ID); !ok || d.Allowed() {
		t.Fatalf("deny decision = %+v ok=%v", d, ok)
	}
}

// Approving publishes nothing when a photo changed under the draft.
func TestDMApproveRefusesChangedPhoto(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["telegram:alice"] = []string{PeerScopeAsk, PeerScopeControl, PeerScopeApprove}
	sb := stubFeedPublish(t, openclawChannels("telegram"))
	stubInvoke(t, "drafted")

	p1 := writePhoto(t, "one.png", "original-bytes")
	if _, err := runDMPostCmd("", "--channel", "telegram", "--sender", "alice",
		"--to", "telegram", "--headline", "Harbor light", "--dek", "A short standfirst.",
		"--attach", p1, "evening body"); err != nil {
		t.Fatalf("dm-post: %v", err)
	}
	drafts := pendingFeedDrafts(t)
	if len(drafts) != 1 {
		t.Fatalf("pending drafts = %d", len(drafts))
	}
	if err := os.WriteFile(p1, []byte("swapped-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newDMCmd()
	cmd.SetArgs([]string{"", "--channel", "telegram", "--sender", "alice", "/approve", drafts[0].ID})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "changed since the draft was approved") {
		t.Fatalf("err = %v out = %q", err, out.String())
	}
	if len(sb.posts) != 0 || len(sb.sends) != 0 {
		t.Fatal("a tampered draft published or sent")
	}
}

// An unpaired sender with photos gets only the pair code: no agent turn, no
// draft, no publish.
func TestDMUnpairedSenderWithPhotosGetsPairCodeOnly(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	sb := stubFeedPublish(t, openclawChannels("telegram"))
	calls := stubInvoke(t, "should never run")

	p1 := writePhoto(t, "one.png", "photo-bytes")
	out := runDMFrontDoorCmd(t, "", "codex", "--channel", "telegram", "--sender", "mallory",
		"--attach", p1, "post this with these photos to my feed and Telegram")
	if !strings.Contains(out, "pair code") {
		t.Fatalf("output has no pair code: %q", out)
	}
	if len(*calls) != 0 {
		t.Fatal("an unpaired sender's message invoked the agent")
	}
	if len(pendingFeedDrafts(t)) != 0 || len(sb.posts) != 0 || len(sb.sends) != 0 {
		t.Fatal("an unpaired sender raised a draft or published")
	}
	if f.requests != 1 {
		t.Fatalf("requests = %d", f.requests)
	}
}

// Attachments reach the agent turn as paths-as-data: the instruction names
// the files, never their contents, and chat.Files stays empty.
func TestDMAttachmentsReachAgentTurnAsPathsNotContents(t *testing.T) {
	f := setupDMFrontDoorEnv(t)
	f.devices["telegram:alice"] = []string{PeerScopeControl}
	calls := stubInvoke(t, "seen")

	const binaryMagic = "PK\x03\x04FAKE-PNG-BYTES-\x00\xff-binary"
	p1 := writePhoto(t, "shot one.png", binaryMagic)
	p2 := writePhoto(t, "shot-two.png", "more-bytes")

	var out string
	t.Run("flag attachments", func(t *testing.T) {
		out = runDMFrontDoorCmd(t, "", "codex", "--channel", "telegram", "--sender", "alice",
			"--attach", p1, "--attach", p2, "post these with captions")
	})
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	got := (*calls)[0]
	if len(got.Files) != 0 {
		t.Fatalf("attachments must not use chat Files (contents would inline): %+v", got.Files)
	}
	if !strings.Contains(got.Instruction, p1) || !strings.Contains(got.Instruction, p2) {
		t.Fatalf("instruction lacks attachment paths:\n%s", got.Instruction)
	}
	if strings.Contains(got.Instruction, "FAKE-PNG-BYTES") {
		t.Fatalf("instruction inlines attachment contents:\n%s", got.Instruction)
	}
	if !strings.Contains(strings.TrimSpace(out), "seen") {
		t.Fatalf("stdout = %q", out)
	}
	if ev := inboundEvents(t, "telegram:alice"); len(ev) != 1 ||
		!strings.Contains(ev[0].Body, "shot one.png") || !strings.Contains(ev[0].Body, "shot-two.png") {
		t.Fatalf("inbound events = %+v", ev)
	}

	t.Run("stdin attachments", func(t *testing.T) {
		calls2 := stubInvoke(t, "seen2")
		stdin := `{"text":"post this too","attachments":["` + p1 + `"]}`
		out2 := runDMFrontDoorCmd(t, stdin, "codex", "--channel", "telegram", "--sender", "alice")
		if strings.TrimSpace(out2) != "seen2" {
			t.Fatalf("stdout = %q", out2)
		}
		if len(*calls2) != 1 || !strings.Contains((*calls2)[0].Instruction, p1) {
			t.Fatalf("stdin attachments did not reach the turn: %+v", *calls2)
		}
	})

	t.Run("missing attachment fails loudly", func(t *testing.T) {
		cmd := newDMCmd()
		cmd.SetArgs([]string{"codex", "--channel", "telegram", "--sender", "alice",
			"--attach", filepath.Join(t.TempDir(), "gone.png"), "post this"})
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		cmd.SetContext(context.Background())
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "attachment") {
			t.Fatalf("err = %v, want an attachment failure", err)
		}
	})
}

// The approved plan round-trips through the approval argv exactly; anything
// off-shape is refused rather than guessed.
func TestFeedPublishArgvRoundTrip(t *testing.T) {
	want := FeedDraft{
		Channel: "telegram", Sender: "alice", Body: "sunset over the harbor",
		Headline: "Harbor light", Dek: "A short standfirst.", Labels: []string{"photos"},
		Attachments: []string{"/tmp/a/one.png", "/tmp/a/two.png"}, Destinations: []string{"telegram", "matrix"},
	}
	got, err := parseFeedPublishArgv(feedPublishArgv(want))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Channel != want.Channel || got.Sender != want.Sender || got.Body != want.Body ||
		got.Headline != want.Headline || got.Dek != want.Dek ||
		strings.Join(got.Labels, ",") != "photos" ||
		strings.Join(got.Attachments, ",") != strings.Join(want.Attachments, ",") ||
		strings.Join(got.Destinations, ",") != "telegram,matrix" {
		t.Fatalf("round trip = %+v", got)
	}
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"not a dm-post", []string{"meet", "other"}},
		{"odd tokens", []string{"meet", "dm-post", "--channel"}},
		{"unknown flag", append(feedPublishArgv(want), "--evil", "x")},
		{"no photos", []string{"meet", "dm-post", "--channel", "c", "--sender", "s", "--headline", "H", "--dek", "D", "--to", "t", "--body", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseFeedPublishArgv(tc.argv); err == nil {
				t.Fatalf("argv %q parsed without error", tc.argv)
			}
		})
	}
}
