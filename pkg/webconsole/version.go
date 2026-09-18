// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"runtime/debug"
	"strings"
	"sync"
)

// Build identifies the binary serving this page.
type Build struct {
	Release string `json:"release"`          // the release a human quotes, e.g. v0.19.4
	Version string `json:"version"`          // full module version, or "devel"
	Commit  string `json:"commit,omitempty"` // short vcs revision
	Time    string `json:"time,omitempty"`   // vcs commit time
	Dirty   bool   `json:"dirty,omitempty"`  // built from a modified tree
	Go      string `json:"go,omitempty"`     // toolchain
	Assets  string `json:"assets,omitempty"` // content hash of the launcher script
}

var (
	buildOnce sync.Once
	buildInfo Build
)

// BuildOf reads the build stamp Go embeds at compile time.
//
// It is surfaced on the page for a reason that is not vanity: "am I looking at
// the build I just made?" was unanswerable from the UI, and a browser serving a
// cached script made a fixed bug keep reproducing. A visible commit — plus the
// asset hash, which changes whenever the launcher's own script does — turns that
// into a glance instead of an investigation.
func BuildOf() Build {
	buildOnce.Do(func() {
		b := Build{Version: "devel"}
		if bi, ok := debug.ReadBuildInfo(); ok {
			b.Go = bi.GoVersion
			if v := bi.Main.Version; v != "" && v != "(devel)" {
				b.Version = v
			}
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					b.Commit = s.Value
					if len(b.Commit) > 12 {
						b.Commit = b.Commit[:12]
					}
				case "vcs.time":
					b.Time = s.Value
				case "vcs.modified":
					b.Dirty = s.Value == "true"
				}
			}
		}
		// The asset hash answers the question the commit cannot: whether the
		// PAGE you are looking at came from this binary or from a cache.
		b.Assets = strings.Trim(etagFor("app.js"), `"`)
		b.Release = releaseOf(b.Version)
		buildInfo = b
	})
	return buildInfo
}

// releaseOf reduces a Go pseudo-version to the release a human would quote.
//
// A devel build reports v0.19.4-0.20260828123024-637ad0c9cf4c+dirty, whose only
// legible part is the leading tag. The commit is carried separately, so keeping
// the timestamp and hash here would just be the same data twice, badly.
func releaseOf(v string) string {
	if v == "" || v == "devel" {
		return "devel"
	}
	if i := strings.Index(v, "-0."); i > 0 {
		return v[:i]
	}
	if i := strings.Index(v, "+"); i > 0 {
		return v[:i]
	}
	return v
}

// Short renders the glanceable form: what a user quotes in a bug report.
func (b Build) Short() string {
	s := b.Version
	if b.Commit != "" {
		s += " · " + b.Commit
	}
	if b.Dirty {
		s += "-dirty"
	}
	return s
}
