package memory

import "time"

type Observation struct {
	IssueID          int64     `json:"issue_id"`
	Title            string    `json:"title,omitempty"`
	Tool             string    `json:"tool,omitempty"`
	Role             string    `json:"role,omitempty"`
	Outcome          string    `json:"outcome"`
	FilesTouched     []string  `json:"files_touched,omitempty"`
	Commits          int       `json:"commits,omitempty"`
	VerifyExit       int       `json:"verify_exit,omitempty"`
	GateExit         int       `json:"gate_exit,omitempty"`
	GateEventID      string    `json:"gate_event_id,omitempty"`
	KilledBy         string    `json:"killed_by,omitempty"`
	Summary          string    `json:"summary,omitempty"`
	FailedApproaches []string  `json:"failed_approaches,omitempty"`
	Tags             []string  `json:"tags,omitempty"`
	TraceID          string    `json:"trace_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}
