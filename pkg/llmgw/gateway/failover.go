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
	if len(candidates) == 0 {
		return nil, 0, 0
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
		outcome := backend.Serve(counted, r, body, modify)
		chosen = backend
		status = outcome.Status
		if counted.bytesWritten > 0 || outcome.Committed || !outcome.CanRetry {
			return chosen, attempts, status
		}
	}
	return chosen, attempts, status
}

func composeFailoverHooks(proxy *httputil.ReverseProxy, attempt *Attempt, modify func(*http.Response) error, backend string, bytesWritten *int64) {
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if *bytesWritten == 0 {
			if DefaultBreaker != nil {
				DefaultBreaker.Trip(backend, 0)
			}
			attempt.CanRetry = true
			attempt.Status = http.StatusBadGateway
			return
		}
		attempt.Committed = true
		attempt.Status = http.StatusBadGateway
		http.Error(w, "upstream failed mid-stream: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		attempt.Status = resp.StatusCode
		if (resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests) && *bytesWritten == 0 {
			if DefaultBreaker != nil {
				cooldown := sched.ParseRetryAfter(resp.Header.Get("Retry-After"))
				DefaultBreaker.Trip(backend, cooldown)
			}
			_ = drainAndClose(resp.Body)
			return errors.New(strconv.Itoa(resp.StatusCode) + ": " + cleanFailoverText)
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
