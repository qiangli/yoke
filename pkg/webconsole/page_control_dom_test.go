//go:build verifydom

package webconsole

import (
	"encoding/json"
	"github.com/chromedp/chromedp"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDOMPageActions(t *testing.T) {
	base, ctx, errs := domEnv(t, Options{Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir(), Delegate: &fakeShaper{}}})
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/"), chromedp.WaitVisible(`[data-bashy-genie-btn]`), chromedp.Evaluate(`(() => {
 const form=document.createElement('div'); form.id='state-fixture'; form.innerHTML='<input readonly><input disabled><button hidden>old</button><a href="#">link</a><div contenteditable="true">editable</div><details><summary>more</summary></details>'; document.body.append(form);
 const run=(name,input)=>BashyPage.invoke(name,input);
 window.pageTest=run('shape.draft',{request:'draft only'}).then(()=>run('presentation.shape-only',{enabled:true})).then(()=>{
  if(!form.children[0].readOnly || !form.children[1].inert || !form.children[3].hidden || !form.inert || !form.querySelector("summary").hidden) throw Error('not restricted');
  return run('presentation.shape-only',{enabled:false});
 }).then(()=>{
  window.pageTestOK=form.children[0].readOnly && !form.children[1].readOnly && form.children[1].disabled && !form.children[1].inert && form.children[2].hidden && !form.children[3].hidden && !form.inert && !form.querySelector("summary").hidden && form.children[4].contentEditable === "true";
 }); return true;
})()`, &ok), chromedp.Poll(`window.pageTestOK === true`, nil)); err != nil {
		t.Fatal(err)
	}
	// Exercise the server-to-browser path, including the browser result receipt.
	var pages struct {
		Pages []struct {
			ID      string `json:"id"`
			Actions []any  `json:"actions"`
		} `json:"pages"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(base + "/api/page/console/pages")
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(response.Body).Decode(&pages)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(pages.Pages) > 0 && len(pages.Pages[0].Actions) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("browser did not advertise actions")
		}
		time.Sleep(20 * time.Millisecond)
	}
	body := `{"page_id":"` + pages.Pages[0].ID + `","action":"shape.draft","input":{"request":"through the server"}}`
	response, err := http.Post(base+"/api/page/console/invoke", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var command struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	err = json.NewDecoder(response.Body).Decode(&command)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for {
		response, err = http.Get(base + "/api/page/console/result?id=" + command.ID)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(response.Body).Decode(&command)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if command.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command did not complete: %+v", command)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('[data-bashy-genie-panel] textarea').value === 'through the server'`, &ok)); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("server command did not update the shared form")
	}
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/shape/console/"), chromedp.WaitVisible(`[data-bashy-genie-btn]`), chromedp.Evaluate(`BashyPage.describe().length === 0`, &ok)); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("protected page registered actions")
	}
	if got := errs(); len(got) > 0 {
		t.Fatalf("browser errors: %v", got)
	}
}
