// Command fixtureapp is a minimal third-party app that speaks the
// dhnt-app-meta-v1 contract. It is the worked example an app author copies, and
// the end-to-end fixture for `bashy app --app`.
//
// The two halves are the whole contract: answer `meta --json` on STDOUT, and
// honour X-Forwarded-Prefix by emitting a <base href> with a trailing slash.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const meta = `{
  "schema_version": "dhnt-app-meta-v1",
  "name":  "fixture",
  "label": "Fixture",
  "icon":  "M4 8a2 2 0 0 1 2-2h3.5l2 2H18a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z",
  "tip":   "A worked example of the app contract",
  "mount": "fixture",
  "mode":  "proxy",
  "auth":  "public",
  "start": ["fixtureapp", "serve"]
}
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "meta" {
		// Deliberately noisy on stderr: a harness banner must not corrupt the
		// contract, so the probe reads stdout only.
		fmt.Fprintln(os.Stderr, "fixtureapp: telemetry on → /dev/null")
		switch os.Getenv("FIXTURE_META_MODE") {
		case "exact-limit":
			fmt.Print(meta)
			fmt.Print(strings.Repeat(" ", (64<<10)-len(meta)))
		case "oversize":
			fmt.Print(strings.Repeat("x", (64<<10)+1))
		case "sleep":
			time.Sleep(5 * time.Second)
			fmt.Print(meta)
		case "exit":
			os.Exit(7)
		default:
			fmt.Print(meta)
		}
		return
	}
	port := flag.String("port", "9911", "listen port")
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	_ = fs.Parse(nil)
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The trailing slash is the entire mechanism: <base> anchors on the
		// LAST '/', so without it every relative asset escapes the mount.
		prefix := strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/home", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><head><base href=%q></head><body>"+
			"<h1>Fixture</h1><p>path=%s</p><a href=\"home\">relative link</a></body>",
			prefix+"/", r.URL.Path)
	})
	fmt.Fprintf(os.Stderr, "fixtureapp on 127.0.0.1:%s\n", *port)
	_ = http.ListenAndServe("127.0.0.1:"+*port, nil)
}
