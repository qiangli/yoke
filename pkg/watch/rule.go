package watch

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Criteria is the declarative predicate: the form a flag or a rule file spells.
// An empty Criteria matches every event, so a rule that means "anything from
// this source" needs no vocabulary at all.
type Criteria struct {
	Source  string            `json:"source,omitempty"`
	Kinds   []string          `json:"kinds,omitempty"`
	Locator string            `json:"locator,omitempty"` // glob
	Key     string            `json:"key,omitempty"`     // glob
	Field   string            `json:"field,omitempty"`
	After   string            `json:"after,omitempty"`
	Before  string            `json:"before,omitempty"`
	Text    string            `json:"text,omitempty"` // regexp over the event's text, then its raw record
	Attrs   map[string]string `json:"attrs,omitempty"`
	Any     []Criteria        `json:"any,omitempty"`
	All     []Criteria        `json:"all,omitempty"`
	Negate  bool              `json:"not,omitempty"`
}

// Compile turns criteria into a predicate. A bad regexp is an ERROR, never a
// predicate that matches nothing: a filter that silently never matches is the
// first way the hand-written watcher failed, and it failed silently for hours.
func (c Criteria) Compile() (Predicate, error) {
	var tests []Predicate

	if c.Source != "" {
		want := c.Source
		tests = append(tests, func(ev Event) bool { return ev.Source == want })
	}
	if len(c.Kinds) > 0 {
		want := map[string]bool{}
		for _, k := range c.Kinds {
			want[k] = true
		}
		tests = append(tests, func(ev Event) bool { return want[ev.Kind] })
	}
	if c.Locator != "" {
		pat := c.Locator
		tests = append(tests, func(ev Event) bool { return globMatch(pat, ev.Locator) })
	}
	if c.Key != "" {
		pat := c.Key
		tests = append(tests, func(ev Event) bool { return globMatch(pat, ev.Key) })
	}
	if c.Field != "" {
		want := c.Field
		tests = append(tests, func(ev Event) bool { return ev.Field == want })
	}
	if c.After != "" {
		want := c.After
		tests = append(tests, func(ev Event) bool { return valueEquals(ev.After, want) })
	}
	if c.Before != "" {
		want := c.Before
		tests = append(tests, func(ev Event) bool { return valueEquals(ev.Before, want) })
	}
	if c.Text != "" {
		re, err := regexp.Compile(c.Text)
		if err != nil {
			return nil, fmt.Errorf("watch: text pattern %q: %w", c.Text, err)
		}
		tests = append(tests, func(ev Event) bool {
			return re.MatchString(ev.Text) || (len(ev.Raw) > 0 && re.Match(ev.Raw))
		})
	}
	for name, want := range c.Attrs {
		n, w := name, want
		tests = append(tests, func(ev Event) bool { return ev.Attr(n) == w })
	}
	for _, sub := range c.All {
		p, err := sub.Compile()
		if err != nil {
			return nil, err
		}
		tests = append(tests, p)
	}
	if len(c.Any) > 0 {
		var alts []Predicate
		for _, sub := range c.Any {
			p, err := sub.Compile()
			if err != nil {
				return nil, err
			}
			alts = append(alts, p)
		}
		tests = append(tests, func(ev Event) bool {
			for _, p := range alts {
				if p(ev) {
					return true
				}
			}
			return false
		})
	}

	pred := func(ev Event) bool {
		for _, t := range tests {
			if !t(ev) {
				return false
			}
		}
		return true
	}
	if c.Negate {
		return func(ev Event) bool { return !pred(ev) }, nil
	}
	return pred, nil
}

func valueEquals(v any, want string) bool { return scalar(v) == want }

// globMatch matches '*' and '?' over ANY byte. path.Match is not usable here:
// it refuses to let '*' cross a '/', and half of these locators are paths.
func globMatch(pattern, s string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	return globHere(pattern, s)
}

func globHere(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			pattern = pattern[1:]
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globHere(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if len(s) == 0 || s[0] != pattern[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return len(s) == 0
}

// predRule is the match and change rules: both are a predicate plus a reason,
// and keeping them one implementation is why "change" cannot drift away from
// "match" in what it accepts.
type predRule struct {
	name   string
	reason string
	pred   Predicate
	fields map[string]bool // change rule: the selected field set (nil = any)
	kinds  map[string]bool // change rule: only real changes
}

func (r *predRule) Name() string { return r.name }

func (r *predRule) On(ev Event, now time.Time) []Alert {
	if r.kinds != nil && !r.kinds[ev.Kind] {
		return nil
	}
	if r.fields != nil && !r.fields[ev.Field] {
		return nil
	}
	if !r.pred(ev) {
		return nil
	}
	e := ev
	at := ev.At
	if at.IsZero() {
		at = now
	}
	fields := map[string]any{}
	if ev.Field != "" {
		fields["field"] = ev.Field
	}
	if ev.Key != "" {
		fields["key"] = ev.Key
	}
	if ev.After != nil {
		fields["after"] = scalar(ev.After)
	}
	if ev.Before != nil {
		fields["before"] = scalar(ev.Before)
	}
	return []Alert{{
		At: at, Rule: r.name, State: StateFire, Reason: r.reason,
		Locator: ev.Locator, Message: FormatEvent(ev), Fields: fields, Event: &e,
	}}
}

// Tick implements Rule: an event-driven rule has nothing to say about time.
func (r *predRule) Tick(time.Time) []Alert { return nil }

// NewMatch builds the match rule: a predicate on the event.
func NewMatch(name string, c Criteria) (Rule, error) {
	pred, err := c.Compile()
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = "match"
	}
	return &predRule{name: name, reason: ReasonMatch, pred: pred}, nil
}

// NewChange builds the change rule: any change of a selected field set. An
// empty field list means any added, removed or changed record.
func NewChange(name string, fields []string, c Criteria) (Rule, error) {
	pred, err := c.Compile()
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = "change"
	}
	r := &predRule{
		name: name, reason: ReasonChange, pred: pred,
		kinds: map[string]bool{KindAdded: true, KindRemoved: true, KindChanged: true},
	}
	if len(fields) > 0 {
		r.fields = map[string]bool{}
		for _, f := range fields {
			r.fields[f] = true
		}
	}
	return r, nil
}

// FormatEvent is the one-line human rendering of an observation, and the
// default alert message. Values are sanitized here, once, so no sink has to
// remember to.
func FormatEvent(ev Event) string {
	loc := ev.Locator
	if loc == "" {
		loc = ev.Source
	}
	switch ev.Kind {
	case KindChanged:
		if ev.Field != "" {
			return fmt.Sprintf("%s %s %s: %s -> %s", loc, ev.Key, ev.Field,
				SanitizeText([]byte(scalar(ev.Before))), SanitizeText([]byte(scalar(ev.After))))
		}
	case KindAdded, KindRemoved:
		if ev.Text != "" {
			return fmt.Sprintf("%s %s %s", loc, ev.Kind, SanitizeText([]byte(ev.Text)))
		}
		return fmt.Sprintf("%s %s %s", loc, ev.Kind, SanitizeText([]byte(ev.Key)))
	}
	if ev.Text != "" {
		return fmt.Sprintf("%s %s %s", loc, ev.Kind, SanitizeText([]byte(ev.Text)))
	}
	return fmt.Sprintf("%s %s", loc, ev.Kind)
}
