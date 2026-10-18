package lenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/stream"
)

func putSince(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func deltaCursor(t *testing.T, out string) (string, string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	var trailer struct {
		T      string        `json:"t"`
		Cursor stream.Cursor `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &trailer); err != nil || trailer.T != "cursor" {
		t.Fatalf("invalid trailer: %s (%v)", out, err)
	}
	b, err := json.Marshal(trailer.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines[:len(lines)-1], "\n"), string(b)
}
func TestSinceReadersAndPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	putSince(t, path, "{\"n\":1}\n{\"n\":")
	out, stderr, code := runTool(t, dir, "", "--since=start", "-t", "jsonl", "rollout.jsonl")
	if code != 0 || stderr != "" {
		t.Fatalf("%d %s", code, stderr)
	}
	data, cursor := deltaCursor(t, out)
	if data != "{\"n\":1}" {
		t.Fatal(data)
	}
	out, stderr, code = runTool(t, dir, "", "--since", cursor, "-t", "jsonl", "rollout.jsonl")
	if code != 0 || stderr != "" {
		t.Fatalf("%d %s", code, stderr)
	}
	data, next := deltaCursor(t, out)
	if data != "" || next != cursor {
		t.Fatal("unchanged refresh advanced cursor")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("2}\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // Independent readers may resume the same immutable envelope.
		out, stderr, code = runTool(t, dir, "", "--since", cursor, "-t", "jsonl", "rollout.jsonl")
		if code != 0 || stderr != "" {
			t.Fatalf("%d %s", code, stderr)
		}
		data, _ = deltaCursor(t, out)
		if data != "{\"n\":2}" {
			t.Fatal(data)
		}
	}
}
func TestSinceRotationAndBinding(t *testing.T) {
	for _, mode := range []string{"rotation", "truncate", "regrow", "target", "format"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "log")
			putSince(t, path, "1\n2\n")
			out, _, code := runTool(t, dir, "", "--since=start", "-t", "jsonl", "log")
			if code != 0 {
				t.Fatal(code)
			}
			_, cursor := deltaCursor(t, out)
			name, format := "log", "jsonl"
			switch mode {
			case "rotation":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				putSince(t, path, "3\n4\n5\n")
			case "truncate":
				putSince(t, path, "3\n")
			case "regrow":
				putSince(t, path, "3\n4\n5\n")
			case "target":
				name = "other"
				putSince(t, filepath.Join(dir, name), "1\n2\n")
			case "format":
				format = "txt"
			}
			out, stderr, code := runTool(t, dir, "", "--since", cursor, "-t", format, "--cursor-out", "cursor", name)
			if code != 1 || out != "" || stderr == "" {
				t.Fatalf("%d %q %s", code, out, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "cursor")); !os.IsNotExist(err) {
				t.Fatal("failed read wrote cursor")
			}
		})
	}
}

type brokenDeltaWriter struct{}

func (brokenDeltaWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }
func TestSinceCheckpointAndFailures(t *testing.T) {
	dir := t.TempDir()
	putSince(t, filepath.Join(dir, "log"), "hello\npartial")
	args := []string{"--since=start", "-t", "txt", "--cursor-out", "cursor", "log"}
	out, stderr, code := runTool(t, dir, "", args...)
	if code != 0 || out != "hello\n" || stderr != "" {
		t.Fatalf("%d %q %s", code, out, stderr)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "cursor"))
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, code = runTool(t, dir, "", "--since=@cursor", "-t", "txt", "--cursor-out", "cursor", "log")
	if code != 0 || out != "" || stderr != "" {
		t.Fatalf("%d %q %s", code, out, stderr)
	}
	var errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Stdio: tool.Stdio{Out: brokenDeltaWriter{}, Err: &errb}}
	if run(rc, args) != 1 {
		t.Fatal("write failure succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "cursor"))
	if !bytes.Equal(saved, after) {
		t.Fatal("failed output advanced cursor")
	}
	for _, args := range [][]string{
		{"--since=start", "-t", "txt", "log"}, {"--since=start", "-t", "jsonl", "-"}, {"--since=start", "-t", "json", "log"}, {"--since=start", "-t", "jsonl", "--depth=1", "log"}, {"--since=10m", "-t", "jsonl", "log"}, {"--since=start", "-t", "txt", "--cursor-out=log", "log"}, {"--since=start", "-t", "jsonl", "log"},
	} {
		out, _, code = runTool(t, dir, "", args...)
		if code == 0 || out != "" {
			t.Fatalf("accepted invalid request %v: %d %q", args, code, out)
		}
	}
}

func TestSinceLargeRolloutRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.jsonl")
	line := `{"payload":"` + strings.Repeat("x", 1024) + "\"}\n"
	putSince(t, path, strings.Repeat(line, 32768))
	_, stderr, code := runTool(t, dir, "", "--since=start", "-t", "jsonl", "--cursor-out=cursor", "large.jsonl")
	if code != 0 {
		t.Fatal(stderr)
	}
	start := time.Now()
	out, stderr, code := runTool(t, dir, "", "--since=@cursor", "-t", "jsonl", "large.jsonl")
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatal(stderr)
	}
	data, _ := deltaCursor(t, out)
	if data != "" {
		t.Fatal("replayed historical data")
	}
	t.Logf("unchanged refresh over 32 MiB rollout: %s", elapsed)
}
