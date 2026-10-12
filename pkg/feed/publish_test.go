package feed

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// TestPublishWithAttachmentsBodyPhotosAndViewerURLs exercises the
// programmatic publish path the DM approval settle uses: headline/dek as
// given, photos under the per-post directory, signed frontmatter, and one
// viewer URL per successful sink.
func TestPublishWithAttachmentsBodyPhotosAndViewerURLs(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	aDir, bDir := t.TempDir(), t.TempDir()
	te.mustRun("sink", "add", "rclone", "a", aDir)
	te.mustRun("sink", "add", "rclone", "b", bDir)

	files := t.TempDir()
	f1 := writeAttachFile(t, files, "photo1.png", "png-bytes-1")
	f2 := writeAttachFile(t, files, "photo2.png", "png-bytes-2")

	res, err := Publish(context.Background(), te.env, PublishRequest{
		Body:        "sunset over the harbor",
		Headline:    "Harbor sunset",
		Dek:         "An evening walk with two photos.",
		Labels:      []string{"photos"},
		Attachments: []string{f1, f2},
		Author:      "telegram:alice",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Post.Headline != "Harbor sunset" || res.Post.Dek != "An evening walk with two photos." {
		t.Fatalf("excerpt = %+v", res.Post)
	}
	if res.Post.Author != "telegram:alice" {
		t.Fatalf("author = %q", res.Post.Author)
	}
	if len(res.URLs) != 2 {
		t.Fatalf("URLs = %v", res.URLs)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v", res.Results)
	}
	if len(res.Uploads) != 2 {
		t.Fatalf("uploads = %+v", res.Uploads)
	}
	if err := VerifyPost(res.Post); err != nil {
		t.Fatalf("published post does not verify: %v", err)
	}
	for _, root := range []string{aDir, bDir} {
		file := firstPostMarkdown(t, root)
		raw, _ := os.ReadFile(file)
		p, err := ParsePost(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Attachments) != 2 {
			t.Fatalf("frontmatter attachments = %+v", p.Attachments)
		}
		if err := VerifyPost(p); err != nil {
			t.Fatalf("signed attachments do not verify: %v", err)
		}
	}
}

// TestPublishRejectsEmptyBody keeps the CLI's fail-loud behavior on the
// shared path.
func TestPublishRejectsEmptyBody(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te.mustRun("sink", "add", "rclone", "a", t.TempDir())
	if _, err := Publish(context.Background(), te.env, PublishRequest{Body: "  "}); err == nil ||
		!strings.Contains(err.Error(), "post body is empty") {
		t.Fatalf("err = %v", err)
	}
}

// TestPublishMatchesPostCLIOutput guards the runPost refactor: the CLI must
// print exactly what it printed before, now rendered from PublishResult.
func TestPublishMatchesPostCLIOutput(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te.mustRun("sink", "add", "rclone", "a", t.TempDir())
	files := t.TempDir()
	f1 := writeAttachFile(t, files, "photo.png", "png-bytes")
	out, errOut, err := te.run("post", writeBody(t, "cli parity body"),
		"--headline", "Harbor light", "--dek", "A short standfirst.", "--attach", f1)
	if err != nil {
		t.Fatalf("post: %v\nstdout: %s\nstderr: %s", err, out, errOut)
	}
	if strings.Count(out, "ok\t") != 2 { // the post plus its attachment
		t.Fatalf("post output = %q", out)
	}
	if !strings.Contains(errOut, "post ") || !strings.Contains(errOut, "index: skipped (host not paired)") {
		t.Fatalf("post stderr = %q", errOut)
	}
}

// TestSendToChannelUsesChannelTransport registers a fake tool backend and
// runs SendToChannel through the real toolcmd path with no network.
func TestSendToChannelUsesChannelTransport(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	if runtime.GOOS == "windows" {
		t.Skip("fake tool is a shell script")
	}
	root := fleettest.Ring(t)
	work := t.TempDir()
	log := filepath.Join(work, "calls.log")
	script := filepath.Join(work, "fakeclaw")
	body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\001' \"$a\"; done >> " + log + "\nprintf '\\002' >> " + log + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := fleet.Tool{Name: "fakeclaw", Kind: fleet.ToolKindCLI}
	tool.CLI.Binary = script
	tool.Commands = []fleet.ToolCommand{{
		Name: "send", Slash: "/send {args}", Mode: "print", Confirm: true,
		Exec: script + " send {args}",
	}}
	if err := fleet.New(fleet.WithRoot(root)).SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	if err := SaveChannels(te.env.dir(), []Channel{
		{Name: "telegram", Cmd: "fakeclaw:send", Args: "--channel telegram"},
		{Name: "quiet", Cmd: "fakeclaw:send", Off: true},
	}); err != nil {
		t.Fatal(err)
	}

	if err := SendToChannel(context.Background(), te.env.dir(), "telegram", "Harbor sunset\nA standfirst.\nhttps://sink.example/p1"); err != nil {
		t.Fatalf("SendToChannel: %v", err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--channel", "telegram", "--message", "Harbor sunset"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("tool argv lacks %q: %q", want, raw)
		}
	}

	for _, tc := range []struct {
		name, channel, message, want string
	}{
		{"unknown channel", "nowhere", "hi", "no channel named"},
		{"off channel", "quiet", "hi", "is off"},
		{"empty message", "telegram", "  ", "message is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := SendToChannel(context.Background(), te.env.dir(), tc.channel, tc.message)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
