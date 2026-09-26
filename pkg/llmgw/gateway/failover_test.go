package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

type fakeBackend struct {
	name       string
	capacity   Capacity
	loaded     map[string]struct{}
	loadedOK   bool
	serve      func(http.ResponseWriter, *http.Request, []byte, func(*http.Response) error) Attempt
	loadedCall chan struct{}
}

func (b *fakeBackend) Name() string { return b.name }
func (b *fakeBackend) Capacity(context.Context) Capacity {
	return b.capacity
}
func (b *fakeBackend) Loaded(context.Context) (map[string]struct{}, bool) {
	if b.loadedCall != nil {
		select {
		case b.loadedCall <- struct{}{}:
		default:
		}
	}
	return b.loaded, b.loadedOK
}
func (b *fakeBackend) Serve(w http.ResponseWriter, r *http.Request, body []byte, modify func(*http.Response) error) Attempt {
	return b.serve(w, r, body, modify)
}

func TestServeWithFailoverRetriesPreWriteResponse(t *testing.T) {
	oldBreaker := DefaultBreaker
	DefaultBreaker = sched.NewBreaker()
	t.Cleanup(func() { DefaultBreaker = oldBreaker })

	firstBodies := make(chan string, 1)
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		firstBodies <- string(body)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "discarded")
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer secondServer.Close()
	firstURL, _ := url.Parse(firstServer.URL)
	secondURL, _ := url.Parse(secondServer.URL)
	first := ReverseProxyBackend("first", firstURL, nil)
	second := ReverseProxyBackend("second", secondURL, nil)

	request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader("old"))
	recorder := httptest.NewRecorder()
	modifyCalls := 0
	chosen, attempts, status := ServeWithFailover(recorder, request, []Backend{first, second}, []byte("replayed"), func(*http.Response) error {
		modifyCalls++
		return nil
	})
	if chosen != second || attempts != 2 || status != http.StatusOK {
		t.Fatalf("chosen=%v attempts=%d status=%d", chosen.Name(), attempts, status)
	}
	if recorder.Body.String() != "replayed" || <-firstBodies != "replayed" {
		t.Errorf("body was not replayed: client=%q", recorder.Body.String())
	}
	if modifyCalls != 1 {
		t.Errorf("modify calls = %d, want 1", modifyCalls)
	}
}

func TestServeWithFailoverDoesNotRetryAfterFirstByte(t *testing.T) {
	secondCalls := 0
	first := &fakeBackend{name: "first", serve: func(w http.ResponseWriter, _ *http.Request, _ []byte, _ func(*http.Response) error) Attempt {
		_, _ = io.WriteString(w, "partial")
		return Attempt{Status: http.StatusBadGateway, CanRetry: true}
	}}
	second := &fakeBackend{name: "second", serve: func(http.ResponseWriter, *http.Request, []byte, func(*http.Response) error) Attempt {
		secondCalls++
		return Attempt{Status: http.StatusOK, Committed: true}
	}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/chat", nil)
	chosen, attempts, status := ServeWithFailover(recorder, request, []Backend{first, second}, nil, nil)
	if chosen != first || attempts != 1 || status != http.StatusBadGateway {
		t.Fatalf("chosen=%v attempts=%d status=%d", chosen.Name(), attempts, status)
	}
	if secondCalls != 0 || recorder.Body.String() != "partial" {
		t.Fatalf("secondCalls=%d body=%q", secondCalls, recorder.Body.String())
	}
}

func TestPickCandidatesForFailoverUsesBreakerAndBudget(t *testing.T) {
	breaker := sched.NewBreaker()
	backends := []Backend{&fakeBackend{name: "a"}, &fakeBackend{name: "b"}, &fakeBackend{name: "c"}}
	breaker.Trip("a", time.Minute)
	got := PickCandidatesForFailover(backends, breaker)
	if len(got) != 2 || got[0].Name() != "b" || got[1].Name() != "c" {
		t.Fatalf("candidates = %v, %v", got[0].Name(), got[1].Name())
	}
}

func TestCleanFailoverErrorClassification(t *testing.T) {
	if isCleanFailoverError(nil) || isCleanFailoverError(context.Canceled) {
		t.Fatal("unexpected classification")
	}
	if !isCleanFailoverError(&testError{text: "503: " + cleanFailoverText}) {
		t.Fatal("clean failover error was not recognized")
	}
}

type testError struct{ text string }

func (e *testError) Error() string { return e.text }
