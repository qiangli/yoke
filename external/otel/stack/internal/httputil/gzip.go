package httputil

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

func ReadGzipFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name + ".gz")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func GzipFileServer(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			if _, err := fs.Stat(fsys, strings.TrimPrefix(r.URL.Path, "/")+".gz"); err == nil {
				w.Header().Set("Content-Encoding", "gzip")
				r2 := r.Clone(r.Context())
				u := *r.URL
				u.Path += ".gz"
				r2.URL = &u
				files.ServeHTTP(w, r2)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
