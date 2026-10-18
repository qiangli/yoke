// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// refStore spills one blob and returns a registry resolving over the store.
func refStore(t *testing.T, blobs ...string) (*Store, *ref.Registry, []string) {
	t.Helper()
	s := NewStore(t.TempDir())
	var digests []string
	for _, b := range blobs {
		d, err := s.Put([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
	}
	g := ref.NewRegistry()
	RegisterRefs(g, s)
	return s, g, digests
}

func TestRefOutFound(t *testing.T) {
	s, g, digests := refStore(t, "the complete elided bytes")
	full := strings.TrimPrefix(digests[0], "sha256:")
	n, err := g.Resolve("out:" + full[:12])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n.Kind != ref.Out || n.Ref != "out:"+full[:12] {
		t.Errorf("identity = %+v", n)
	}
	if !strings.Contains(n.Title, "sha256:"+full) {
		t.Errorf("Title = %q, want the full digest", n.Title)
	}
	if n.Open != "bashy out "+full[:12] {
		t.Errorf("Open = %q, want the out reader", n.Open)
	}
	if !strings.HasSuffix(n.Where, full) {
		t.Errorf("Where = %q, want the blob path", n.Where)
	}
	// The full and scheme-prefixed spellings resolve to the same bytes the
	// `out` reader returns: byte-identical to `bashy out <handle>`.
	for _, in := range []string{"out:" + full, "out:sha256:" + full, "urn:dhnt:out:" + full[:12]} {
		m, err := g.Resolve(in)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", in, err)
		}
		got, _, err := s.Get(m.ID)
		if err != nil || string(got) != "the complete elided bytes" {
			t.Errorf("Resolve(%s) reads %q, %v", in, got, err)
		}
	}
}

func TestRefOutNotFound(t *testing.T) {
	_, g, _ := refStore(t, "something else entirely")
	if _, err := g.Resolve("out:deadbeef"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("absent prefix err = %v, want ErrNotFound", err)
	}
	full := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := g.Resolve("out:" + full); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("absent digest err = %v, want ErrNotFound", err)
	}
}

func TestRefOutAmbiguousIsError(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	// Two blobs sharing a prefix: force the collision by writing the files
	// directly under names the test controls.
	for _, name := range []string{
		"aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888",
		"aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb9999",
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := ref.NewRegistry()
	RegisterRefs(g, s)
	_, err := g.Resolve("out:aaaa1111")
	if err == nil || errors.Is(err, ref.ErrNotFound) || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix err = %v, want the ambiguity named (never not-found)", err)
	}
}

func TestRefOutUnreadableStoreIsError(t *testing.T) {
	g := ref.NewRegistry()
	RegisterRefs(g, NewStore(filepath.Join(t.TempDir(), "never-created")))
	_, err := g.Resolve("out:9c2d4f1a")
	if err == nil || errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("missing root err = %v, want a store error (never not-found)", err)
	}
	if _, err := g.Resolve("out:nothex!!"); errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("malformed handle err = %v, want an error (never not-found)", err)
	}
}
