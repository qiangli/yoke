// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"
)

// runHTMLSource is the run view's template. It is embedded rather than inlined
// so it stays editable as HTML, and it is entirely self-contained: no CDN, no
// external stylesheet, no script. A viewer that needs the network is a viewer
// that does not work on the machine that ran the job.
//
//go:embed ui/run.html.tmpl
var runHTMLSource string

// indexHTMLSource is the served run index.
//
//go:embed ui/index.html.tmpl
var indexHTMLSource string

var htmlFuncs = template.FuncMap{
	"dur": fmtMS,
	"ts": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("2006-01-02 15:04:05")
	},
	"cls":  statusClass,
	"join": func(s []string) string { return strings.Join(s, " ") },
}

var runHTML = template.Must(template.New("run").Funcs(htmlFuncs).Parse(runHTMLSource))

var indexHTML = template.Must(template.New("index").Funcs(htmlFuncs).Parse(indexHTMLSource))

// statusClass maps a status onto a CSS class token. Statuses are a closed set
// produced by Status.String(), but this is defensive on purpose: the value
// reaches the page from report.json, which is a file on disk, and a class
// attribute is not a place to interpolate whatever a file happens to contain.
func statusClass(s string) string {
	for _, known := range []string{
		"pending", "running", "done", "failed",
		"skipped", "up-to-date", "condition-skipped",
	} {
		if s == known {
			return s
		}
	}
	return "unknown"
}

// htmlCell is one slot in the layered graph table. Task is nil where a layer
// has fewer targets than the widest one.
type htmlCell struct{ Task *RunTask }

// htmlRunView is the run page's template data.
//
// Live drives the one behavioural difference between the exported page and the
// served one: the export is a static artifact you can mail to someone, so it
// carries no script; the served page adds an SSE listener. Both render the same
// server-side markup first, so the served page is correct and readable before
// any script runs — and if scripting is off it simply stops updating.
type htmlRunView struct {
	Title  string
	Run    *RunEntry
	Tasks  []RunTask
	Levels []int
	Grid   [][]htmlCell
	Live   bool
	DocKey string
	RunID  string
	// StreamURL is built in Go rather than concatenated in the template so the
	// page interpolates ONE escaped value into script context instead of
	// stitching a URL together from three. LogTailBase is the same for the log
	// stream; the page appends only the (encodeURIComponent'd) log path.
	StreamURL   string
	LogTailBase string
}

// htmlIndexView is the served index's template data.
type htmlIndexView struct {
	Runs []RunSummary
	Live int
}

// renderIndexHTML renders the list of runs the viewer knows about.
func renderIndexHTML(runs []RunSummary, live int) ([]byte, error) {
	var buf bytes.Buffer
	if err := indexHTML.Execute(&buf, htmlIndexView{Runs: runs, Live: live}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderRunHTML renders one journaled run as a standalone page: a layered graph
// table (one column per topological layer) over the target list.
//
// The layered table is deliberately NOT a drawn graph. Columns come straight
// from the layer each node already carries in graph.json, so there is no layout
// algorithm, no edge routing, and nothing to get wrong — and for the mostly
// linear pipelines dag runs it reads better than a spaghetti diagram would.
func renderRunHTML(entry *RunEntry, graph *RunGraph) ([]byte, error) {
	return renderRunView(htmlRunView{
		Title: "dag run — " + runTitle(entry),
		Run:   entry,
		Tasks: entry.Tasks,
	}, entry, graph)
}

// renderRunPage renders a run for the server. When live is set the page adds
// the SSE listener; docKey/runID/eventCount let it resume the stream from where
// the server-side render already got to, so no event is shown twice and none is
// missed between render and subscribe.
func renderRunPage(entry *RunEntry, graph *RunGraph, live bool, docKey, runID string, eventCount int) ([]byte, error) {
	title := "dag run — " + runTitle(entry)
	if live {
		title = "▶ " + title
	}
	view := htmlRunView{
		Title: title, Run: entry, Tasks: entry.Tasks,
		Live: live, DocKey: docKey, RunID: runID,
	}
	if live {
		q := fmt.Sprintf("doc=%s&run=%s", url.QueryEscape(docKey), url.QueryEscape(runID))
		view.StreamURL = fmt.Sprintf("/events?%s&from=%d", q, eventCount)
		view.LogTailBase = "/logtail?" + q
	}
	return renderRunView(view, entry, graph)
}

func renderRunView(view htmlRunView, entry *RunEntry, graph *RunGraph) ([]byte, error) {
	if graph != nil {
		view.Levels, view.Grid = layerGrid(entry.Tasks, graph)
	}
	var buf bytes.Buffer
	if err := runHTML.Execute(&buf, view); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runTitle is the human label for a run: the DAG file's base name plus its
// targets, falling back to the run id.
func runTitle(entry *RunEntry) string {
	base := entry.File
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if len(entry.Targets) > 0 {
		return base + " " + strings.Join(entry.Targets, " ")
	}
	if base != "" {
		return base
	}
	return entry.RunID
}

// layerGrid arranges the run's targets into columns by topological layer.
// Targets present in the graph but absent from the report (never reached) still
// occupy their slot, so the shape of what did NOT run stays visible.
func layerGrid(tasks []RunTask, graph *RunGraph) ([]int, [][]htmlCell) {
	byName := make(map[string]*RunTask, len(tasks))
	for i := range tasks {
		byName[tasks[i].Name] = &tasks[i]
	}
	cols := map[int][]htmlCell{}
	maxLayer, maxRows := -1, 0
	for _, n := range graph.Nodes {
		cols[n.Layer] = append(cols[n.Layer], htmlCell{Task: byName[n.Name]})
		if n.Layer > maxLayer {
			maxLayer = n.Layer
		}
		if len(cols[n.Layer]) > maxRows {
			maxRows = len(cols[n.Layer])
		}
	}
	if maxLayer < 0 {
		return nil, nil
	}
	levels := make([]int, 0, maxLayer+1)
	for l := 0; l <= maxLayer; l++ {
		levels = append(levels, l)
	}
	grid := make([][]htmlCell, maxRows)
	for r := range grid {
		row := make([]htmlCell, len(levels))
		for c := range levels {
			if r < len(cols[c]) {
				row[c] = cols[c][r]
			}
		}
		grid[r] = row
	}
	return levels, grid
}
