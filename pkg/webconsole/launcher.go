// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/coopauth"
)

func launcherDir(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(filepath.Join(absolute, "index.html"))
	if err != nil {
		return "", fmt.Errorf("apps: launcher index.html: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("apps: launcher index.html must be a file")
	}
	return absolute, nil
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	for _, p := range s.requestPanels(r) {
		if s.liveRegistered && p.Source == "registered" && firstSegment(r.URL.Path) == strings.Trim(p.Path, "/") {
			mount := strings.TrimSuffix(p.Path, "/")
			coopauth.Mount(mount, s.proxyTo(p)).ServeHTTP(w, r)
			return
		}
	}
	if s.opts.Launcher == "" {
		s.handleSPA(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	// Stock panels keep their own scripts and styles even with a replacement launcher.
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(spaFS, name); err == nil && !st.IsDir() {
			s.handleSPA(w, r)
			return
		}
	}
	if name == "" || name == "index.html" {
		doc, err := os.ReadFile(filepath.Join(s.opts.Launcher, "index.html"))
		if err != nil {
			http.Error(w, "launcher unreadable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(injectBase(doc, coopauth.BaseHref(r)))
		return
	}
	http.FileServer(http.Dir(s.opts.Launcher)).ServeHTTP(w, r)
}
