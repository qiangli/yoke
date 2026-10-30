package browsercmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/coreutils/tool"
)

// subcommand documents ONE verb: its operands, its modes, and — the
// part that was missing and cost real time — its output contract.
//
// Fourteen subcommands used to share one help page that documented
// none of them: `diff <(browser tabs --help) <(browser --help)` was
// empty. `tabs`'s vocabulary and `screenshot`'s output contract could
// only be found by experiment, and both were found wrong.
type subcommand struct {
	Name string
	Args string
	One  string   // one-line summary
	Mode string   // which modes implement it
	Out  string   // the output contract: stream, encoding, envelope
	Body []string // detail lines
}

// guardHelp is the sensitive-input guard's contract, shown on the writing
// verbs. There is no flag that disables it.
var guardHelp = []string{
	"SENSITIVE-INPUT GUARD (every mode, every site, no opt-out): bashy refuses to",
	"type into a password field, an autocomplete username/password/one-time-code/cc-*",
	"field, or a field whose own name/id/label/aria/placeholder identifies ssn/tax id/ein/",
	"routing/bank account/card number/password/username — and refuses any SSN/ITIN, EIN, ABA",
	"routing number, Luhn card number, IBAN or bashy-vault secret as a value",
	"(type, select, eval, dispatch-event, clipboard write, navigate URL).",
	"A refusal asks the OPERATOR once, in a native dialog naming the site, the",
	"question and the reason: Enter it here / Use clipboard (bashy fills the field",
	"BY REFERENCE; the value never reaches the agent), I will type it myself (bashy",
	"focuses the field, the human types, then answers Done), Skip this question,",
	"or Stop; no answer = stop. The caller gets only the outcome:",
	"  guard:filled-by-human exit 0 | guard:human-typed exit 0 |",
	"  guard:skipped exit 4 | guard:stopped exit 5 | guard:fill-mismatch exit 6",
	"  (--json: outcome_class guard_filled_by_human|guard_human_typed|guard_skipped|",
	"  guard_stopped|guard_fill_mismatch, reason; data.ask_id when filled by reference)",
	"A fill by reference is VERIFIED where the value already is (the page): data.verified",
	"= match | mismatch, data.field_len, data.value_len — never the value, never a hash.",
	"A mismatch (the page rewrote, truncated or rejected it) is a failed fill: exit 6.",
	"A fill that could not run at all stops (exit 5, data.via fill-failed) with a",
	"value-free data.cause code (not-connected, tab-gone, not-found, refused, …).",
	"`browser type --from-ask <id> <target>` refills by reference with an answer the",
	"guard admitted for that same field, once; it never takes text.",
	"Read results pass through; password input values stay suppressed. Sensitive write",
	"and operator-answer records stay masked. Surrounding prose does not classify a field.",
}

var subcommands = map[string]subcommand{
	"status": {
		Name: "status", Args: "",
		One:  "report whether an action issued right now would reach a page",
		Mode: "solo, probe, live",
		Out:  "text `mode=… reachable=…`; --json emits a per-mode object (fields that do not apply to the mode are omitted, not defaulted)",
		Body: []string{
			"In live mode it reports the hub port, whether the extension is attached,",
			"its version against the required floor, and the driven tab — so an",
			"unreachable answer names which of those is missing.",
		},
	},
	"stop": {
		Name: "stop", Args: "",
		One:  "end the persistent probe browser session",
		Mode: "probe",
		Out:  "text naming the stopped endpoint; --json emits {success,mode,stopped,probe_url}",
	},
	"navigate": {
		Name: "navigate", Args: "<url>",
		One:  "load a page and return its title/url/content",
		Mode: "solo, probe, live",
		Out:  "envelope with title, url, content",
	},
	"extract": {
		Name: "extract", Args: "[scope-selector]",
		One:  "list the page's interactive elements and text",
		Mode: "solo, probe, live",
		Out:  "envelope with content (page text), elements (numbered listing), total, truncated, viewport, and --json `data` = {viewport, elements[]} as typed data",
		Body: []string{
			"The [N] index is an ephemeral address from this extraction:",
			"`browser click --index N` uses that enumeration; stale index writes are refused.",
			"Rows include stable_id, label, selector and name. Keep stable_id opaque and",
			"reuse selector verbatim (including frame/shadow qualification).",
			"Hidden elements are omitted; --include-hidden keeps them and marks",
			"each `visible:false`, so \"absent\" and \"not displayed\" stay separable.",
			"`viewport` answers \"is a responsive breakpoint hiding this?\" without",
			"an eval that the page's CSP may forbid.",
		},
	},
	"click": {
		Name: "click", Args: "<selector> | <index> | --index N | --text STRING",
		One:  "click one element and report what it did",
		Mode: "solo, probe, live",
		Out:  "envelope with content=\"clicked: navigated\" | \"clicked: dialog\" | \"clicked: dom-changed\" | \"clicked: no-effect\" (data.effect names it); a click that resolves NOTHING is success:false with a reason",
		Body: []string{
			"All three address forms return the same envelope shape. A bare integer",
			"operand is an element index (no CSS selector is a bare integer), the",
			"same numbering `extract` prints.",
			"The outcome is OBSERVED within a bounded watch after the dispatch:",
			"navigated (the URL moved), dialog (a DOM dialog opened), dom-changed",
			"(a mutation was seen), or no-effect (the click landed but the page",
			"did not react). no-effect is still success — the report is the result.",
		},
	},
	"text": {
		Name: "text", Args: "[selector] | [index] | --index N | --text STRING",
		One:  "read visible text without evaluating page script",
		Mode: "solo, probe, live",
		Out:  "envelope with content = the element's (or page's) rendered text; a password input reads as its name only, never its value",
		Body: []string{
			"With no target it reads the whole page (every frame in live mode).",
			"The shared selector syntax applies (CSS, xpath=, text=, label=, >>>,",
			"framecss:); a bare integer operand is an element index.",
			"Runs in the extension's isolated world (live) or a CDP isolated world",
			"(probe/solo) — nothing is compiled from a string, so a page whose",
			"Content-Security-Policy omits 'unsafe-eval' still answers where",
			"`eval` is refused. A read, never a write: the sensitive-input guard",
			"passes it through.",
		},
	},
	"type": {
		Name: "type", Args: "<selector> <text...> | --from-ask ID <selector>",
		One:  "focus an element and set its value",
		Mode: "solo, probe, live",
		Out:  "envelope with content=\"typed\"; a guarded field or value ends in content guard:filled-by-human or guard:human-typed (exit 0), guard:skipped (exit 4), guard:stopped (exit 5) or guard:fill-mismatch (exit 6, a by-reference fill the page did not keep), --json outcome_class guard_* + reason",
		Body: guardHelp,
	},
	"select": {
		Name: "select", Args: "<selector> | <index> | --index N | --text LABEL  <option...>",
		One:  "pick an option in a dropdown, combobox, listbox or radio group",
		Mode: "live",
		Out:  "envelope with content=\"selected\" and data.selected; an option that is absent or does not stick is success:false naming what the widget shows",
		Body: []string{
			"Works on a native <select>, a radio group (target the group or any member),",
			"and custom ARIA listboxes/comboboxes: opens the widget, types to filter when",
			"it is searchable, picks the option by name (exact > prefix > substring), then",
			"verifies. `extract` lists these widgets with their current value.",
			"The option text passes the sensitive-input value check (see `browser type --help`).",
		},
	},
	"eval": {
		Name: "eval", Args: "<javascript>",
		One:  "evaluate JavaScript in the page and return the value",
		Mode: "solo, probe, live",
		Out:  "envelope with data = the stringified value",
		Body: []string{
			"In live mode this compiles a string in the page's MAIN world, so a page",
			"whose Content-Security-Policy omits 'unsafe-eval' REFUSES it. That is a",
			"page policy, not a bashy limit — use `browser dispatch-event` instead,",
			"which fires a named event without compiling anything, or `browser text`",
			"to read visible text under the same policy.",
		},
	},
	"dispatch-event": {
		Name: "dispatch-event", Args: "<event-name> [--detail JSON] [--on window|document|<selector>]",
		One:  "fire a named DOM event without compiling a string",
		Mode: "solo, probe, live",
		Out:  "envelope with content=\"dispatched <name> on <target>\" and data {event,target,default_prevented}",
		Body: []string{
			"The remedy for `eval` under a strict CSP: nothing is compiled from a",
			"string, so no script-src directive applies. --detail is parsed as JSON",
			"into a CustomEvent detail (a non-JSON value is passed through as a string).",
		},
	},
	"wait-for-selector": {
		Name: "wait-for-selector", Args: "<selector>",
		One:  "block until an element is visible/attached/detached",
		Mode: "solo, probe, live",
		Out:  "envelope; success:false on timeout",
		Body: []string{"--timeout-ms sets the budget (default 5000); --state is visible|attached|detached."},
	},
	"screenshot": {
		Name: "screenshot", Args: "[path] | --output PATH | --base64",
		One:  "capture the DRIVEN tab as a PNG",
		Mode: "solo, probe, live",
		Out:  "by DEFAULT writes a PNG and prints only its path (envelope `path`). --base64 opts into inline base64 in `image`. --json `data` names the capture path taken (stitched_full_page vs full_page).",
		Body: []string{
			"The default is a file, not base64: an inline capture is ~200 KB of",
			"base64 and lands in an agent's context window unless every call is",
			"redirected defensively.",
			"The capture is of the tab every other subcommand drives, not of whatever",
			"is in the foreground, and it waits for a paint first (--settle-ms) so an",
			"image taken right after a click is not the pre-click frame.",
			"--full-page captures beyond the viewport AND stitches inner scroll",
			"areas tile by tile, so every panel appears at its full content height",
			"(rows below shift down). Page and panel positions are restored even",
			"when the stitch fails; a failed stitch is an explicit error, never",
			"a silent single capture (once panels exist that fallback would be",
			"missing rows below each panel's fold).",
		},
	},
	"cookies-get": {
		Name: "cookies-get", Args: "[name [domain]]",
		One:  "read cookies (HttpOnly included in live mode)",
		Mode: "solo, probe, live",
		Out:  "envelope with data = a JSON array of cookie objects",
	},
	"scroll": {
		Name: "scroll", Args: "[up|down] [amount] [selector] | --index N | --text STRING",
		One:  "scroll the page, or an inner panel around an element",
		Mode: "solo, probe, live",
		Out:  "envelope with content describing the new position (content names the panel for an addressed scroll)",
		Body: []string{
			"Without a target the window scrolls (default down 500). With one — a",
			"selector operand, --index N, --text, or --scope — the element's nearest",
			"scrollable ancestor moves instead, so a scroll meant for an inner panel",
			"never moves the window behind it.",
		},
	},
	"keyboard-press": {
		Name: "keyboard-press", Args: "<key>",
		One:  "send one key (\"Enter\", \"Tab\", \"Escape\", \"a\")",
		Mode: "solo, probe, live",
		Out:  "envelope with content",
	},
	"back": {
		Name: "back", Args: "",
		One:  "go back one history entry",
		Mode: "solo, probe, live",
		Out:  "envelope with the resulting page",
	},
	"tabs": {
		Name: "tabs", Args: "list | switch | new [url] | close",
		One:  "list and select tabs",
		Mode: "list: solo, probe, live · switch/new/close: live only",
		Out:  "envelope with content (human listing) and, under --json, data = a typed array of {index,id,title,url,active,driven}",
		Body: []string{
			"Address a tab with --url SUB or --title SUB rather than by index: an",
			"index captured at the start of a script is stale the moment any tab",
			"opens or closes. An ambiguous substring is an error listing the",
			"candidates, never a silent pick of the first.",
			"A bare integer operand is still accepted as a 1-based index.",
		},
	},
	"fetch": {
		Name: "fetch", Args: "<url>",
		One:  "plain HTTP GET (no browser) — `navigate` is the JS-executing form",
		Mode: "any",
		Out:  "the body on stdout; --json emits {url,status,status_code,headers,body,truncated}",
	},
	"hub": {
		Name: "hub", Args: "[--port N]",
		One:  "run the live-mode WebSocket hub in the foreground",
		Mode: "live",
		Out:  "progress on stdout, diagnostics on stderr (this is the ONLY subcommand that logs)",
	},
	"setup": {
		Name: "setup", Args: "[--no-open] [live|probe|solo] [dir]",
		One:  "print/extract what a mode needs before first use",
		Mode: "any",
		Out:  "instructions on stdout",
	},
	"login": {
		Name: "login", Args: "<url> --success-url|--token-selector|--cookie",
		One:  "drive a login flow and print the resulting token/cookie/url",
		Mode: "solo, probe, live",
		Out:  "the captured value on stdout; --json emits {done,reason,token,cookie,url}",
	},
}

func subcommandNames() []string {
	names := make([]string, 0, len(subcommands))
	for n := range subcommands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// printSubHelp renders one verb's page. Returns false when name is not
// a known subcommand, so the caller can fall through to the tool help.
func printSubHelp(rc *tool.RunContext, name string) bool {
	sc, ok := subcommands[name]
	if !ok {
		return false
	}
	fmt.Fprintf(rc.Out, "Usage: browser [--json] [--mode solo|probe|live] %s %s\n\n", sc.Name, sc.Args)
	fmt.Fprintf(rc.Out, "%s\n\n", sc.One)
	fmt.Fprintf(rc.Out, "Modes:  %s\n", sc.Mode)
	fmt.Fprintf(rc.Out, "Output: %s\n", sc.Out)
	if len(sc.Body) > 0 {
		fmt.Fprintln(rc.Out)
		for _, line := range sc.Body {
			fmt.Fprintf(rc.Out, "%s\n", line)
		}
	}
	fmt.Fprintf(rc.Out, "\nSee 'browser --help' for the global flags, or coreutils/docs/browser.md.\n")
	return true
}

// unknownSubcommand names the valid set. An error that knows a verb is
// invalid knows what would have been valid; withholding it turns
// discovery into a guessing game.
func unknownSubcommand(name string) string {
	return fmt.Sprintf("unknown subcommand %q; expected one of: %s",
		name, strings.Join(subcommandNames(), ", "))
}
