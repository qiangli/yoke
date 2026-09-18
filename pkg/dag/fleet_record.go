package dag

import (
	"sort"
	"sync"
)

// attemptLog collects run records by task. Flattening follows the graph's
// topological order, so parallel completion order cannot change the report.
type attemptLog struct {
	mu      sync.Mutex
	order   []string
	records map[string][]RunRecord
}

func newAttemptLog(order []*Node) *attemptLog {
	tasks := make([]string, 0, len(order))
	for _, node := range order {
		tasks = append(tasks, node.Task.Name)
	}
	return &attemptLog{order: tasks, records: make(map[string][]RunRecord, len(tasks))}
}

func (l *attemptLog) add(record RunRecord) {
	l.mu.Lock()
	l.records[record.Task] = append(l.records[record.Task], record)
	l.mu.Unlock()
}

func (l *attemptLog) all() []RunRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	var records []RunRecord
	for _, task := range l.order {
		attempts := append([]RunRecord(nil), l.records[task]...)
		sort.SliceStable(attempts, func(i, j int) bool {
			return attempts[i].Attempt < attempts[j].Attempt
		})
		records = append(records, attempts...)
	}
	return records
}

// record is deliberately inert outside Engine.Run, where no attempt log is
// bound. Skip-classified attempts are omitted because they earned no verdict.
func (e *Engine) record(task *Task, worker *Worker, attempt int, res TaskResult) {
	if e.attempts == nil || res.Status == StatusConditionSkipped {
		return
	}
	e.attempts.add(RecordAttempt(task, worker, attempt, res))
}
