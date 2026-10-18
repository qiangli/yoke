package adapt

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/lens"
)

func TestGenericFields(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"n":9007199254740993,"hide":"secret","nested":{"x":1}}`, "{\"n\":9007199254740993,\"nested\":{\"x\":1}}\n"},
		{`[{"n":1,"hide":2},{"nested":[true,null]}]`, "[{\"n\":1},{\"nested\":[true,null]}]\n"},
		{"{\"n\":1}\n{\"n\":2}", "{\"n\":1}\n{\"n\":2}\n"},
	} {
		out, err := projectJSON([]byte(tc.in), []string{"n", "nested", "absent"})
		if err != nil || string(out) != tc.want {
			t.Fatalf("%q %v", out, err)
		}
	}
	for _, bad := range []string{"", `null`, `[1]`, `{"n":1} secret`, `"secret"`} {
		out, err := projectJSON([]byte(bad), []string{"n"})
		if err == nil || len(out) != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%q %v", out, err)
		}
	}
	rows := curated(t)
	p, err := Build(rows, []string{"docker", "ps"}, Request{Options: map[string]string{"json": "true", "fields": "ID,Names"}})
	if err != nil || !reflect.DeepEqual(p.Fields, []string{"ID", "Names"}) || !reflect.DeepEqual(p.Argv, []string{"docker", "ps", "--format=json"}) {
		t.Fatal(p, err)
	}
	for _, s := range []string{"", ",", strings.Repeat("x", 4097)} {
		if _, err := Build(rows, []string{"docker", "ps"}, Request{Options: map[string]string{"json": "true", "fields": s}}); err == nil {
			t.Fatal("accepted invalid fields")
		}
	}
}

func TestPublishedLensMissingParserDoesNotExecute(t *testing.T) {
	argv, cat := helper(t)
	a, _ := cat.Adapter("fixture")
	a.JSON = []string{"lens: cmd:missing-secret"}
	if err := cat.SaveAdapter(a); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	var out, diag bytes.Buffer
	c := NewCmd(PrepareJSONLens, fleet.WithoutCloudOverlay())
	c.SetArgs(append([]string{"--json", "--fields=n", "--"}, append(argv, "marker", marker)...))
	c.SetOut(&out)
	c.SetErr(&diag)
	if code := ExitCode(c.Execute()); code != 125 {
		t.Fatal(code)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("executed missing parser")
	}
	if strings.Contains(out.String()+diag.String(), "secret") {
		t.Fatal("diagnostic leaked parser")
	}
}

func TestBoundedConversion(t *testing.T) {
	var out bytes.Buffer
	w := newBoundedOutput(context.Background(), &out, func(data []byte) ([]byte, error) {
		result, err := lens.Convert(context.Background(), lens.Request{From: "csv", To: "json", Data: data})
		return result.Data, err
	})
	_, _ = w.Write([]byte("name,n\nalice,2\n"))
	if err := w.Close(); err != nil || !strings.Contains(out.String(), `"alice"`) {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	called := false
	w = newBoundedOutput(context.Background(), &out, func(data []byte) ([]byte, error) { called = true; return data, nil })
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 10; i++ {
		if n, err := w.Write(chunk); n != len(chunk) || err != nil {
			t.Fatal(n, err)
		}
	}
	if w.data.Len() != 0 || w.Close() == nil || called || out.Len() != 0 {
		t.Fatal("overflow was not drained/refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = newBoundedOutput(ctx, &out, func(data []byte) ([]byte, error) { t.Fatal("conversion after cancellation"); return nil, nil })
	if w.Close() == nil {
		t.Fatal("cancel ignored")
	}
}

func TestProjectionPreservesChildStatusAndInput(t *testing.T) {
	argv, _ := helper(t)
	var out, diag bytes.Buffer
	// The helper's first line is a JSON argv array; its payload is invalid JSON.
	// Projection failure must not leak the payload or replace native status 37.
	p := Plan{Argv: argv, Fields: []string{"n"}}
	if code := Run(context.Background(), p, strings.NewReader("private-token"), &out, &diag, PrepareJSONLens); code != 37 {
		t.Fatal(code)
	}
	if out.Len() != 0 || strings.Contains(diag.String(), "private-token") {
		t.Fatal("leaked invalid input")
	}
	// Native success is preserved even when its non-JSON output cannot project.
	p.Argv = append(argv, "success")
	if code := Run(context.Background(), p, nil, io.Discard, io.Discard, PrepareJSONLens); code != 0 {
		t.Fatal(code)
	}
}

func TestCLIJSONProjection(t *testing.T) {
	argv, cat := helper(t)
	a, _ := cat.Adapter("fixture")
	a.JSON = []string{}
	if err := cat.SaveAdapter(a); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	c := NewCmd(PrepareJSONLens, fleet.WithoutCloudOverlay())
	c.SetArgs(append([]string{"--json", "--fields=payload,argv", "--"}, append(argv, "json", "a b", "", "--since=opaque")...))
	c.SetIn(strings.NewReader("piped payload"))
	c.SetOut(&out)
	c.SetErr(&diag)
	if code := ExitCode(c.Execute()); code != 37 {
		t.Fatal(code, diag.String())
	}
	if out.String() != `{"argv":["a b","","--since=opaque"],"payload":"piped payload"}`+"\n" || diag.Len() != 0 {
		t.Fatal(out.String(), diag.String())
	}
}
