package guard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/secrets"
)

// The vault matcher recognises a bashy vault secret typed into (or shown on) a
// page WITHOUT holding the secret: every value is reduced to a keyed digest
// the moment it is read, and only digests are kept — in memory, and in a 0600
// cache so a one-shot `bashy browser type` does not hit the vault each time.
//
// Short values are excluded from substring search (they occur in ordinary
// text); a short value still refuses an exact whole-value match.
const (
	minVaultExact     = 4
	minVaultSubstring = 8
	vaultCacheTTL     = 10 * time.Minute
	vaultFetchTimeout = 3 * time.Second
)

// vaultValues is the value source; a variable so tests supply values without
// a vault or a network. Production reads the secrets client.
var vaultValues = func(ctx context.Context, remote bool, fn func(string)) error {
	return secrets.EachVaultValue(ctx, remote, vaultFetchTimeout, fn)
}

type vaultDigests struct {
	Key     string           `json:"key"`     // hex HMAC key (per cache file)
	Fetched time.Time        `json:"fetched"` // last successful remote read
	Lens    map[int][]string `json:"lens"`    // value length -> hex digests
}

type vaultMatcher struct {
	key  []byte
	lens map[int]map[string]bool
}

var (
	vaultMu  sync.Mutex
	vaultMem *vaultMatcher
	vaultAt  time.Time
)

// resetVault drops the in-memory matcher (tests).
func resetVault() {
	vaultMu.Lock()
	vaultMem, vaultAt = nil, time.Time{}
	vaultMu.Unlock()
}

func vaultCachePath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "bashy", "browser-guard-vault.json")
}

// loadVault returns the matcher, refreshing the remote half when the cache
// is stale. Local render-cache values are always re-read (no network).
func loadVault(ctx context.Context) *vaultMatcher {
	vaultMu.Lock()
	defer vaultMu.Unlock()
	if vaultMem != nil && time.Since(vaultAt) < vaultCacheTTL {
		return vaultMem
	}
	var cached vaultDigests
	path := vaultCachePath()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &cached)
		}
	}
	key, _ := hex.DecodeString(cached.Key)
	if len(key) != 32 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		cached = vaultDigests{}
	}
	m := &vaultMatcher{key: key, lens: map[int]map[string]bool{}}
	add := func(v string) {
		if len(v) < minVaultExact {
			return
		}
		set := m.lens[len(v)]
		if set == nil {
			set = map[string]bool{}
			m.lens[len(v)] = set
		}
		set[m.digest(v)] = true
	}
	stale := time.Since(cached.Fetched) >= vaultCacheTTL
	if err := vaultValues(ctx, stale, add); err == nil && stale {
		cached.Fetched = time.Now()
	} else {
		// Remote unreachable or still fresh: keep the cached digests.
		for n, ds := range cached.Lens {
			for _, d := range ds {
				set := m.lens[n]
				if set == nil {
					set = map[string]bool{}
					m.lens[n] = set
				}
				set[d] = true
			}
		}
	}
	if path != "" {
		out := vaultDigests{Key: hex.EncodeToString(key), Fetched: cached.Fetched, Lens: map[int][]string{}}
		for n, set := range m.lens {
			for d := range set {
				out.Lens[n] = append(out.Lens[n], d)
			}
			sort.Strings(out.Lens[n])
		}
		if b, err := json.Marshal(out); err == nil {
			_ = os.MkdirAll(filepath.Dir(path), 0o700)
			tmp := path + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	vaultMem, vaultAt = m, time.Now()
	return m
}

func (m *vaultMatcher) digest(v string) string {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))
}

// equal reports whether s is exactly a vault value.
func (m *vaultMatcher) equal(s string) bool {
	if m == nil || len(s) < minVaultExact {
		return false
	}
	return m.lens[len(s)][m.digest(s)]
}

// spans finds vault values embedded in s (values of minVaultSubstring+).
func (m *vaultMatcher) spans(s string) []Span {
	if m == nil {
		return nil
	}
	var out []Span
	for n, set := range m.lens {
		if n < minVaultSubstring || n > len(s) {
			continue
		}
		for i := 0; i+n <= len(s); i++ {
			if set[m.digest(s[i:i+n])] {
				out = append(out, Span{Start: i, End: i + n, Kind: KindVault})
				i += n - 1
			}
		}
	}
	return out
}
