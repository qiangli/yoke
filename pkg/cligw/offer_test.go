package cligw

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

func TestSlashRegisteredOfferInventory(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(fleet.SeedsEnv, "off")
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
	cat := NewFleetCatalog(c)
	record := fleet.Command{Name: "offer-fixture", Exec: []string{"fixture"}, Synopsis: "Describe fixture", Effects: []string{"read"}, Timeout: "2s", Args: &fleet.CommandSchema{
		Positionals: []fleet.CommandParameter{{Name: "phrase", Required: true}},
		Flags:       []fleet.CommandFlag{{Name: "number", Type: "int"}, {Name: "enabled", Type: "bool", Default: "true"}},
	}}
	if err := c.SaveCommand(record); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRegisteredOffers([]string{record.Name}); err != nil {
		t.Fatal(err)
	}
	sel, err := cat.ParseModelSelector("registered-offers")
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.Candidates(context.Background(), sel, Filter{}); len(got) != 0 {
		t.Fatalf("offers in inference pool: %v", got)
	}
	rows := cat.Candidates(context.Background(), sel, Filter{Slash: record.Name})
	if len(rows) != 1 || rows[0].Tool != fleet.OffersTool {
		t.Fatalf("slash candidates %v", rows)
	}
	adapter, _ := c.Tool(fleet.OffersTool)
	cmd, _ := adapter.Command(record.Name)
	if cmd.Timeout != "2s" || !slices.Equal(cmd.Effects, record.Effects) {
		t.Fatalf("generated command %+v", cmd)
	}
	// Use the real print planner and chat resolver, without launching a process.
	argv := []string{"--number=10", "--enabled=false", "--", "--two words"}
	res, err := toolcmd.Run(context.Background(), adapter, cmd, "fixture", toolcmd.Options{Catalog: c, Agent: rows[0].Name, Argv: argv, DryRun: true})
	if err != nil || len(res.Argv) < 1 || !slices.Equal(res.Argv[1:], append([]string{record.Name}, argv...)) {
		t.Fatalf("print invocation %q: %v", res.Argv, err)
	}
	runner := &fakeRunner{res: toolcmd.Result{Outcome: toolcmd.OutcomeSuccess}}
	old := runToolCommand
	runToolCommand = runner.run
	t.Cleanup(func() { runToolCommand = old })
	server, err := NewServer(ServerOptions{Catalog: cat})
	if err != nil {
		t.Fatal(err)
	}
	server.router.lookPath = func(name string) (string, error) { return name, nil }
	t.Cleanup(func() { _ = server.Close() })
	call := func(input string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"registered-offers","messages":[{"role":"user","content":%q}]}`, input)))
		req.Header.Set("Authorization", "Bearer "+server.Token())
		req.Header.Set(FilterHeader, "slash="+record.Name)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		return w
	}
	w := call(`{"number":1e1,"enabled":false,"phrase":"--two words"}`)
	if w.Code != 200 || len(runner.calls) != 1 || !slices.Equal(runner.calls[0].Argv, argv) {
		t.Fatalf("status %d, body %s, calls %+v", w.Code, w.Body, runner.calls)
	}
	if w := call(`{"phrase":false}`); w.Code != 400 || len(runner.calls) != 1 {
		t.Fatalf("bad argument accepted: %d %s", w.Code, w.Body)
	}
	record.Effects = []string{"spend"}
	if err := c.SaveCommand(record); err != nil {
		t.Fatal(err)
	}
	if w := call(`{"phrase":"hi"}`); w.Code != 409 || len(runner.calls) != 1 {
		t.Fatalf("stale offer: %d %s", w.Code, w.Body)
	}
	if err := c.SetRegisteredOffers(nil); err != nil {
		t.Fatal(err)
	}
	if adapter, _ := c.Tool(fleet.OffersTool); len(adapter.Commands) != 0 {
		t.Fatal("withdrawn offer still present")
	}
}
