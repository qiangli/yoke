package webconsole

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	core "github.com/qiangli/yoke/pkg/shape"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/websession"
)

func TestShapeHTTPProgramPreviewArguments(t *testing.T) {
	old := osUserLookup
	osUserLookup = func() string { return "preview-operator" }
	t.Cleanup(func() { osUserLookup = old })
	home := t.TempDir()
	source := newNotesSource(t)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	shaping := &fleet.AppShaping{Kind: "cli", Build: []string{"fixture-build", "{output}"}, Test: []string{exe}}
	opts := ShapeOptions{Home: home, ProgramRunner: shapeverify.ProgramRunnerFunc(func(_ context.Context, run shapeverify.ProgramRun) error {
		if run.Argv[0] != "fixture-build" {
			return nil
		}
		in, e := os.Open(exe)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(run.Argv[1], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		return errorsJoinClose(e, out)
	})}
	caller := core.Caller{Principal: "os:preview-operator", Actor: "os:preview-operator", RepresentedUser: "os:preview-operator"}
	svc := (&shapedApp{name: "notes", repo: source, shaping: shaping, opts: opts}).candidateService()
	from := t.TempDir()
	if e = copyTree(source, from); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(from, "public", "index.html"), []byte("candidate"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := svc.Import(context.Background(), caller, core.Wish{Request: "preview arguments"}, from)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Verify(context.Background(), caller, c.ID); e != nil {
		t.Fatal(e)
	}
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	token, e := sessions.Mint("preview-operator")
	if e != nil {
		t.Fatal(e)
	}
	h := newTestHandler(t, Options{RequireLogin: true, Sessions: sessions, Panels: []Panel{{Name: "notes", Path: "/notes/", ShapedSource: source, Shaping: shaping, Auth: AuthSystem, Available: true}}, Shape: opts})
	headers := withCookie(&http.Cookie{Name: sessionCookie, Value: token})
	path := "/shape/notes/preview/" + c.ID + "/"
	query := url.Values{}
	for _, arg := range []string{"-test.run=^TestShapeHTTPPreviewChild$", "--", "shape", "--help", "literal;$(unchanged)"} {
		query.Add("arg", arg)
	}
	w := do(h, http.MethodGet, path+"?"+query.Encode(), "192.0.2.20:5555", headers)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "shape,--help,literal;$(unchanged)") {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
	w = do(h, http.MethodGet, path+"?arg="+url.QueryEscape(strings.Repeat("x", (32<<10)+1)), "192.0.2.20:5555", headers)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize %d %s", w.Code, w.Body.String())
	}
	for name, query := range map[string]string{"count": strings.Repeat("arg=x&", 129), "NUL": "arg=%00"} {
		t.Run(name, func(t *testing.T) {
			w := do(h, http.MethodGet, path+"?"+query, "192.0.2.20:5555", headers)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("argument limit %d %s", w.Code, w.Body.String())
			}
		})
	}
	w = do(h, http.MethodGet, path, "192.0.2.20:5555", headers)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Usage of") {
		t.Fatalf("default help %d %s", w.Code, w.Body.String())
	}
}
func errorsJoinClose(e error, f *os.File) error {
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func TestShapeHTTPPreviewChild(t *testing.T) {
	if len(flag.Args()) != 3 || flag.Args()[0] != "shape" {
		return
	}
	fmt.Print(strings.Join(flag.Args(), ","))
}
