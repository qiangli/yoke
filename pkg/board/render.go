package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"text/tabwriter"
)

type JSONRenderer struct{}

func (JSONRenderer) Render(b *Board, _ Options) ([]byte, error) {
	out, err := json.MarshalIndent(b, "", "  ")
	if err == nil {
		out = append(out, '\n')
	}
	return out, err
}

type TerminalRenderer struct{}

func (TerminalRenderer) Render(b *Board, opts Options) ([]byte, error) {
	var out bytes.Buffer
	fmt.Fprintf(&out, "%s\n", strings.ToUpper(b.Title))
	fmt.Fprintf(&out, "%d todos · %d sprints · %d runs · %d need steward · %d unattended · ETA median %s\n", b.Summary.Todos, b.Summary.Sprints, b.Summary.Runs, b.Summary.NeedsSteward, b.Summary.Unattended, duration(b.Summary.ETAMedianSeconds))
	if b.Utilization != nil {
		fmt.Fprintf(&out, "%s\n", b.Utilization.Banner())
	}
	for _, lane := range b.Lanes {
		fmt.Fprintf(&out, "\n%s (%d):\n", lane.Title, len(lane.Cards))
		if len(lane.Cards) == 0 {
			fmt.Fprintln(&out, "  —")
			continue
		}
		for _, c := range lane.Cards {
			switch c.Layer {
			case "run":
				salvage := ""
				if c.Salvageable {
					salvage = fmt.Sprintf("  SALVAGEABLE: %d unmerged commit(s)", c.Unmerged)
				}
				stale := ""
				if c.Stale {
					stale = "  STALE"
				}
				fmt.Fprintf(&out, "  #%s %-28s %-10s %-3s %-20s age %s  elapsed %s  eta %s%s%s\n", c.ID, c.Label, dash(c.Tool), band(c.Band), dash(c.Model), duration(c.AgeSeconds), duration(c.Elapsed), duration(c.ETA), stale, salvage)
			default:
				fmt.Fprintf(&out, "  %s #%s %s [%s]\n", c.Layer, c.ID, c.Label, c.Scope)
			}
		}
		if lane.Dropped > 0 {
			fmt.Fprintf(&out, "  … %d more not shown\n", lane.Dropped)
		}
	}
	for _, p := range b.Panels {
		expanded := opts.Expand[p.ID] || opts.Expand["all"]
		mark := "+"
		if expanded {
			mark = "-"
		}
		fmt.Fprintf(&out, "\n[%s] %s — %s\n", mark, p.Title, p.Collapsed)
		if !expanded {
			continue
		}
		w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, strings.Join(p.Columns, "\t"))
		for _, row := range p.Rows {
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
		_ = w.Flush()
	}
	for _, w := range b.Warnings {
		fmt.Fprintf(&out, "\nwarning: %s\n", w)
	}
	return out.Bytes(), nil
}

type HTMLRenderer struct{}

func (HTMLRenderer) Render(b *Board, opts Options) ([]byte, error) {
	data := struct {
		*Board
		Open map[string]bool
	}{b, opts.Expand}
	var out bytes.Buffer
	err := boardHTML.Execute(&out, data)
	return out.Bytes(), err
}

var boardHTML = template.Must(template.New("board").Funcs(template.FuncMap{"dur": duration, "band": band}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title><style>
:root{color-scheme:light dark;--bg:#f5f7fb;--card:#fff;--ink:#18202b;--muted:#657184;--line:#dce2eb;--accent:#315efb;--warn:#b55300} @media(prefers-color-scheme:dark){:root{--bg:#11151b;--card:#1a2029;--ink:#edf2f7;--muted:#9da8b8;--line:#303947;--accent:#8aa4ff;--warn:#ffb45f}} *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 ui-sans-serif,system-ui,sans-serif}main{max-width:1500px;margin:auto;padding:clamp(16px,3vw,40px)}h1{margin:0}.sub{color:var(--muted);margin:.3rem 0 1.4rem}.summary,.lanes{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px}.metric,.lane,details{background:var(--card);border:1px solid var(--line);border-radius:12px}.metric{padding:14px}.metric b{display:block;font-size:1.5rem}.lanes{align-items:start;margin:18px 0}.lane{padding:12px;min-width:0}.lane h2{font-size:1rem;margin:0 0 10px}.card{border-left:3px solid var(--accent);padding:8px;margin:7px 0;background:color-mix(in srgb,var(--card),var(--accent) 5%);overflow-wrap:anywhere}.needs-steward .card{border-color:var(--warn)}.meta{font-size:.82rem;color:var(--muted)}details{margin:10px 0;padding:0 14px}summary{cursor:pointer;padding:14px 0;font-weight:700}summary span{font-weight:400;color:var(--muted)}.scroll{overflow:auto;margin-bottom:14px}table{border-collapse:collapse;width:100%;min-width:650px}th,td{text-align:left;padding:8px;border-bottom:1px solid var(--line)}th{color:var(--muted);font-size:.75rem}.warning{color:var(--warn)}@media(max-width:700px){.lanes{display:block}.lane{margin:10px 0}main{padding:14px}}
</style></head><body><main><h1>{{.Title}}</h1><p class="sub">Machine-global steward view · generated {{.GeneratedAt}}</p><section class="summary"><div class="metric"><b>{{.Summary.Todos}}</b>todos</div><div class="metric"><b>{{.Summary.Sprints}}</b>sprints</div><div class="metric"><b>{{.Summary.Runs}}</b>runs</div><div class="metric"><b>{{.Summary.NeedsSteward}}</b>need steward</div><div class="metric"><b>{{.Summary.Unattended}}</b>unattended</div><div class="metric"><b>{{dur .Summary.ETAMedianSeconds}}</b>ETA median</div></section><section class="lanes">{{range .Lanes}}<div class="lane {{.ID}}"><h2>{{.Title}} ({{len .Cards}})</h2>{{if .Cards}}{{range .Cards}}<div class="card"><b>{{.Layer}} #{{.ID}}</b> {{.Label}}<div class="meta">{{.State}}{{if .Tool}} · {{.Tool}} · {{band .Band}} · {{.Model}}{{end}} · age {{dur .AgeSeconds}}{{if .Tool}} · elapsed {{dur .Elapsed}} · eta {{dur .ETA}}{{end}}{{if .Stale}} · STALE{{end}}{{if .Salvageable}} · SALVAGEABLE: {{.Unmerged}} unmerged commit(s){{end}}</div></div>{{end}}{{else}}<span class="meta">—</span>{{end}}{{if .Dropped}}<p>… {{.Dropped}} more not shown</p>{{end}}</div>{{end}}</section>{{range .Panels}}<details id="{{.ID}}" {{if index $.Open .ID}}open{{end}}><summary>{{.Title}} <span>— {{.Collapsed}}</span></summary><div class="scroll"><table><thead><tr>{{range .Columns}}<th>{{.}}</th>{{end}}</tr></thead><tbody>{{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody></table></div></details>{{end}}{{range .Warnings}}<p class="warning">Warning: {{.}}</p>{{end}}</main></body></html>`))
