// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/qiangli/yoke/pkg/coopauth"
)

const siteContentSecurityPolicy = "sandbox allow-scripts allow-forms allow-popups allow-downloads"

// siteHandler serves a registered site's live directory. It deliberately does
// not use http.FileServer: the site boundary includes dot-path, gitignore and
// resolved-symlink policy that must be checked before every open.
func siteHandler(source string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", siteContentSecurityPolicy)

		logical, ok := safeSitePath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}

		root, err := filepath.EvalSymlinks(source)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		root, err = filepath.Abs(root)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		candidate := filepath.Join(root, filepath.FromSlash(logical))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		resolvedRel, ok := siteRelative(root, resolved)
		if !ok || hasDotSiteSegment(resolvedRel) {
			http.NotFound(w, r)
			return
		}

		st, err := os.Stat(resolved)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if ignoredSitePath(root, logical, st.IsDir()) || ignoredSitePath(root, resolvedRel, st.IsDir()) {
			http.NotFound(w, r)
			return
		}

		if st.IsDir() {
			if !strings.HasSuffix(r.URL.Path, "/") {
				http.Redirect(w, r, coopauth.PrefixPath(r, r.URL.Path+"/"), http.StatusMovedPermanently)
				return
			}
			logical = strings.TrimSuffix(logical, "/")
			if logical != "" {
				logical += "/"
			}
			logical += "index.html"
			resolved = filepath.Join(resolved, "index.html")
			resolved, err = filepath.EvalSymlinks(resolved)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			resolvedRel, ok = siteRelative(root, resolved)
			if !ok || hasDotSiteSegment(resolvedRel) || ignoredSitePath(root, logical, false) || ignoredSitePath(root, resolvedRel, false) {
				http.NotFound(w, r)
				return
			}
		}

		f, err := os.Open(resolved)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err = f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			http.Error(w, "site file unreadable", http.StatusInternalServerError)
			return
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "site file unreadable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf("\"%x\"", h.Sum(nil)))
		http.ServeContent(w, r, filepath.Base(logical), st.ModTime(), f)
	})
}

// safeSitePath validates URL path syntax before filepath.Clean can erase the
// evidence. Backslash is rejected on every OS so a path cannot be benign on
// Unix and become traversal after the same record moves to Windows.
func safeSitePath(urlPath string) (string, bool) {
	if !strings.HasPrefix(urlPath, "/") || strings.ContainsRune(urlPath, '\x00') || strings.ContainsRune(urlPath, '\\') {
		return "", false
	}
	rel := strings.TrimPrefix(urlPath, "/")
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	return rel, true
}

func siteRelative(root, target string) (string, bool) {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func hasDotSiteSegment(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// ignoredSitePath applies the source root's .gitignore, including directory
// matches inherited by descendants. An unreadable or oversized ignore file
// fails closed by returning true.
func ignoredSitePath(root, rel string, isDir bool) bool {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil || len(b) > 64<<10 {
		return true
	}
	var patterns []gitignore.Pattern
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, gitignore.ParsePattern(line, nil))
		}
	}
	parts := strings.Split(strings.Trim(filepath.ToSlash(rel), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return false
	}
	matcher := gitignore.NewMatcher(patterns)
	for i := 1; i < len(parts); i++ {
		if matcher.Match(parts[:i], true) {
			return true
		}
	}
	return matcher.Match(parts, isDir)
}
