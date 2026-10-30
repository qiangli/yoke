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
