package ask

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Media answers: `bashy ask --media photo|video|audio` asks the human for a
// photo or a recording instead of a typed value. The channel is a one-time page
// on loopback, opened in the person's own browser: a live camera preview, an
// optional teleprompter (--script) for the words to read, record / retake /
// use. The take is posted back to this process and delivered exactly like a
// typed answer — the agent gets a path, never has to drive the camera, and the
// person never types a command.

//go:embed media.html
var mediaPage string

var mediaTmpl = template.Must(template.New("media").Parse(mediaPage))

// maxMediaBytes bounds one take; a few minutes of 720p video fits easily.
const maxMediaBytes = 1 << 30

// mediaKinds are the accepted --media values.
var mediaKinds = map[string]bool{"photo": true, "video": true, "audio": true}

type mediaPageData struct {
	Kind       string
	Prompt     string
	ScriptJSON template.JS
	MaxSeconds int
	Requester  string
}

// openBrowser is a variable so tests can capture the URL instead of opening it.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// visibleSession reports whether a browser opened here would appear in front
// of a person: never inside a remote SSH session without a display.
func visibleSession() bool {
	if os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == "" {
		return true
	}
	return runtime.GOOS == "linux" && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "")
}

func readScript(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("ask: reading --script: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// obtainMedia serves the capture page, opens it, and waits for one take.
func obtainMedia(ctx context.Context, r Request, kind, script string, maxSeconds int, status io.Writer) ([]byte, error) {
	if !mediaKinds[kind] {
		return nil, fmt.Errorf("ask: --media must be photo, video or audio (got %q)", kind)
	}
	if !visibleSession() {
		return nil, errors.New("ask: --media needs a session whose screen a person can see (this one is remote); run it on the machine in front of them")
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	scriptJSON, err := json.Marshal(script)
	if err != nil {
		return nil, err
	}
	data := mediaPageData{
		Kind:       kind,
		Prompt:     r.Prompt,
		ScriptJSON: template.JS(scriptJSON),
		MaxSeconds: maxSeconds,
		Requester:  requesterLabel(r.Requester),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("ask: opening the capture page: %w", err)
	}
	got := make(chan []byte, 1)
	base := "/" + token + "/"
	mux := http.NewServeMux()
	mux.HandleFunc(base, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != base {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = mediaTmpl.Execute(w, data)
	})
	mux.HandleFunc(base+"upload", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		b, err := io.ReadAll(io.LimitReader(req.Body, maxMediaBytes+1))
		if err != nil || len(b) == 0 || len(b) > maxMediaBytes {
			http.Error(w, "empty or too large", http.StatusBadRequest)
			return
		}
		select {
		case got <- b:
			fmt.Fprintln(w, "received")
		default:
			http.Error(w, "already answered", http.StatusConflict)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	url := "http://" + ln.Addr().String() + base
	if err := openBrowser(url); err != nil {
		fmt.Fprintf(status, "ask: could not open a browser (%v); open this page on this machine: %s\n", err, url)
	} else {
		fmt.Fprintf(status, "ask: opened the %s page in your browser — waiting for the take\n", kind)
	}

	wait := time.Until(r.Expires)
	select {
	case b := <-got:
		return b, nil
	case <-time.After(wait):
		return nil, fmt.Errorf("ask: no %s arrived within %s", kind, wait.Round(time.Second))
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ask: generating a page token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
