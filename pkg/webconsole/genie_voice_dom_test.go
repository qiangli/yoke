// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestDOMGenieVoice drives the wish box's mic in a real browser (Sprint 624
// V2). What it proves, in order:
//
//   - the mic shows itself only where the browser says it can recognize
//     speech ON THE DEVICE: it asks SpeechRecognition.available with
//     processLocally and quality "dictation" (the default quality is
//     "command", which a wish is not);
//   - what is spoken lands in the SAME textarea, after the text already
//     typed there, and nothing is submitted — the person still presses send;
//   - a browser that can only recognize speech in its vendor's cloud (no
//     available(), which is Safari today) gets NO mic and a hint saying how
//     to dictate with the operating system instead, never a silent cloud
//     call and never a disabled button with no reason;
//   - Alt+Shift+V opens the panel and starts dictation;
//   - closing the panel stops the recognizer.
func TestDOMGenieVoice(t *testing.T) {
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	base, ctx, errors := domEnv(t, Options{Panels: []Panel{notes},
		Shape: ShapeOptions{Home: t.TempDir(), WorkRoot: t.TempDir(), Delegate: &fakeShaper{}}})

	until := func(what, expr string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			var ok bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: never true: %s", what, expr)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	eval := func(what, expr string, out any) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, out)); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const panel = `document.querySelector('[data-bashy-genie-panel]')`
	const mic = `document.querySelector('[data-bgn-mic]')`
	const hint = `document.querySelector('[data-bgn-voice-hint]')`
	// A recognizer that reports on-device recognition and records what it
	// was asked for. The component reads window.SpeechRecognition when the
	// panel opens and when the mic is pressed, so installing it here is
	// enough — and it means no real audio, no real model, no network.
	const fake = `(() => {
	  window.__sr = { asked: [], instances: [], state: "available" };
	  class FakeSR {
	    constructor() { this.processLocally = false; window.__sr.instances.push(this); }
	    start() { this.started = true; }
	    stop() { this.started = false; if (this.onend) this.onend(); }
	    static available(o) { window.__sr.asked.push(o); return Promise.resolve(window.__sr.state); }
	    static install(o) { window.__sr.installed = o; return Promise.resolve(true); }
	  }
	  window.SpeechRecognition = FakeSR;
	  delete window.webkitSpeechRecognition;
	  return true;
	})()`

	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`[data-bashy-genie-btn]`),
		chromedp.Evaluate(fake, nil),
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
	); err != nil {
		t.Fatal(err)
	}
	until("the mic appears for on-device recognition", mic+` && !`+mic+`.hidden`)
	until("the OS-dictation hint is not shown as well", hint+`.hidden === true`)

	// The button itself says voice is available (the quiet indicator), and
	// its accessible name offers both ways in. The mark is re-callable
	// because a route can appear after the page loaded.
	eval("re-marking the button", `window.bashyVoice.markButton(), true`, nil)
	until("the shaping button carries the mic badge",
		`!!document.querySelector('[data-bashy-genie-btn].bgn-has-voice .bgn-voice-dot')`)
	var name string
	eval("the button's accessible name", `document.querySelector('[data-bashy-genie-btn]').getAttribute('aria-label')`, &name)
	if !strings.Contains(name, "type or speak") {
		t.Errorf("the button's accessible name is %q; it must offer typing and speaking", name)
	}
	var badges int
	eval("badge count after a second mark", `(window.bashyVoice.markButton(), document.querySelectorAll('.bgn-voice-dot').length)`, &badges)
	if badges != 1 {
		t.Errorf("%d badges after marking twice; want exactly one", badges)
	}
	// The dot sits inside the Flow mark itself, not in a corner badge, and
	// its state is carried by shape and name as well as colour.
	until("the dot is drawn inside the Flow mark in the ready state",
		`document.querySelector('[data-bashy-genie-btn] svg .bgn-voice-dot[data-state="on"] .bgn-vd-on') !== null`)
	var offName string
	eval("the host turns voice off", `(window.bashyVoice.setState("off"), document.querySelector('[data-bashy-genie-btn]').getAttribute('aria-label'))`, &offName)
	if !strings.Contains(offName, "voice is off") || strings.Contains(offName, "type or speak") {
		t.Errorf("the accessible name with voice off is %q; want it to say voice is off, once", offName)
	}
	until("off is a slashed ring, not a coloured dot",
		`!!document.querySelector('.bgn-voice-dot[data-state="off"] .bgn-vd-slash') && !document.querySelector('.bgn-voice-dot .bgn-vd-on')`)
	eval("the host turns voice back on", `window.bashyVoice.setState("on"), true`, nil)

	// It asked for on-device recognition at dictation quality.
	var asked struct {
		Langs          []string `json:"langs"`
		Quality        string   `json:"quality"`
		ProcessLocally bool     `json:"processLocally"`
	}
	eval("the availability question", `window.__sr.asked[0]`, &asked)
	if !asked.ProcessLocally || asked.Quality != "dictation" || len(asked.Langs) == 0 {
		t.Errorf("available() asked %+v; want processLocally true, quality dictation, a language", asked)
	}

	// The person types, then speaks: the spoken words are appended to what
	// is already in the SAME box, and the wish is not sent.
	if err := chromedp.Run(ctx,
		chromedp.SendKeys(`[data-bashy-genie-panel] textarea`, "make the title"),
		chromedp.Click(`[data-bgn-mic]`),
	); err != nil {
		t.Fatal(err)
	}
	until("the mic reports it is listening", mic+`.getAttribute('aria-pressed') === 'true'`)
	until("the shaping button's dot shows listening",
		`document.querySelector('[data-bashy-genie-btn] .bgn-voice-dot').getAttribute('data-state') === 'listening' && document.querySelector('[data-bashy-genie-btn]').getAttribute('aria-label').includes('listening')`)
	until("the recognizer was told to stay on the device", `!!window.__sr.instances.at(-1).processLocally`)
	until("the recognizer was started", `window.__sr.instances.at(-1).started === true`)
	eval("a recognized phrase", `(() => {
	  const r = window.__sr.instances.at(-1);
	  r.onresult({ resultIndex: 0, results: [{ 0: { transcript: "bigger and blue" }, isFinal: true }] });
	  return true;
	})()`, nil)
	var box string
	eval("the wish box", `document.querySelector('[data-bashy-genie-panel] textarea').value`, &box)
	if box != "make the title bigger and blue" {
		t.Errorf("the box reads %q; want the typed text with the spoken words appended", box)
	}
	var sent bool
	eval("whether anything was submitted", `!!(window.__sr.submitted)`, &sent)
	if sent {
		t.Error("dictation must never submit the wish by itself")
	}

	// Pressing again stops it; closing the panel stops it too.
	if err := chromedp.Run(ctx, chromedp.Click(`[data-bgn-mic]`)); err != nil {
		t.Fatal(err)
	}
	until("the mic stops on a second press", mic+`.getAttribute('aria-pressed') === 'false'`)
	eval("Alt+Shift+V", `(() => {
	  document.querySelector('[data-bashy-genie-panel] .bgn-row .bgn-b:nth-last-child(2)').click();
	  document.dispatchEvent(new KeyboardEvent('keydown', { altKey: true, shiftKey: true, code: 'KeyV', bubbles: true }));
	  return true;
	})()`, nil)
	until("the shortcut opens the panel", panel+`.hidden === false`)
	until("the shortcut starts dictation", mic+`.getAttribute('aria-pressed') === 'true'`)
	eval("closing the panel", `document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })), true`, nil)
	until("closing the panel stops the recognizer", mic+`.getAttribute('aria-pressed') === 'false'`)

	// A cloud-only browser: the API is there but cannot be asked to stay on
	// the device (Safari's prefixed implementation). No mic; the hint says
	// how to dictate with the system and why the mic is off.
	eval("a cloud-only recognizer", `(() => {
	  function CloudSR() {}
	  CloudSR.prototype.start = function () {};
	  window.SpeechRecognition = CloudSR;
	  return true;
	})()`, nil)
	if err := chromedp.Run(ctx,
		chromedp.Click(`[data-bashy-genie-btn]`),
		chromedp.WaitVisible(`[data-bashy-genie-panel] textarea`),
	); err != nil {
		t.Fatal(err)
	}
	until("the mic is withheld from a cloud-only browser", mic+`.hidden === true`)
	until("the OS-dictation hint takes its place", hint+`.hidden === false && `+hint+`.textContent.length > 20`)
	var why string
	eval("the hint", hint+`.textContent`, &why)
	if !strings.Contains(strings.ToLower(why), "cloud") {
		t.Errorf("the hint reads %q; it must say why the mic is off", why)
	}

	if thrown := errors(); len(thrown) > 0 {
		t.Fatalf("a script threw: %s", strings.Join(thrown, "; "))
	}
}
