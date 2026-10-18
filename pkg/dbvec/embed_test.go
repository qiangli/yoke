package dbvec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// fakeEmbedder is an OpenAI-compatible /v1/embeddings endpoint answering
// with a fixed reply; it records every request it receives.
type fakeEmbedder struct {
	srv   *httptest.Server
	calls atomic.Int32
	last  atomic.Value // map[string]any request body
	auth  atomic.Value // string Authorization header
}

func newFakeEmbedder(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *fakeEmbedder {
	t.Helper()
	fe := &fakeEmbedder{}
	fe.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fe.calls.Add(1)
		fe.auth.Store(r.Header.Get("Authorization"))
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			http.Error(w, "wrong route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		fe.last.Store(body)
		reply(w, r)
	}))
	t.Cleanup(fe.srv.Close)
	return fe
}

func (fe *fakeEmbedder) base() string { return fe.srv.URL + "/v1" }

func replyVector(vec string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":`+vec+`}],"model":"m"}`)
	}
}

func TestEmbedExplicitEndpoint(t *testing.T) {
	for _, mode := range []dbstore.Mode{dbstore.Memory, dbstore.Persistent} {
		t.Run(string(mode), func(t *testing.T) {
			fe := newFakeEmbedder(t, replyVector(`[1,0,0,0.5]`))
			f := newFixture(t, mode)
			f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "nomic-embed-text@v1.5"}, true)
			t.Setenv("DBVEC_TEST_KEY", "sk-secret-value")
			out := f.must(OpPut, Input{Collection: "e", ID: "a", Text: "hello", Endpoint: fe.base(), APIKeyEnv: "DBVEC_TEST_KEY"}, false)
			if r := out.Result.(map[string]any); r["revision"] != int64(1) {
				t.Fatalf("put: %+v", r)
			}
			body := fe.last.Load().(map[string]any)
			if body["model"] != "nomic-embed-text@v1.5" || body["input"] != "hello" {
				t.Fatalf("request body: %+v", body)
			}
			if got := fe.auth.Load().(string); got != "Bearer sk-secret-value" {
				t.Fatalf("auth header: %q", got)
			}
			if b64 := f.get("e", "a").Vector.B64; b64 != encodeVector([]float32{1, 0, 0, 0.5}).B64 {
				t.Fatalf("stored vector %s", b64)
			}
			// Search by text embeds the query the same way, without a key.
			if got := f.search(Input{Collection: "e", Text: "hello", Endpoint: fe.base()}); !strings.Contains(got, "a@") {
				t.Fatalf("text search: %s", got)
			}
			if got := fe.auth.Load().(string); got != "" {
				t.Fatalf("key sent without --api-key-env: %q", got)
			}
			if n := fe.calls.Load(); n != 2 {
				t.Fatalf("calls: %d", n)
			}
		})
	}
}

func TestEmbedRefusals(t *testing.T) {
	fe := newFakeEmbedder(t, replyVector(`[1,0,0,0]`))
	f := newFixture(t, dbstore.Memory)
	f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "cosine", Model: "m1"}, true)
	t.Setenv("DBVEC_EMPTY_KEY", "")
	for name, in := range map[string]Input{
		"no endpoint":         {Collection: "e", Text: "x"},
		"text and vector":     {Collection: "e", Text: "x", Vector: json.RawMessage(`[1,0,0,0]`), Endpoint: fe.base()},
		"endpoint for vector": {Collection: "e", Vector: json.RawMessage(`[1,0,0,0]`), Endpoint: fe.base()},
		"userinfo":            {Collection: "e", Text: "x", Endpoint: "http://u:p@127.0.0.1:1/v1"},
		"query":               {Collection: "e", Text: "x", Endpoint: fe.base() + "?key=k"},
		"scheme":              {Collection: "e", Text: "x", Endpoint: "file:///v1"},
		"empty key env":       {Collection: "e", Text: "x", Endpoint: fe.base(), APIKeyEnv: "DBVEC_EMPTY_KEY"},
		"bad key env name":    {Collection: "e", Text: "x", Endpoint: fe.base(), APIKeyEnv: "A-B"},
		"other model":         {Collection: "e", Text: "x", Endpoint: fe.base(), Model: "m2"},
	} {
		if c := f.code(OpPut, in, false); c != "invalid_input" {
			t.Errorf("%s: %s", name, c)
		}
	}
	if c := f.code(OpPut, Input{Collection: "e", Text: strings.Repeat("x", MaxTextBytes+1), Endpoint: fe.base()}, false); c != "limit_exceeded" {
		t.Errorf("oversize text: %s", c)
	}
	if c := f.code(OpPut, Input{Collection: "nope", Text: "x", Endpoint: fe.base()}, false); c != "not_found" {
		t.Errorf("missing collection: %s", c)
	}
	if n := fe.calls.Load(); n != 0 {
		t.Fatalf("refused requests reached the endpoint %d times", n)
	}
}

func TestEmbedBadReplies(t *testing.T) {
	f := newFixture(t, dbstore.Memory)
	f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, true)
	for name, tc := range map[string]struct {
		reply func(http.ResponseWriter, *http.Request)
		code  string
	}{
		"bad dimension": {replyVector(`[1,2,3]`), "invalid_input"},
		"not finite":    {replyVector(`[1,2,3,1e999]`), "invalid_input"},
		"malformed":     {func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"data":`) }, "backend_error"},
		"two items": {func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"data":[{"embedding":[1,0,0,0]},{"embedding":[1,0,0,0]}]}`)
		}, "backend_error"},
		"base64":   {replyVector(`"AAAAAA=="`), "backend_error"},
		"status":   {func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusUnauthorized) }, "backend_error"},
		"oversize": {func(w http.ResponseWriter, _ *http.Request) { w.Write(make([]byte, maxEmbedResponse+10)) }, "limit_exceeded"},
	} {
		fe := newFakeEmbedder(t, tc.reply)
		if c := f.code(OpPut, Input{Collection: "e", ID: "a", Text: "x", Endpoint: fe.base()}, false); c != tc.code {
			t.Errorf("%s: %s, want %s", name, c, tc.code)
		}
	}
	if got := f.search(Input{Collection: "e", Vector: json.RawMessage(`[0,0,0,0]`)}); strings.Contains(got, "a@") {
		t.Fatalf("a failed embedding wrote a vector: %s", got)
	}
}

// A redirect is never followed, so a bearer key cannot leave the named host.
func TestEmbedRefusesRedirect(t *testing.T) {
	other := newFakeEmbedder(t, replyVector(`[1,0,0,0]`))
	fe := newFakeEmbedder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.base()+"/embeddings", http.StatusTemporaryRedirect)
	})
	f := newFixture(t, dbstore.Memory)
	f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, true)
	t.Setenv("DBVEC_TEST_KEY", "sk-secret-value")
	_, err := f.run(OpPut, Input{Collection: "e", Text: "x", Endpoint: fe.base(), APIKeyEnv: "DBVEC_TEST_KEY"}, false)
	if dbstore.Code(err) != "backend_error" || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("redirect: %v", err)
	}
	if n := other.calls.Load(); n != 0 {
		t.Fatalf("redirect followed %d times", n)
	}
}

func TestEmbedCancel(t *testing.T) {
	release := make(chan struct{})
	fe := newFakeEmbedder(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	f := newFixture(t, dbstore.Memory)
	f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, true)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := f.runCtx(ctx, OpPut, Input{Collection: "e", Text: "x", Endpoint: fe.base()}, false)
	if c := dbstore.Code(err); c != "cancelled" {
		t.Fatalf("cancel: %s %v", c, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

// Supplied vectors never touch the network: no endpoint exists to call.
func TestSuppliedVectorZeroNetwork(t *testing.T) {
	fe := newFakeEmbedder(t, replyVector(`[1,0,0,0]`))
	f := newFixture(t, dbstore.Memory)
	f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, true)
	f.must(OpPut, Input{Collection: "e", ID: "a", Vector: json.RawMessage(`[1,0,0,0]`)}, false)
	f.search(Input{Collection: "e", Vector: json.RawMessage(`[1,0,0,0]`)})
	if n := fe.calls.Load(); n != 0 {
		t.Fatalf("supplied vectors made %d calls", n)
	}
	saved := embedClient
	embedClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("network used for a supplied vector")
		return nil, nil
	})}
	defer func() { embedClient = saved }()
	f.must(OpPut, Input{Collection: "e", ID: "b", Vector: json.RawMessage(`[0,1,0,0]`)}, false)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
