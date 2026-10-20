package watch

// Criteria is the declarative predicate: the form a flag or a rule file spells.
type Criteria struct {
	Source  string            `json:"source,omitempty"`
	Kinds   []string          `json:"kinds,omitempty"`
	Locator string            `json:"locator,omitempty"`
	Key     string            `json:"key,omitempty"`
	Field   string            `json:"field,omitempty"`
	After   string            `json:"after,omitempty"`
	Before  string            `json:"before,omitempty"`
	Text    string            `json:"text,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
	Any     []Criteria        `json:"any,omitempty"`
	All     []Criteria        `json:"all,omitempty"`
	Negate  bool              `json:"not,omitempty"`
}

// Compile turns criteria into a predicate.
func (c Criteria) Compile() (Predicate, error) { return nil, nil }

// NewMatch builds the match rule: a predicate on the event.
func NewMatch(name string, c Criteria) (Rule, error) { return nil, nil }

// NewChange builds the change rule: any change of a selected field set.
func NewChange(name string, fields []string, c Criteria) (Rule, error) { return nil, nil }
