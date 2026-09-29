// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/ladder"
)

// readLadderEventsFn is a seam for tests: it must never read ~/.bashy in tests.
var readLadderEventsFn = defaultReadLadderEvents

func defaultReadLadderEvents() ([]ladder.Event, error) {
	path := ladder.DefaultStorePath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	st, err := ladder.OpenStore(path)
	if err != nil {
		return nil, err
	}
	return st.Read()
}

type dutyLinesView struct {
	L3Code   *float64 `json:"L3Code"`
	L4Code   *float64 `json:"L4Code"`
	L4Manage *float64 `json:"L4Manage"`
	L5Code   *float64 `json:"L5Code"`
	L5Manage *float64 `json:"L5Manage"`
	L5Judge  *float64 `json:"L5Judge"`
}

func newDutyLinesView(l ladder.Lines) *dutyLinesView {
	lineVal := func(v float64) *float64 {
		if v <= 0 {
			return nil
		}
		return &v
	}
	return &dutyLinesView{
		L3Code:   lineVal(l.L3Code),
		L4Code:   lineVal(l.L4Code),
		L4Manage: lineVal(l.L4Manage),
		L5Code:   lineVal(l.L5Code),
		L5Manage: lineVal(l.L5Manage),
		L5Judge:  lineVal(l.L5Judge),
	}
}

type dutyRowView struct {
	Rank        int      `json:"rank"`
	Separable   bool     `json:"separable"`
	Agent       string   `json:"agent"`
	R           float64  `json:"r"`
	RD          float64  `json:"rd"`
	Lower       float64  `json:"lower"`
	Events      int      `json:"events"`
	Established bool     `json:"established"`
	Band        int      `json:"band"`
	Provisional int      `json:"provisional,omitempty"`
	Missing     []string `json:"missing,omitempty"`
	Currency    string   `json:"currency,omitempty"`
	Move        string   `json:"move"`
	Cost        *float64 `json:"cost,omitempty"`
	RPerDollar  *float64 `json:"r_per_dollar,omitempty"`
}

type dutyLeaderboardView struct {
	SchemaVersion string                   `json:"schema_version"`
	Season        int                      `json:"season"`
	Lines         *dutyLinesView           `json:"lines,omitempty"`
	Duties        map[string][]dutyRowView `json:"duties"`
	Rows          []dutyRowView            `json:"rows"`
	Provenance    string                   `json:"provenance,omitempty"`
	Unavailable   string                   `json:"unavailable,omitempty"`
	Note          string                   `json:"note,omitempty"`
}

type agentEventsView struct {
	Agent       string         `json:"agent"`
	Events      []ladder.Event `json:"events"`
	Unavailable string         `json:"unavailable,omitempty"`
}

func parseDuties(s string) ([]ladder.Duty, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all":
		return []ladder.Duty{ladder.DutyCode, ladder.DutyManage, ladder.DutyJudge}, nil
	case string(ladder.DutyCode):
		return []ladder.Duty{ladder.DutyCode}, nil
	case string(ladder.DutyManage):
		return []ladder.Duty{ladder.DutyManage}, nil
	case string(ladder.DutyJudge):
		return []ladder.Duty{ladder.DutyJudge}, nil
	default:
		return nil, fmt.Errorf("leaderboard: unknown duty %q (code, manage, judge, all)", s)
	}
}

// handleDutyLeaderboard is GET /api/sprint/leaderboard/duty.
func (s *server) handleDutyLeaderboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	v := dutyLeaderboardView{
		SchemaVersion: capability.LeaderboardDutySchema,
		Duties:        make(map[string][]dutyRowView),
		Rows:          []dutyRowView{},
		Provenance:    capability.Provenance,
	}

	events, err := readLadderEventsFn()
	if err != nil {
		v.Unavailable = "the ladder store could not be read: " + err.Error()
		writeJSON(w, http.StatusOK, v)
		return
	}

	if len(events) == 0 {
		v.Note = "no rated events yet"
		v.Duties["code"] = []dutyRowView{}
		v.Duties["manage"] = []dutyRowView{}
		v.Duties["judge"] = []dutyRowView{}
		writeJSON(w, http.StatusOK, v)
		return
	}

	duties, err := parseDuties(r.URL.Query().Get("duty"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	season, _ := strconv.Atoi(r.URL.Query().Get("season"))
	if season <= 0 {
		season = 1
		for _, e := range events {
			if e.Season > season {
				season = e.Season
			}
		}
	}

	board := capability.ComputeDutyBoard(events, season, nil, duties)
	v.Season = board.Season
	v.Lines = newDutyLinesView(board.Lines)

	isCost := r.URL.Query().Get("cost") == "1" || r.URL.Query().Get("cost") == "true"
	var costs map[string]float64
	if isCost {
		costs = capability.RatedCost(events, season)
		v.Note = "informational — routing only, never promotes"
	}

	for _, duty := range duties {
		dName := string(duty)
		rows := board.Duties[dName]
		views := make([]dutyRowView, 0, len(rows))
		for _, rw := range rows {
			rowView := dutyRowView{
				Rank:        rw.Rank,
				Separable:   rw.Separable,
				Agent:       rw.Agent,
				R:           rw.R,
				RD:          rw.RD,
				Lower:       rw.Lower,
				Events:      rw.Events,
				Established: rw.Established,
				Band:        rw.Band,
				Provisional: rw.Provisional,
				Missing:     rw.Missing,
				Currency:    rw.Currency,
				Move:        rw.Move,
			}
			if isCost {
				if c, ok := costs[rw.Agent]; ok && c > 0 {
					costVal := c
					rpd := (rw.R - 1000) / c
					rowView.Cost = &costVal
					rowView.RPerDollar = &rpd
				}
			}
			views = append(views, rowView)
		}
		if isCost {
			sort.SliceStable(views, func(i, j int) bool {
				if (views[i].RPerDollar != nil) != (views[j].RPerDollar != nil) {
					return views[i].RPerDollar != nil
				}
				if views[i].RPerDollar != nil && views[j].RPerDollar != nil && *views[i].RPerDollar != *views[j].RPerDollar {
					return *views[i].RPerDollar > *views[j].RPerDollar
				}
				return views[i].Agent < views[j].Agent
			})
		}
		v.Duties[dName] = views
	}

	if len(duties) == 1 {
		v.Rows = v.Duties[string(duties[0])]
	} else if codeRows, ok := v.Duties["code"]; ok {
		v.Rows = codeRows
	}

	writeJSON(w, http.StatusOK, v)
}

// handleDutyAgentEvents is GET /api/sprint/leaderboard/duty/agent.
func (s *server) handleDutyAgentEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	v := agentEventsView{
		Agent:  name,
		Events: []ladder.Event{},
	}

	if name == "" {
		writeJSON(w, http.StatusOK, v)
		return
	}

	events, err := readLadderEventsFn()
	if err != nil {
		v.Unavailable = "the ladder store could not be read: " + err.Error()
		writeJSON(w, http.StatusOK, v)
		return
	}

	var agentEvents []ladder.Event
	for _, e := range events {
		if e.Agent == name {
			e.Note = sanitizePaths(e.Note)
			agentEvents = append(agentEvents, e)
		}
	}

	sort.Slice(agentEvents, func(i, j int) bool {
		return agentEvents[i].At.After(agentEvents[j].At)
	})

	if len(agentEvents) > 100 {
		agentEvents = agentEvents[:100]
	}

	v.Events = agentEvents
	writeJSON(w, http.StatusOK, v)
}

func sanitizePaths(s string) string {
	if strings.Contains(s, "/") || strings.Contains(s, "\\") {
		for _, part := range strings.Fields(s) {
			if strings.HasPrefix(part, "/") || strings.HasPrefix(part, "~/") {
				s = strings.ReplaceAll(s, part, "[redacted]")
			}
		}
	}
	return s
}
