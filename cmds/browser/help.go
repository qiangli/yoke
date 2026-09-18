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
			"The [N] index printed for each element is a CLICK ADDRESS:",
			"`browser click --index N` resolves through the identical enumeration.",
			"Hidden elements are omitted; --include-hidden keeps them and marks",
			"each `visible:false`, so \"absent\" and \"not displayed\" stay separable.",
			"`viewport` answers \"is a responsive breakpoint hiding this?\" without",
			"an eval that the page's CSP may forbid.",
		},
	},
	"click": {
		Name: "click", Args: "<css-selector> | <index> | --index N | --text STRING",
		One:  "click one element",
		Mode: "solo, probe, live",
		Out:  "envelope with content=\"clicked\"; a click that resolves NOTHING is success:false with a reason",
		Body: []string{
			"All three address forms return the same envelope shape. A bare integer",
			"operand is an element index (no CSS selector is a bare integer), the",
			"same numbering `extract` prints.",
		},
	},
	"type": {
		Name: "type", Args: "<css-selector> <text...>",
		One:  "focus an element and set its value",
		Mode: "solo, probe, live",
		Out:  "envelope with content=\"typed\"",
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
			"which fires a named event without compiling anything.",
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
		Name: "wait-for-selector", Args: "<css-selector>",
		One:  "block until an element is visible/attached/detached",
		Mode: "solo, probe, live",
		Out:  "envelope; success:false on timeout",
		Body: []string{"--timeout-ms sets the budget (default 5000); --state is visible|attached|detached."},
	},
	"screenshot": {
		Name: "screenshot", Args: "[path] | --output PATH | --base64",
		One:  "capture the DRIVEN tab as a PNG",
		Mode: "solo, probe, live",
		Out:  "by DEFAULT writes a PNG and prints only its path (envelope `path`). --base64 opts into inline base64 in `image`.",
		Body: []string{
			"The default is a file, not base64: an inline capture is ~200 KB of",
			"base64 and lands in an agent's context window unless every call is",
			"redirected defensively.",
			"The capture is of the tab every other subcommand drives, not of whatever",
			"is in the foreground, and it waits for a paint first (--settle-ms) so an",
			"image taken right after a click is not the pre-click frame.",
			"--full-page captures beyond the viewport.",
		},
	},
	"cookies-get": {
		Name: "cookies-get", Args: "[name [domain]]",
		One:  "read cookies (HttpOnly included in live mode)",
		Mode: "solo, probe, live",
		Out:  "envelope with data = a JSON array of cookie objects",
	},
	"scroll": {
		Name: "scroll", Args: "[up|down] [amount]",
		One:  "scroll the page",
		Mode: "solo, probe, live",
		Out:  "envelope with content describing the new scrollY",
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
		Name: "setup", Args: "[live|probe|solo] [dir]",
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
