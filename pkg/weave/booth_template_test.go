package weave

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestTemplateBuildReuseAndDigest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	src := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "input"), []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := buildBoothTemplate(context.Background(), 7, "1214", src, []string{"printf warmed > warmed"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildBoothTemplate(context.Background(), 7, "1214", src, []string{"printf warmed > warmed"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Dir != second.Dir || first.Digest != second.Digest {
		t.Fatalf("template was not reused: %#v != %#v", first, second)
	}
	if first.Dir != filepath.Join(home, "sprint", "booths", "7", "1214", "template") {
		t.Fatalf("template directory = %q", first.Dir)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "input"), []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}
	third, err := buildBoothTemplate(context.Background(), 7, "1214", src, []string{"printf warmed > warmed"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Digest == first.Digest {
		t.Fatal("digest did not change after source changed")
	}
}

func TestTemplateCopyIsIndependent(t *testing.T) {
	template := t.TempDir()
	if err := os.MkdirAll(filepath.Join(template, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "nested", "input"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "attempt")
	method, elapsed, err := copyBoothTemplate(boothTemplate{Dir: template}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if method == "" || elapsed < 0 {
		t.Fatalf("copy report = %q, %s", method, elapsed)
	}
	templateInfo, err := os.Stat(filepath.Join(template, "nested", "input"))
	if err != nil {
		t.Fatal(err)
	}
	copyInfo, err := os.Stat(filepath.Join(dst, "nested", "input"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(templateInfo, copyInfo) {
		t.Fatal("attempt file reuses the template inode")
	}
	if err := os.WriteFile(filepath.Join(dst, "nested", "input"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(template, "nested", "input"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("template changed through copy: %q", got)
	}
	if _, _, err := copyBoothTemplate(boothTemplate{Dir: template}, dst); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("existing destination error = %v", err)
	}
}

func TestTemplateSetupFailureLeavesLog(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	src := t.TempDir()
	_, err := buildBoothTemplate(context.Background(), 8, "bad", src, []string{"echo deliberate-failure; exit 9"})
	if err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("setup error = %v", err)
	}
	log, readErr := os.ReadFile(filepath.Join(BoothTemplateDir(8, "bad"), "..", "setup.log"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(log), "deliberate-failure") {
		t.Fatalf("setup log = %q", log)
	}
}

func TestTemplateDigestStable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := boothTemplateDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := boothTemplateDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("unstable digest: %q != %q", first, second)
	}
}

// BenchmarkTemplateCopyAndPlainClone reports per-operation time for an attempt
// copy and a fresh local plain clone. A one-iteration APFS sample was 2.1 ms /
// 49 KB for the copy and 4.6 ms / 2.3 MB for the clone; it makes no assertion.
func BenchmarkTemplateCopyAndPlainClone(b *testing.B) {
	b.Setenv("BASHY_HOME", b.TempDir())
	src := filepath.Join(b.TempDir(), "source")
	repo, err := gogit.PlainInit(src, false)
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "fixture"), []byte(strings.Repeat("contents\n", 1024)), 0644); err != nil {
		b.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := worktree.Add("fixture"); err != nil {
		b.Fatal(err)
	}
	if _, err := worktree.Commit("seed", &gogit.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid"}}); err != nil {
		b.Fatal(err)
	}
	template, err := buildBoothTemplate(context.Background(), 9, "benchmark", src, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("template-copy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst := filepath.Join(b.TempDir(), "attempt")
			if _, _, err := copyBoothTemplate(template, dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("plain-clone", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst := filepath.Join(b.TempDir(), "attempt")
			if _, err := gogit.PlainClone(dst, false, &gogit.CloneOptions{URL: src}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
