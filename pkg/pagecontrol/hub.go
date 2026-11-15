// Package pagecontrol carries a private contract between a page and its server.
// The mounting application authenticates every request, supplies its canonical
// principal, and explicitly allows presentation actions. Domain mutations retain
// their existing APIs and authorization; a browser session is not a credential
// delegation mechanism for a less privileged CLI caller.
package pagecontrol

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Action struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
type Page struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Actions []Action `json:"actions"`
}
type Command struct {
	ID     string          `json:"id"`
	PageID string          `json:"page_id"`
	Action string          `json:"action"`
	Input  json.RawMessage `json:"input"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Options defaults to denying every action. AllowAction must enumerate actions
// whose authority is shared by ALL credentials allowed to reach this hub.
// Use separate hubs when credential classes have different capabilities.
type Options struct {
	AllowAction func(string) bool
	CommandTTL  time.Duration
	PageTTL     time.Duration
}
type livePage struct {
	Page
	principal string
	expires   time.Time
	queue     chan string
	conn      *websocket.Conn
}
type pending struct {
	Command
	principal string
	expires   time.Time
}
type Hub struct {
	mu       sync.Mutex
	pages    map[string]*livePage
	commands map[string]*pending
	options  Options
	closed   bool
}

func New(options ...Options) *Hub {
	o := Options{}
	if len(options) > 0 {
		o = options[0]
	}
	if o.CommandTTL <= 0 {
		o.CommandTTL = 30 * time.Second
	}
	if o.PageTTL <= 0 {
		o.PageTTL = 45 * time.Second
	}
	return &Hub{pages: map[string]*livePage{}, commands: map[string]*pending{}, options: o}
}

var actionName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

const maxBytes = 32 << 10

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		fail(w, 400, "invalid request body")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "expected one JSON value")
		return false
	}
	return true
}
func sameOrigin(r *http.Request, required bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return !required
	}
	u, e := url.Parse(origin)
	return e == nil && u.Host == r.Host && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == ""
}
func (h *Hub) allowed(name string) bool {
	return actionName.MatchString(name) && h.options.AllowAction != nil && h.options.AllowAction(name)
}

// Serve expects paths stripped to /events, /pages, /invoke, or /result.
// Authentication and HTTP cookie-CSRF checks remain the mounting application's
// responsibility. The websocket additionally requires a same-origin Origin.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, principal string) {
	if strings.TrimSpace(principal) == "" || len(principal) > 4096 {
		fail(w, 401, "authentication required")
		return
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		fail(w, 503, "page control unavailable")
		return
	}
	if !sameOrigin(r, false) {
		fail(w, 403, "cross-origin page control refused")
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/events":
		h.events(w, r, principal)
	case r.Method == "GET" && r.URL.Path == "/pages":
		h.mu.Lock()
		h.cleanLocked()
		pages := []Page{}
		for _, p := range h.pages {
			if p.principal == principal {
				copy := p.Page
				copy.Actions = append([]Action{}, p.Actions...)
				pages = append(pages, copy)
			}
		}
		h.mu.Unlock()
		sort.Slice(pages, func(i, j int) bool { return pages[i].ID < pages[j].ID })
		writeJSON(w, 200, map[string]any{"pages": pages})
	case r.Method == "POST" && r.URL.Path == "/invoke":
		h.invoke(w, r, principal)
	case r.Method == "GET" && r.URL.Path == "/result":
		h.mu.Lock()
		h.cleanLocked()
		c := h.commands[r.URL.Query().Get("id")]
		var result Command
		found := c != nil && c.principal == principal
		if found {
			result = c.Command
		}
		h.mu.Unlock()
		if !found {
			fail(w, 404, "command not found")
			return
		}
		writeJSON(w, 200, result)
	default:
		fail(w, 404, "page control operation not found")
	}
}
func (h *Hub) cleanLocked() {
	now := time.Now()
	for id, p := range h.pages {
		if now.After(p.expires) {
			h.closeLocked(id)
		}
	}
	for id, c := range h.commands {
		if now.After(c.expires) && (c.Status == "queued" || c.Status == "sent") {
			c.Status = "expired"
			c.Error = "command expired; outcome may be unknown; not replayed"
		}
		if now.After(c.expires.Add(5 * time.Minute)) {
			delete(h.commands, id)
		}
	}
}
func (h *Hub) closeLocked(id string) {
	if p := h.pages[id]; p != nil && p.conn != nil {
		_ = p.conn.Close()
	}
	delete(h.pages, id)
	for _, c := range h.commands {
		if c.PageID == id {
			switch c.Status {
			case "queued":
				c.Status = "failed"
				c.Error = "page disconnected before delivery"
			case "sent":
				c.Status = "expired"
				c.Error = "page disconnected; outcome unknown; not replayed"
			}
		}
	}
}
func (h *Hub) invoke(w http.ResponseWriter, r *http.Request, principal string) {
	var in struct {
		PageID string          `json:"page_id"`
		Action string          `json:"action"`
		Input  json.RawMessage `json:"input"`
	}
	if !body(w, r, &in) {
		return
	}
	if len(in.Input) > maxBytes {
		fail(w, 422, "input too large")
		return
	}
	if len(in.Input) == 0 {
		in.Input = json.RawMessage("null")
	}
	command, code, message := h.enqueue(principal, in.PageID, in.Action, in.Input)
	if message != "" {
		fail(w, code, message)
		return
	}
	writeJSON(w, code, command)
}

// enqueue performs only bounded in-memory work while holding the mutex. HTTP
// response writes happen after it returns; a slow caller cannot lock all pages.
func (h *Hub) enqueue(principal, pageID, action string, input json.RawMessage) (Command, int, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanLocked()
	if h.closed {
		return Command{}, 503, "page control unavailable"
	}
	p := h.pages[pageID]
	if p == nil || p.principal != principal {
		return Command{}, 404, "page not found"
	}
	if !h.allowed(action) {
		return Command{}, 403, "page action not allowed"
	}
	mounted := false
	for _, a := range p.Actions {
		if a.Name == action {
			mounted = true
			break
		}
	}
	if !mounted {
		return Command{}, 422, "page action not mounted"
	}
	count := 0
	for _, c := range h.commands {
		if c.principal == principal {
			count++
		}
	}
	if len(h.commands) >= 2048 || count >= 128 || len(p.queue) >= cap(p.queue) {
		return Command{}, 429, "page command capacity reached"
	}
	c := &pending{Command: Command{ID: uuid.NewString(), PageID: p.ID, Action: action, Input: input, Status: "queued"}, principal: principal, expires: time.Now().Add(h.options.CommandTTL)}
	h.commands[c.ID] = c
	p.queue <- c.ID
	return c.Command, 202, ""
}

func (h *Hub) events(w http.ResponseWriter, r *http.Request, principal string) {
	if !sameOrigin(r, true) {
		fail(w, 403, "same-origin websocket required")
		return
	}
	h.mu.Lock()
	h.cleanLocked()
	count := 0
	for _, p := range h.pages {
		if p.principal == principal {
			count++
		}
	}
	if len(h.pages) >= 256 || count >= 16 {
		h.mu.Unlock()
		fail(w, 429, "page capacity reached")
		return
	}
	p := &livePage{Page: Page{ID: uuid.NewString(), Actions: []Action{}}, principal: principal, expires: time.Now().Add(h.options.PageTTL), queue: make(chan string, 16)}
	h.pages[p.ID] = p
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.closeLocked(p.ID); h.mu.Unlock() }()
	conn, err := (&websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return sameOrigin(r, true) }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	h.mu.Lock()
	alive := !h.closed && h.pages[p.ID] != nil
	if alive {
		p.conn = conn
	}
	h.mu.Unlock()
	if !alive {
		return
	}
	conn.SetReadLimit(64 << 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m struct {
				Actions *[]Action       `json:"actions"`
				Label   string          `json:"label"`
				ID      string          `json:"id"`
				Result  json.RawMessage `json:"result"`
				Error   string          `json:"error"`
			}
			if conn.ReadJSON(&m) != nil {
				return
			}
			h.mu.Lock()
			h.cleanLocked()
			current := h.pages[p.ID]
			if current == nil {
				h.mu.Unlock()
				return
			}
			if m.Actions != nil {
				seen := map[string]bool{}
				valid := len(*m.Actions) <= 100 && len(m.Label) <= 160
				for _, a := range *m.Actions {
					if !h.allowed(a.Name) || len(a.Description) > 512 || seen[a.Name] {
						valid = false
					}
					seen[a.Name] = true
				}
				if !valid {
					h.mu.Unlock()
					return
				}
				current.Actions = append([]Action{}, (*m.Actions)...)
				current.Label = m.Label
				current.expires = time.Now().Add(h.options.PageTTL)
			}
			if m.ID != "" {
				c := h.commands[m.ID]
				if len(m.Result) > maxBytes || len(m.Error) > 2048 {
					h.mu.Unlock()
					return
				}
				if c != nil && c.principal == principal && c.PageID == p.ID && c.Status == "sent" && time.Now().Before(c.expires) {
					c.Result = m.Result
					c.Error = m.Error
					c.Status = "completed"
					if m.Error != "" {
						c.Status = "failed"
					}
				}
			}
			h.mu.Unlock()
		}
	}()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if conn.WriteJSON(map[string]string{"page_id": p.ID}) != nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// Authentication is rechecked by the mounting application on every reconnect.
	lifetime := time.NewTimer(5 * time.Minute)
	defer lifetime.Stop()
	for {
		select {
		case <-done:
			return
		case <-r.Context().Done():
			return
		case <-lifetime.C:
			return
		case <-tick.C:
			h.mu.Lock()
			h.cleanLocked()
			alive := h.pages[p.ID] != nil
			h.mu.Unlock()
			if !alive {
				return
			}
		case id := <-p.queue:
			h.mu.Lock()
			h.cleanLocked()
			c := h.commands[id]
			if c == nil || c.Status != "queued" {
				h.mu.Unlock()
				continue
			}
			c.Status = "sent"
			wire := c.Command
			h.mu.Unlock()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteJSON(map[string]any{"command": wire}) != nil {
				return
			}
		}
	}
}

// ClosePrincipal invalidates that principal's live pages, closes their sockets,
// and never retries a command whose delivery or outcome was uncertain.
// Applications should call it when an authenticated session is revoked.
func (h *Hub) ClosePrincipal(principal string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, p := range h.pages {
		if p.principal == principal {
			h.closeLocked(id)
		}
	}
}

// Close releases every live connection. The hub starts no background workers
// outside request lifetimes; it cannot be reused after Close.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for id := range h.pages {
		h.closeLocked(id)
	}
}
