package dbvec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/advice"
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

// Only text with an explicit endpoint declares network egress; the same op
// with a supplied vector declares none.
func TestSpecInputDeclaresNetOnlyForText(t *testing.T) {
	for _, tc := range []struct {
		op, in string
		net    bool
	}{
		{OpPut, `{"collection":"e","id":"a","vector":[1,0]}`, false},
		{OpSearch, `{"collection":"e","vector":[1,0]}`, false},
		{OpPut, `{"collection":"e","id":"a","text":"hi","endpoint":"http://127.0.0.1:1/v1"}`, true},
		{OpSearch, `{"collection":"e","text":"hi","endpoint":"http://127.0.0.1:1/v1"}`, true},
	} {
		op, a, ok, err := dblifecycle.LookupCategoryOp(tc.op, json.RawMessage(tc.in))
		if !ok || err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
		for _, mode := range []dbstore.Mode{dbstore.Memory, dbstore.Persistent} {
			s := op.Declare(json.RawMessage(tc.in), mode, a)
			if got := slices.Contains(s.Effects, "net.egress"); got != tc.net {
				t.Fatalf("%s %s %s: effects %v", tc.op, mode, tc.in, s.Effects)
			}
		}
	}
}

// An effect cap without net refuses text before the endpoint sees a request,
// on the direct path and through an owner session; supplied vectors pass.
func TestNetCapDeniesTextBeforeRequest(t *testing.T) {
	noNet, err := advice.ParseCap("read,write,destroy")
	if err != nil {
		t.Fatal(err)
	}
	withNet, _ := advice.ParseCap("read,write,destroy,net")
	denied := advice.WithCap(context.Background(), noNet)
	fe := newFakeEmbedder(t, replyVector(`[1,0,0,0]`))
	modes(t, func(t *testing.T, f *fixture) {
		f.must(OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, true)
		before := fe.calls.Load()
		for _, op := range []string{OpPut, OpSearch} {
			_, err := f.runCtx(denied, op, Input{Collection: "e", ID: "a", Text: "hi", Endpoint: fe.base()}, false)
			if dbstore.Code(err) != "unsupported" || !strings.Contains(err.Error(), "net.egress") {
				t.Fatalf("%s under a no-net cap: %v", op, err)
			}
		}
		if n := fe.calls.Load(); n != before {
			t.Fatalf("denied text reached the endpoint (%d calls)", n-before)
		}
		if _, err := f.runCtx(denied, OpPut, Input{Collection: "e", ID: "v", Vector: json.RawMessage(`[1,0,0,0]`)}, false); err != nil {
			t.Fatalf("supplied vector under a no-net cap: %v", err)
		}
		if _, err := f.runCtx(advice.WithCap(context.Background(), withNet), OpPut, Input{Collection: "e", ID: "a", Text: "hi", Endpoint: fe.base()}, false); err != nil {
			t.Fatalf("text under a net cap: %v", err)
		}
	})

	session := startOwner(t)
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: session, Name: "n"}
	o := ownerCall{t: t, session: session, target: target}
	o.ok("a", "create", Input{}, 0, false, "")
	o.ok("a", OpCollectionAdd, Input{Collection: "e", Dimension: 4, Metric: "l2", Model: "m"}, 0, true, "")
	before := fe.calls.Load()
	b, _ := json.Marshal(Input{Collection: "e", ID: "a", Text: "hi", Endpoint: fe.base()})
	_, err = dblifecycle.Call(denied, session, dblifecycle.Request{Op: OpPut, Target: target, Input: b, Wait: time.Second})
	if dbstore.Code(err) != "unsupported" || fe.calls.Load() != before {
		t.Fatalf("owner call under a no-net cap: %v (calls %d)", err, fe.calls.Load()-before)
	}
	res := o.ok("a", OpPut, Input{Collection: "e", ID: "a", Text: "hi", Endpoint: fe.base()}, 0, false, "")
	if !slices.Contains(res.Effects, "net.egress") {
		t.Fatalf("owner response effects %v", res.Effects)
	}
	if res := o.ok("a", OpPut, Input{Collection: "e", ID: "v", Vector: json.RawMessage(`[1,0,0,0]`)}, 0, false, ""); slices.Contains(res.Effects, "net.egress") {
		t.Fatalf("supplied vector declared %v", res.Effects)
	}
}
