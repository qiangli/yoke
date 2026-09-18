package ask

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// isolate points the store at a private tempdir, the same idiom room_test uses.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	return dir
}

func newTestRequest(t *testing.T, mutate ...func(*Request)) Request {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := Request{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Prompt:        "GitHub PAT",
		Name:          "GH_PAT",
		Secret:        true,
		Created:       now,
		Expires:       now.Add(10 * time.Second),
		ValueExpires:  now.Add(time.Minute),
		Sink:          Sink{Kind: SinkFile, Detail: "a private file"},
		Requester:     currentRequester(),
	}
	for _, m := range mutate {
		m(&r)
	}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The rendezvous is the rung that always works, so it is the one that must work.
func TestRendezvousRoundTrip(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)

	const secret = "ghp_the_actual_secret"
	got := make(chan []byte, 1)
	errc := make(chan error, 1)
	var status bytes.Buffer
	go func() {
		v, err := waitForAnswer(r, &status)
		if err != nil {
			errc <- err
			return
		}
		got <- v
	}()

	waitFor(t, func() bool { return channelReady(r.ID) })
	if err := Answer(r, []byte(secret)); err != nil {
		t.Fatal(err)
	}

	select {
	case v := <-got:
		if string(v) != secret {
			t.Errorf("value = %q, want %q", v, secret)
		}
	case err := <-errc:
		t.Fatalf("waitForAnswer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the answer")
	}

	// The instruction the requester prints must carry the sentinel a harness keys
	// on AND a command a human can retype — and neither may carry the value.
	out := status.String()
	if !strings.Contains(out, SchemaVersion) {
		t.Errorf("status is missing the %s sentinel:\n%s", SchemaVersion, out)
	}
	if !strings.Contains(out, "bashy ask claim "+shortID(r.ID)) {
		t.Errorf("status is missing the human instruction:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Error("the status line leaked the value")
	}
}

// The invariant the whole design rests on. Deliberately TWO-SIDED: a test that
// only asserts the secret is absent would pass against an implementation that
// never obtained the secret at all — an absence-of-evidence success.
func TestRequestNeverContainsTheValueOnDisk(t *testing.T) {
	root := isolate(t)
	r := newTestRequest(t)

	const secret = "ghp_never_write_me"
	got := make(chan []byte, 1)
	errc := make(chan error, 1)
	go func() {
		v, err := waitForAnswer(r, &bytes.Buffer{})
		if err != nil {
			errc <- err
			return
		}
		got <- v
	}()
	waitFor(t, func() bool { return channelReady(r.ID) })
	if err := Answer(r, []byte(secret)); err != nil {
		t.Fatal(err)
	}

	var value []byte
	select {
	case value = <-got:
	case err := <-errc:
		t.Fatalf("waitForAnswer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	// Side one: the value really was delivered.
	if string(value) != secret {
		t.Fatalf("value = %q, want %q — the absence check below would be vacuous", value, secret)
	}
	// Side two: it is nowhere under the store.
	assertNotOnDisk(t, root, secret)
}

// Once answered, the channel is gone: a second delivery cannot succeed, so a
// captured instruction line cannot be replayed to feed a second value in.
func TestAnswerIsSingleUse(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = waitForAnswer(r, &bytes.Buffer{})
	}()
	waitFor(t, func() bool { return channelReady(r.ID) })
	if err := Answer(r, []byte("first")); err != nil {
		t.Fatal(err)
	}
	<-done

	if err := Answer(r, []byte("second")); err == nil {
		// The socket is unlinked on close, so a second dial must fail. On the file
		// channel the request dir is still there, so guard on the socket having
		// existed at all.
		if !forceFileChannel() && runtime.GOOS != "windows" {
			t.Error("a second answer succeeded — the channel is not single-use")
		}
	}
}

// The file channel is the degraded path and only Windows exercises it naturally.
// Forcing it here means it is covered on every platform, not just the CI leg
// nobody runs locally.
func TestFileFallbackRoundTrip(t *testing.T) {
	root := isolate(t)
	t.Setenv(NoSocketEnv, "1")
	r := newTestRequest(t)

	const secret = "file-channel-secret"
	got := make(chan []byte, 1)
	errc := make(chan error, 1)
	go func() {
		v, err := waitForAnswer(r, &bytes.Buffer{})
		if err != nil {
			errc <- err
			return
		}
		got <- v
	}()

	// Wait for the CHANNEL, not merely for the request file. The request is saved
	// before the listener starts, so waiting on it races recordChannel and the
	// answer bounces with "not ready" — an intermittent failure that says nothing
	// about the code under test.
	waitFor(t, func() bool { return channelReady(r.ID) })
	if err := Answer(r, []byte(secret)); err != nil {
		t.Fatal(err)
	}

	select {
	case v := <-got:
		if string(v) != secret {
			t.Errorf("value = %q, want %q", v, secret)
		}
	case err := <-errc:
		t.Fatalf("waitForAnswer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out on the file channel")
	}

	// The answer file is transient by contract — it must not survive the read.
	if _, err := os.Stat(filepath.Join(requestDir(r.ID), answerFile)); err == nil {
		t.Error("the answer file survived the read")
	}
	assertNotOnDisk(t, root, secret)
}

// An unanswered request whose deadline passed must not linger and must never be
// offered to a human to answer.
func TestExpiredRequestIsReapedOnRead(t *testing.T) {
	isolate(t)
	r := newTestRequest(t, func(r *Request) {
		r.Expires = time.Now().Add(-time.Second)
	})

	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expired request survived the read: %+v", all)
	}
	if _, err := os.Stat(requestDir(r.ID)); err == nil {
		t.Error("the request directory was not removed")
	}
}

// Nobody is waiting for an answer to a request whose requester died, so prompting
// for one is pure noise.
func TestDeadRequesterIsPruned(t *testing.T) {
	isolate(t)
	newTestRequest(t, func(r *Request) {
		// A pid that cannot be alive. Not merely unused — the reaper must not
		// depend on the pid being absent from the table by luck.
		r.Requester.PID = 0x7FFFFFFE
	})
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("request with a dead requester survived: %+v", all)
	}
}

// A DELIVERED value must NOT be reaped just because the requester exited — it
// exits immediately after delivery by design, and reaping on that signal would
// delete every value at the instant it became useful.
func TestDeliveredValueSurvivesRequesterExit(t *testing.T) {
	isolate(t)
	r := newTestRequest(t, func(r *Request) {
		r.Requester.PID = 0x7FFFFFFE // dead
		r.Expires = time.Now().Add(-time.Second)
	})
	if _, err := deliver(r, []byte("delivered-value")); err != nil {
		t.Fatal(err)
	}

	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("a delivered value was reaped: got %d requests, want 1", len(all))
	}
	b, err := os.ReadFile(filepath.Join(requestDir(r.ID), valueFile))
	if err != nil {
		t.Fatalf("value file gone: %v", err)
	}
	if string(b) != "delivered-value" {
		t.Errorf("value = %q", b)
	}
}

// ...but once its own TTL passes, it does go.
func TestDeliveredValueExpiresOnItsOwnTTL(t *testing.T) {
	isolate(t)
	r := newTestRequest(t, func(r *Request) {
		r.ValueExpires = time.Now().Add(-time.Second)
	})
	if _, err := deliver(r, []byte("stale")); err != nil {
		t.Fatal(err)
	}
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("value outlived its TTL: %+v", all)
	}
}

func TestDefaultSinkWritesA0600File(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	path, err := deliver(r, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "s3cret" {
		t.Errorf("content = %q", b)
	}
	assertMode0600(t, path)
}

// An --out path must never silently clobber, and must never be followed through a
// symlink — the two halves of the classic shared-directory attack.
func TestOutSinkRefusesClobberAndSymlink(t *testing.T) {
	dir := isolate(t)

	t.Run("refuses to overwrite an existing file", func(t *testing.T) {
		target := filepath.Join(dir, "existing")
		if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := newTestRequest(t, func(r *Request) {
			r.Sink = Sink{Kind: SinkOut, Detail: target}
		})
		if _, err := deliver(r, []byte("secret")); err == nil {
			t.Fatal("clobbered an existing file")
		}
		b, _ := os.ReadFile(target)
		if string(b) != "original" {
			t.Errorf("the existing file was modified: %q", b)
		}
	})

	t.Run("refuses to follow a symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("O_NOFOLLOW has no Windows equivalent; the ACL on the profile dir stands in")
		}
		victim := filepath.Join(dir, "victim")
		link := filepath.Join(dir, "link")
		if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		r := newTestRequest(t, func(r *Request) {
			r.Sink = Sink{Kind: SinkOut, Detail: link}
		})
		if _, err := deliver(r, []byte("secret")); err == nil {
			t.Fatal("wrote through a symlink")
		}
		b, _ := os.ReadFile(victim)
		if string(b) != "theirs" {
			t.Errorf("the symlink target was overwritten: %q", b)
		}
	})

	t.Run("refuses a world-writable parent directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix mode bits")
		}
		open := filepath.Join(dir, "open")
		if err := os.Mkdir(open, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o777); err != nil {
			t.Fatal(err)
		}
		r := newTestRequest(t, func(r *Request) {
			r.Sink = Sink{Kind: SinkOut, Detail: filepath.Join(open, "v")}
		})
		if _, err := deliver(r, []byte("secret")); err == nil {
			t.Fatal("wrote a secret into a world-writable directory")
		}
	})
}

// An empty answer is a decline everywhere, and must never be delivered as a value.
func TestEmptyAnswerIsRefused(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	if err := Answer(r, nil); err == nil {
		t.Error("an empty value was accepted")
	}
	if err := Answer(r, []byte("")); err == nil {
		t.Error("an empty value was accepted")
	}
}

// A unique prefix resolves; an ambiguous one must NOT be guessed at.
func TestFindPrefixAndAmbiguity(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)

	got, err := Find(shortID(r.ID))
	if err != nil {
		t.Fatalf("prefix lookup failed: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("resolved %s, want %s", got.ID, r.ID)
	}
	if _, err := Find("zzzzzzzz"); err == nil {
		t.Error("a non-matching prefix resolved to something")
	}
	if _, err := Find(""); err == nil {
		t.Error("an empty id resolved to something")
	}
}

// --- helpers -------------------------------------------------------------

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the request channel to come up")
}

// channelReady reports that the answer channel is actually open.
//
// It must wait for the SOCKET, not merely for the request file, wherever sockets
// are in play. Answering before the bind completes is refused — correctly, since a
// missing socket on a socket platform means "the channel is closed" — so a helper
// that returned early here would race the listener and fail for the wrong reason.
// In production the ordering is guaranteed the other way: waitForAnswer binds
// first and only then prints the instruction a human can act on.
func channelReady(id string) bool {
	return openChannel(requestDir(id)) != ""
}

// assertNotOnDisk walks the whole store looking for the secret. This is the check
// that would catch a future refactor deciding to "just cache the answer" somewhere.
func assertNotOnDisk(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if bytes.Contains(b, []byte(secret)) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("the value was found on disk in %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // no unix mode bits; the profile-directory ACL is the protection
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s is mode %#o, want 0600", path, perm)
	}
}
