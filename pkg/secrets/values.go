package secrets

import (
	"context"
	"os"
	"time"
)

// EachVaultValue hands every vault secret VALUE this host can see to fn, for a
// caller that must recognise a secret without keeping it (the browser guard
// hashes each value and drops it). Nothing here logs, prints, or returns a
// value in an error.
//
// Sources, in order:
//  1. the local render cache `bashy secret env` writes (no network);
//  2. when remote is true and a token resolves, the vault's own listing over
//     the existing Client, bounded by timeout.
//
// A missing cache or an unreachable vault is not an error for the caller's
// purpose — it reports what it could read. The returned error is the remote
// failure, if any, so the caller can keep a previous result instead.
func EachVaultValue(ctx context.Context, remote bool, timeout time.Duration, fn func(value string)) error {
	if cp := cacheFile(); cp != "" {
		if b, err := os.ReadFile(cp); err == nil {
			for _, v := range ParseEnv(b) {
				if v != "" {
					fn(v)
				}
			}
		}
	}
	if !remote {
		return nil
	}
	c, err := Config{}.Resolve()
	if err != nil {
		return nil // no token: nothing remote to read
	}
	if timeout > 0 {
		c.HTTP.Timeout = timeout
	}
	type listed struct {
		items []Item
		err   error
	}
	ch := make(chan listed, 1)
	go func() {
		items, err := c.List()
		ch <- listed{items, err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case l := <-ch:
		if l.err != nil {
			return l.err
		}
		for _, it := range l.items {
			if it.Value != "" {
				fn(it.Value)
			}
		}
	}
	return nil
}
