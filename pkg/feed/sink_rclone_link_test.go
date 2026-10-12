package feed

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRcloneSinkAddChecksRemote(t *testing.T) {
	ctx := context.Background()
	runs := 0
	run := func(_ context.Context, args ...string) ([]byte, error) {
		runs++
		if len(args) != 1 || args[0] != "listremotes" {
			t.Fatalf("rclone args = %v", args)
		}
		return []byte("other:\nready:\n"), nil
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	env := Env{RcloneRun: run}
	cmd := NewCmdWith(env)
	cmd.SetArgs([]string{"sink", "add", "rclone", "missing", "absent:posts"})
	if err := cmd.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), "bashy rclone config") {
		t.Fatalf("missing remote error = %v", err)
	}
	if cfgs, err := LoadSinks(env.dir()); err != nil || len(cfgs) != 0 {
		t.Fatalf("missing remote saved: %+v, %v", cfgs, err)
	}
	cmd = NewCmdWith(env)
	cmd.SetArgs([]string{"sink", "add", "rclone", "good", "ready:posts"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("configured remote: %v", err)
	}
	if runs != 2 {
		t.Fatalf("listremotes calls = %d, want 2", runs)
	}
}

func TestRclonePublicURL(t *testing.T) {
	const dest = "ready:feed/posts/a.md"
	const rel = "posts/a.md"
	for _, tc := range []struct {
		name, output string
		err          error
		base, want   string
	}{
		{name: "link supported", output: "https://files.example/a.md\n", want: "https://files.example/a.md"},
		{name: "link unsupported with base", err: errors.New("doesn't support public links"), base: "https://feed.example", want: "https://feed.example/posts/a.md"},
		{name: "link unsupported without base", err: errors.New("doesn't support public links")},
		{name: "non-public link", output: "ready:feed/posts/a.md\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if len(args) != 2 || args[0] != "link" || args[1] != dest {
					t.Fatalf("rclone args = %v", args)
				}
				return []byte(tc.output), tc.err
			}
			got := rclonePublicURL(context.Background(), run, dest, tc.base, rel)
			if got != tc.want {
				t.Fatalf("public URL = %q, want %q", got, tc.want)
			}
			if tc.want == "" && len(publicSinkURLs([]string{got})) != 0 {
				t.Fatal("non-public result would reach registry")
			}
		})
	}
}

func TestRcloneSinkPutUsesPublicLink(t *testing.T) {
	const dest = "ready:feed/posts/a.md"
	var calls []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		switch args[0] {
		case "copyto":
			if len(args) != 3 || args[2] != dest {
				t.Fatalf("copyto args = %v", args)
			}
			return nil, nil
		case "link":
			if len(args) != 2 || args[1] != dest {
				t.Fatalf("link args = %v", args)
			}
			return []byte("https://files.example/a.md\n"), nil
		default:
			t.Fatalf("unexpected rclone command: %v", args)
			return nil, nil
		}
	}
	s, err := openRcloneSinkWithRunner(SinkConfig{Name: "ready", Target: "ready:feed", PublicURL: "https://fallback.example/feed"}, run)
	if err != nil {
		t.Fatal(err)
	}
	url, err := s.Put(context.Background(), "posts/a.md", []byte("post"))
	if err != nil || url != "https://files.example/a.md" {
		t.Fatalf("Put URL = %q, %v", url, err)
	}
	if strings.Join(calls, ",") != "copyto,link" {
		t.Fatalf("rclone calls = %v", calls)
	}
}
