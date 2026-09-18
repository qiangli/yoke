package probe

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/network"

	"github.com/qiangli/yoke/pkg/browser/internal/cdpactions"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

func TestExtractScriptDefaultsAndPayloadParsing(t *testing.T) {
	js := cdpactions.ExtractScript(wire.Action{Scope: "main", Goal: "Sign in", Limit: 2, Offset: -5})
	for _, want := range []string{`SCOPE_SEL="main"`, `MATCH="Sign in"`, `LIMIT=2`, `OFFSET=0`} {
		if !strings.Contains(js, want) {
			t.Fatalf("script missing %s:\n%s", want, js)
		}
	}

	res := cdpactions.ParseExtractPayload(`{"content":"body","elements":"[1] <button>OK</button>","total":4,"truncated":true}`)
	if !res.Success || res.Content != "body" || res.Total != 4 || !res.Truncated {
		t.Fatalf("unexpected result: %#v", res)
	}
	errRes := cdpactions.ParseExtractPayload(`{"error":"extract: scope main not found"}`)
	if errRes.Success || errRes.Error == "" {
		t.Fatalf("expected extract error, got %#v", errRes)
	}
}

func TestActionValidationAndHelpers(t *testing.T) {
	if res, err := cdpactions.Evaluate(context.Background(), ""); err != nil || res.Error == "" {
		t.Fatalf("empty evaluate should return result error, res=%#v err=%v", res, err)
	}
	if got := cdpactions.KeyFor("Enter"); got == "Enter" {
		t.Fatalf("expected Enter to map to chromedp keyboard constant")
	}
	clickJS := cdpactions.ClickByTextScript(`Bob's`, "#app")
	if !strings.Contains(clickJS, `"Bob's"`) || !strings.Contains(clickJS, `"#app"`) {
		t.Fatalf("click script did not quote inputs: %s", clickJS)
	}
}

func TestFilterCookies(t *testing.T) {
	cookies := []*network.Cookie{
		{Name: "sid", Value: "1", Domain: ".example.com", Path: "/", Secure: true},
		{Name: "pref", Value: "2", Domain: "other.test", Path: "/"},
	}
	out := cdpactions.FilterCookies(cookies, "sid", "app.example.com")
	if len(out) != 1 || out[0].Name != "sid" || out[0].Domain != ".example.com" {
		t.Fatalf("unexpected cookies: %#v", out)
	}
}

func TestLiveProbeSkippedWhenUnavailable(t *testing.T) {
	ctx := context.Background()
	c := New("")
	if !c.Available(ctx) {
		t.Skip("no Chrome reachable at remote debugging endpoint")
	}
	defer c.Close()
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: "1+1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Data != "2" {
		t.Fatalf("unexpected live result: %#v", res)
	}
}

func TestSelectPageTargetPrefersReusableRealPage(t *testing.T) {
	targets := []pageTarget{
		{ID: "worker", Type: "service_worker", URL: "https://example.test/sw.js"},
		{ID: "blank", Type: "page", URL: "about:blank"},
		{ID: "meet", Type: "page", URL: "http://127.0.0.1:8637/"},
		{ID: "other", Type: "page", URL: "https://example.test/"},
	}
	if got := selectPageTarget(targets); got != "meet" {
		t.Fatalf("selected %q, want the first real page", got)
	}
	if got := selectPageTarget(targets[:2]); got != "blank" {
		t.Fatalf("selected %q, want existing blank page", got)
	}
}
