// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/redact"
)

// canaries are one value per leak class. The last two are the LIVE machine's
// own identity, because a scrubber that catches "example.com" and misses the
// hostname it actually runs on has caught nothing.
func canaries(t *testing.T) []string {
	t.Helper()
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	return []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.dBjftJeZ4CVPmB92K27uhbUJU1p1r",
		"AKIAIOSFODNN7EXAMPLE",
		"https://alice:hunter2@git.internal/repo.git",
		"postgres://svc:s3cr3t@db.internal:5432/app",
		"/Users/alice/.ssh/id_rsa",
		`C:\Users\alice\secrets.txt`,
		"aa:bb:cc:dd:ee:ff",
		"10.1.2.3",
		"alice@example.com",
		host,
		home,
	}
}

// writeThrough drives the PRODUCTION write path and returns the bytes on disk.
//
// It deliberately does not inspect the returned struct. Asserting on a field
// proves the function was called; only reading the artifact proves what was
// persisted. It also builds the scrubber with redact.FromHost — the same
// constructor production uses — because a hand-seeded fixture scrubber tests a
// different program from the one that ships.
func writeThrough(t *testing.T, argv []string, cwd string) string {
	t.Helper()
	root := t.TempDir()

	w := Open(root)
	defer w.Close()

	s := redact.FromHost()
	body := Scrub(s, argv, cwd, TemplateOpts{
		HomeDir: os.Getenv("HOME"),
		TmpDir:  os.TempDir(),
	})
	exit := 0
	if err := w.Append(Record{
		At: time.Now().UTC(), Cmd: argv[0], Exit: &exit, Observed: true,
	}, body, "ep-test"); err != nil {
		t.Fatalf("append: %v", err)
	}

	var got []byte
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return err
		}
		b, err := os.ReadFile(p)
		got = append(got, b...)
		return err
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return string(got)
}

func TestNoSecretReachesDisk(t *testing.T) {
	for _, canary := range canaries(t) {
		if strings.TrimSpace(canary) == "" {
			continue
		}
		t.Run(canary, func(t *testing.T) {
			on := writeThrough(t, []string{"curl", "-H", canary, "https://x/y"}, "/tmp")
			if strings.Contains(on, canary) {
				t.Errorf("canary reached disk verbatim:\n  canary %q\n  file   %s", canary, on)
			}
		})
	}
}

// TestNoSecretReachesDiskAnyPosition is the property the table cannot express:
// a leak class is only covered if it is masked wherever it appears.
func TestNoSecretReachesDiskAnyPosition(t *testing.T) {
	shapes := [][]string{
		{"psql", "CANARY"},
		{"mysql", "-pCANARY"},
		{"deploy", "--token", "CANARY"},
		{"deploy", "--token=CANARY"},
		{"env", "API_TOKEN=CANARY", "run"},
		{"curl", "-H", "Authorization: Bearer CANARY"},
		{"git", "clone", "CANARY"},
		{"ssh", "-i", "CANARY", "host"},
	}
	for _, canary := range canaries(t) {
		if strings.TrimSpace(canary) == "" {
			continue
		}
		for _, shape := range shapes {
			argv := make([]string, len(shape))
			for i, w := range shape {
				argv[i] = strings.ReplaceAll(w, "CANARY", canary)
			}
			on := writeThrough(t, argv, "/tmp")
			if strings.Contains(on, canary) {
				t.Errorf("canary reached disk\n  argv   %q\n  canary %q\n  file   %s",
					argv, canary, on)
			}
		}
	}
}

func FuzzNoSecretReachesDisk(f *testing.F) {
	f.Add("curl", "-H", "sk-abcdefghijklmnopqrstuvwxyz012345")
	f.Add("mysql", "-p", "hunter2")
	f.Add("git", "clone", "https://u:p@h/r.git")

	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()

	f.Fuzz(func(t *testing.T, prog, flag, val string) {
		if prog == "" {
			t.Skip()
		}
		argv := []string{prog, flag, val}
		root := t.TempDir()
		w := Open(root)
		defer w.Close()

		body := Scrub(redact.FromHost(), argv, home, TemplateOpts{HomeDir: home})
		exit := 0
		if err := w.Append(Record{Cmd: prog, Exit: &exit, Observed: true}, body, "ep-fuzz"); err != nil {
			t.Skip()
		}

		files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
		for _, p := range files {
			b, _ := os.ReadFile(p)
			for _, id := range []string{host, home} {
				if len(id) > 3 && strings.Contains(string(b), id) {
					t.Fatalf("machine identity %q reached disk from argv %q", id, argv)
				}
			}
		}
	})
}

// TestScrubStampsBeforeWrite pins the ordering rule: the record carries proof
// that redaction ran, and the proof is a COUNT — never the values.
func TestScrubStampsBeforeWrite(t *testing.T) {
	body := Scrub(redact.FromHost(), []string{"ssh", "alice@10.1.2.3"}, "/tmp", TemplateOpts{})
	if body.Elided() == 0 {
		t.Fatal("scrubber found nothing in an argv containing a user and an IP")
	}
	for _, a := range body.argv {
		if strings.Contains(a, "10.1.2.3") {
			t.Fatalf("raw IP survived into the stored argv: %q", a)
		}
	}
	if got := body.Template(); strings.Contains(got, "10.1.2.3") {
		t.Fatalf("raw IP survived into the template: %q", got)
	}
}

// TestTemplateUsesClassesNotTags is the cross-host mergeability rule.
//
// The stored argv keeps co-reference tags so the evidence still reads as a
// sentence, but the TEMPLATE — the node key — must use bare classes. A tag is
// derived from this machine's hostname, so a tag in the key would give two
// machines two different nodes for the same command.
func TestTemplateUsesClassesNotTags(t *testing.T) {
	body := Scrub(redact.FromHost(), []string{"ssh", "-p", "2222", "alice@remote.host"}, "/", TemplateOpts{})
	if got := body.Template(); got != "ssh -p <PORT> <USER>@<HOST>" {
		t.Errorf("template must be class-based, got %q", got)
	}
}
