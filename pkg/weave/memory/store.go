package memory

import "context"

type Caps struct {
	Persistent bool
	Semantic   bool
	Graph      bool
}

type Query struct {
	Files   []string
	Title   string
	Tags    []string
	IssueID int64
	Limit   int
}

type Store interface {
	Remember(ctx context.Context, o Observation) error
	Recall(ctx context.Context, q Query) ([]Observation, error)
	Caps() Caps
}

type Prefs struct {
	Semantic bool
	Graph    bool
}

func Open(dir string, prefs Prefs) (Store, Caps, error) {
	_ = prefs
	st := NewJSONLStore(dir)
	caps := st.Caps()
	return st, caps, nil
}
