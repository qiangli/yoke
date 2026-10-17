package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

const failoverRetryBudget = 1

const cleanFailoverText = "upstream response is a failover candidate"

// PickCandidatesForFailover removes cooling backends when at least one backend
// is available, preserves rank order, and caps the result at two attempts.
func PickCandidatesForFailover(candidates []Backend, breaker *sched.Breaker) []Backend {
	if len(candidates) == 0 {
		return nil
	}
	available := candidates
	if breaker != nil {
		names := make([]string, 0, len(candidates))
		byName := make(map[string][]Backend, len(candidates))
		for _, backend := range candidates {
			if backend == nil {
				continue
			}
			name := backend.Name()
			names = append(names, name)
			byName[name] = append(byName[name], backend)
		}
		if healthyNames := breaker.FilterAvailable(names); len(healthyNames) > 0 {
			available = make([]Backend, 0, len(healthyNames))
			for _, name := range healthyNames {
				bucket := byName[name]
				if len(bucket) == 0 {
					continue
				}
				available = append(available, bucket[0])
				byName[name] = bucket[1:]
			}
		}
	}
	max := 1 + failoverRetryBudget
	if len(available) > max {
		available = available[:max]
	}
	return append([]Backend(nil), available...)
}

func pickCandidatesForFailover(candidates []Backend, breaker *sched.Breaker) []Backend {
	return PickCandidatesForFailover(candidates, breaker)
}

// ServeWithFailover serves candidates in order while the response remains
// untouched. It never starts another backend after a response byte is written.
func ServeWithFailover(w http.ResponseWriter, r *http.Request, candidates []Backend, body []byte, modify func(*http.Response) error) (chosen Backend, attempts, status int) {
	chosen, attempts, outcome := ServeWithFailoverEx(w, r, candidates, body, modify)
	return chosen, attempts, outcome.Status
}

// ServeWithFailoverEx is ServeWithFailover with the last attempt's outcome
// attached, so the caller can answer a classified upstream failure instead
// of a generic 502.
func ServeWithFailoverEx(w http.ResponseWriter, r *http.Request, candidates []Backend, body []byte, modify func(*http.Response) error) (chosen Backend, attempts int, outcome Attempt) {
	if len(candidates) == 0 {
		return nil, 0, Attempt{}
	}
	counted := &byteWriteCounter{ResponseWriter: w}
	for i, backend := range candidates {
		if counted.bytesWritten > 0 {
			break
		}
		if backend == nil {
			continue
		}
		attempts = i + 1
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", contentLengthString(len(body)))
		outcome = backend.Serve(counted, r, body, modify)
		chosen = backend
		if counted.bytesWritten > 0 || outcome.Committed || !outcome.CanRetry {
			return chosen, attempts, outcome
		}
	}
	return chosen, attempts, outcome
}

// errTerminalUpstream marks an upstream failure the gateway must NOT spend
// failover budget on: a dead credential or an exhausted balance gets no
// second attempt, on this or any other metered key.
var errTerminalUpstream = errors.New("upstream failure is not retryable")

func isTerminalUpstreamError(err error) bool { return errors.Is(err, errTerminalUpstream) }

func composeFailoverHooks(proxy *httputil.ReverseProxy, attempt *Attempt, modify func(*http.Response) error, backend string, bytesWritten *int64) {
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if *bytesWritten == 0 {
			if isTerminalUpstreamError(err) {
				// ModifyResponse already classified and stored the
				// stable status. Stop here; the handler answers the
				// caller with the classification.
				attempt.CanRetry = false
				return
			}
			if !isCleanFailoverError(err) {
				failure := ClassifyTransportError(err).WithEndpoint(backend, "")
				attempt.Failure = &failure
				attempt.Status = failure.Status
				attempt.CanRetry = failure.Retryable
				if DefaultBreaker != nil {
					DefaultBreaker.Trip(backend, 0)
				}
				return
			}
			if DefaultBreaker != nil {
				DefaultBreaker.Trip(backend, 0)
			}
			attempt.CanRetry = true
			return
		}
		attempt.Committed = true
		attempt.Status = http.StatusBadGateway
		http.Error(w, "upstream failed mid-stream: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		attempt.Status = resp.StatusCode
		attempt.UpstreamStatus = resp.StatusCode
		if *bytesWritten == 0 && resp.StatusCode >= 400 {
			var peek []byte
			if resp.Body != nil {
				peek, _ = io.ReadAll(io.LimitReader(resp.Body, maxClassifyBody))
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(nil))
			}
			failure := ClassifyUpstream(resp.StatusCode, peek, resp.Header).WithEndpoint(backend, "")
			switch failure.Class {
			case UpstreamAuth, UpstreamBilling:
				// Dead key or dry balance: fail fast with the
				// classification. Never retry, never fall through to
				// another metered key.
				attempt.Failure = &failure
				attempt.Status = failure.Status
				return errTerminalUpstream
			case UpstreamRateLimited, UpstreamTransient, UpstreamTimeout:
				attempt.Failure = &failure
				attempt.Status = failure.Status
				if DefaultBreaker != nil {
					cooldown := sched.ParseRetryAfter(resp.Header.Get("Retry-After"))
					DefaultBreaker.Trip(backend, cooldown)
				}
				return errors.New(strconv.Itoa(resp.StatusCode) + ": " + cleanFailoverText)
			default:
				// Any other 4xx is the caller's error in the
				// upstream's own shape: restore the peeked body
				// and fall through to pass it through untouched.
				resp.Body = io.NopCloser(bytes.NewReader(peek))
				resp.ContentLength = int64(len(peek))
				resp.Header.Set("Content-Length", strconv.Itoa(len(peek)))
			}
		}
		if modify != nil {
			if err := modify(resp); err != nil {
				return err
			}
		}
		attempt.Committed = true
		return nil
	}
}

func drainAndClose(body io.ReadCloser) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, probeBodyLimit))
	return body.Close()
}

type byteWriteCounter struct {
	http.ResponseWriter
	bytesWritten int64
}

func (w *byteWriteCounter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *byteWriteCounter) WriteString(s string) (int, error) {
	n, err := io.WriteString(w.ResponseWriter, s)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *byteWriteCounter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *byteWriteCounter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func isCleanFailoverError(err error) bool {
	return err != nil && strings.Contains(err.Error(), cleanFailoverText)
}

func contentLengthString(length int) string { return strconv.Itoa(length) }
