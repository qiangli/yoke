package quotaobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RecordBestEffort bounds lock contention and never propagates journal failures.
func RecordBestEffort(ctx context.Context, o Observation) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Millisecond)
	defer cancel()
	if err := Record(ctx, o); err != nil {
		if errors.Is(err, ErrRealHomeWrite) {
			slog.Warn("quota observation refused", "source", o.Source, "error", err)
			return
		}
		slog.Debug("quota observation skipped", "source", o.Source, "error", err)
	}
}

// ObserveResponse captures only allowlisted response headers and, for errors,
// at most the first 2 KiB as the caller consumes the body. Successful bodies
// are never read or wrapped. Local admission responses are not vendor evidence.
func ObserveResponse(ctx context.Context, resp *http.Response, o Observation) {
	if resp.Header.Get("X-Bashy-Refusal") != "" {
		return
	}
	o.Status = resp.StatusCode
	o.Headers = make(map[string]string)
	for k, values := range resp.Header {
		key := strings.ToLower(k)
		if key == "retry-after" || strings.HasPrefix(key, "x-ratelimit-") || strings.HasPrefix(key, "anthropic-ratelimit-") {
			o.Headers[key] = strings.Join(values, ", ")
		}
	}
	if resp.StatusCode < 400 {
		if len(o.Headers) > 0 {
			RecordBestEffort(ctx, o)
		}
		return
	}
	if resp.Body == nil {
		RecordBestEffort(ctx, o)
		return
	}
	resp.Body = &observedBody{ReadCloser: resp.Body, ctx: ctx, observation: o}
}

type observedBody struct {
	io.ReadCloser
	ctx         context.Context
	observation Observation
	prefix      []byte
	once        sync.Once
}

func (b *observedBody) record() {
	b.once.Do(func() {
		b.observation.Excerpt = string(b.prefix)
		RecordBestEffort(b.ctx, b.observation)
	})
}
func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if remaining := 2048 - len(b.prefix); remaining > 0 {
		b.prefix = append(b.prefix, p[:min(n, remaining)]...)
	}
	if err != nil || len(b.prefix) == 2048 {
		b.record()
	}
	return n, err
}
func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	b.record()
	return err
}
