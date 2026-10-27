package binmgr

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentTreeProvisionDoesNotReplaceWinner(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	h := &zip.FileHeader{Name: "tool.exe"}
	h.SetMode(0755)
	w, _ := zw.CreateHeader(h)
	w.Write([]byte("executable"))
	w, _ = zw.Create("lib/data")
	w.Write([]byte("required sibling"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.Write(archive.Bytes())
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	tool := Tool{Name: "concurrent-tree", Version: "1", Assets: map[string]Asset{Platform(): {URL: srv.URL + "/tool.zip", SHA256: sha256hex(archive.Bytes()), Tree: true, Entrypoint: "tool.exe"}}}
	errs := make(chan error, 2)
	go func() { _, err := Ensure(context.Background(), tool); errs <- err }()
	<-entered
	go func() { _, err := Ensure(context.Background(), tool); errs <- err }()
	// Give the second process-equivalent caller a chance to enter provisioning
	// while the first download is held. Without the lock both fetch and publish.
	time.Sleep(200 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("concurrent tree fetched/published %d times; want 1", n)
	}
}

func TestTreeLockHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	held, err := lockTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lockTree(ctx, dir); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}
