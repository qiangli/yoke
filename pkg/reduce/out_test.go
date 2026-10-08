package reduce

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestOutCommandContract(t *testing.T) {
	store := NewStore(t.TempDir())
	body := []byte("line one\n\x00\xfflast line without newline")
	digest, err := store.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{digest, strings.TrimPrefix(digest, "sha256:"), strings.TrimPrefix(digest, "sha256:")[:MinHandleLen]} {
		cmd := NewOutCmd(func() (*Store, error) { return store, nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{handle})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), body) {
			t.Fatalf("%s: altered binary output", handle)
		}
	}
	for _, args := range [][]string{nil, {"one", "two"}, {"../outside"}, {"zzzzzzzz"}, {"00000000"}} {
		cmd := NewOutCmd(func() (*Store, error) { return store, nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("args %q succeeded", args)
		}
		if out.Len() != 0 {
			t.Errorf("args %q emitted partial output", args)
		}
	}
	sentinel := errors.New("store unavailable")
	cmd := NewOutCmd(func() (*Store, error) { return nil, sentinel })
	cmd.SetArgs([]string{digest})
	if err := cmd.Execute(); !errors.Is(err, sentinel) {
		t.Fatalf("lost resolver failure: %v", err)
	}
	cmd = NewOutCmd(nil)
	cmd.SetArgs([]string{digest})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing resolver succeeded")
	}
	if err := Recover(store, digest, failedOutWriter{}); !errors.Is(err, errOutWrite) {
		t.Fatalf("lost writer failure: %v", err)
	}
}

var errOutWrite = errors.New("sink closed")

type failedOutWriter struct{}

func (failedOutWriter) Write([]byte) (int, error) { return 0, errOutWrite }
