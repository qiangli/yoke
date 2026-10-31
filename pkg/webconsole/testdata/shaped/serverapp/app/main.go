// Command serverapp is the fixture for a shaped app that declares a server
// (shaped.json): it reports its revision, the path it was asked for and
// whether the console's session cookie reached it, and keeps one note in
// its data directory.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	addr := flag.String("addr", "", "listen address")
	data := flag.String("data", "", "data directory")
	flag.Parse()
	note := filepath.Join(*data, "note.txt")
	// An HTML page, for the console's page-level injection (the common
	// Genie button rides on a declared server's pages too).
	http.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><html><head><title>serverapp</title></head><body><h1>version=%s</h1></body></html>\n", version)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			if err := os.WriteFile(note, b, 0o600); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		_, cerr := r.Cookie("bashy_console")
		saved, _ := os.ReadFile(note)
		w.Header().Set("Cache-Control", "max-age=3600")
		fmt.Fprintf(w, "version=%s path=%s query=%s console-cookie=%t note=%s\n",
			version, r.URL.Path, r.URL.RawQuery, cerr == nil, saved)
	})
	log.Fatal(http.ListenAndServe(*addr, nil))
}
