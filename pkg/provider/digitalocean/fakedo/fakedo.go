// Package fakedo is an in-process fake of the DigitalOcean v2 API subset the
// provider adapter uses, for tests only (httptest; it never reaches the
// network). It records request counts and can inject the failures a
// provisioning controller must survive: async status transitions, 5xx,
// a response lost after the side effect happened, and foreign or untagged
// droplets.
//
// Served endpoints: GET /v2/account, GET /v2/regions, GET /v2/sizes,
// GET|POST /v2/droplets (tag_name, page, per_page), GET|DELETE
// /v2/droplets/{id}, POST /v2/droplets/{id}/actions, GET /v2/actions/{id}.
package fakedo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestToken is the only bearer token the fake accepts.
const TestToken = "fakedo-test-token"

// Failure is how an injected fault behaves.
type Failure int

const (
	// Fail5xx answers 503 BEFORE the side effect: nothing changed.
	Fail5xx Failure = iota
	// LoseResponse performs the side effect, then drops the connection
	// without a response (the client sees a transport error).
	LoseResponse
	// StallAfterSideEffect performs the side effect, then never answers
	// until the client gives up (a timeout after the side effect).
	StallAfterSideEffect
)

// Droplet is the fake's record of one droplet, in DO's JSON shape.
type Droplet struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"` // new, active, off, archive
	SizeSlug  string    `json:"size_slug"`
	Region    Slug      `json:"region"`
	Image     Slug      `json:"image"`
	Tags      []string  `json:"tags"`
	VolumeIDs []string  `json:"volume_ids"`
	Networks  Networks  `json:"networks"`
	CreatedAt time.Time `json:"created_at"`
	UserData  string    `json:"-"`

	polls int // GETs left before a "new" droplet becomes "active"
}

// Slug is DO's {"slug": ...} object.
type Slug struct {
	Slug string `json:"slug"`
}

// Networks is DO's droplet networks object.
type Networks struct {
	V4 []Network `json:"v4"`
}

// Network is one droplet address.
type Network struct {
	IPAddress string `json:"ip_address"`
	Type      string `json:"type"`
}

type action struct {
	ID        int    `json:"id"`
	Status    string `json:"status"` // in-progress, completed, errored
	Type      string `json:"type"`
	DropletID int    `json:"resource_id"`
	size      string
	polls     int
}

type fault struct {
	method, prefix string
	kind           Failure
	n              int
}

// Server is a running fake. URL is its base URL (use it as the adapter's
// BaseURL; the fake serves under /v2).
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	droplets map[int]*Droplet
	actions  map[int]*action
	nextID   int
	polls    int
	maxPer   int
	faults   []*fault
	creates  int
	deletes  int
	requests int
	stall    chan struct{}
	regions  []map[string]any
	sizes    []map[string]any
	account  map[string]any
}

// SetCatalog replaces the served /v2/regions and /v2/sizes entries.
func (s *Server) SetCatalog(regions, sizes []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regions, s.sizes = regions, sizes
}

// SetAccount replaces the served /v2/account object.
func (s *Server) SetAccount(account map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = account
}

// New starts a fake and closes it when the test ends.
func New(t testing.TB) *Server {
	s := &Server{
		droplets: map[int]*Droplet{},
		actions:  map[int]*action{},
		nextID:   1000,
		stall:    make(chan struct{}),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		close(s.stall)
		s.Server.Close()
	})
	return s
}

// SetAsyncPolls makes a new droplet report "new" for n GETs before
// turning "active", and every action report "in-progress" for n GETs
// before completing. The default, 0, is synchronous.
func (s *Server) SetAsyncPolls(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls = n
}

// SetMaxPerPage caps list page size regardless of per_page, so clients
// must follow links.pages.next.
func (s *Server) SetMaxPerPage(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxPer = n
}

// Inject arms a fault for the next n requests whose method matches and
// whose path starts with pathPrefix (e.g. "POST", "/v2/droplets").
func (s *Server) Inject(method, pathPrefix string, kind Failure, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{method: method, prefix: pathPrefix, kind: kind, n: n})
}

// AddDroplet seeds a droplet the adapter did not create (foreign or
// untagged, depending on tags) and returns its ID.
func (s *Server) AddDroplet(name string, tags ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(name, "s-1vcpu-1gb", "r1", "ubuntu-24-04-x64", "active", tags, "").ID
}

// AddGPUDroplet seeds a GPU droplet, which the real unfiltered list omits.
func (s *Server) AddGPUDroplet(name string, tags ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(name, "gpu-h100x1-80gb", "r1", "ubuntu-24-04-x64", "active", tags, "").ID
}

// AttachVolume records a block-storage volume on a droplet; DO keeps (and
// bills) volumes after the droplet is deleted.
func (s *Server) AttachVolume(id int, volumeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.droplets[id]; d != nil {
		d.VolumeIDs = append(d.VolumeIDs, volumeID)
	}
}

// Droplet returns a copy of a droplet, or false when it does not exist.
func (s *Server) Droplet(id int) (Droplet, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.droplets[id]
	if !ok {
		return Droplet{}, false
	}
	c := *d
	c.Tags = slices.Clone(d.Tags)
	return c, true
}

// CreateCount is the number of droplet creates that took effect.
func (s *Server) CreateCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.creates }

// DeleteCount is the number of droplet deletes that took effect.
func (s *Server) DeleteCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.deletes }

// RequestCount is the number of authenticated API requests received.
func (s *Server) RequestCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }

func (s *Server) addLocked(name, size, region, image, status string, tags []string, userData string) *Droplet {
	s.nextID++
	id := s.nextID
	d := &Droplet{
		ID: id, Name: name, Status: status, SizeSlug: size,
		Region: Slug{region}, Image: Slug{image}, Tags: slices.Clone(tags),
		VolumeIDs: []string{},
		Networks: Networks{V4: []Network{
			{IPAddress: fmt.Sprintf("192.0.2.%d", id%250+1), Type: "public"},
		}},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Second),
		UserData:  userData,
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	if status == "new" {
		d.polls = s.polls
		if d.polls == 0 {
			d.Status = "active"
		}
	}
	s.droplets[id] = d
	return d
}

// takeFault returns the armed fault matching r, consuming one use.
func (s *Server) takeFault(r *http.Request) (Failure, bool) {
	for _, f := range s.faults {
		if f.n > 0 && f.method == r.Method && strings.HasPrefix(r.URL.Path, f.prefix) {
			f.n--
			return f.kind, true
		}
	}
	return 0, false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+TestToken {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Unable to authenticate you")
		return
	}
	s.mu.Lock()
	s.requests++
	kind, faulted := s.takeFault(r)
	if faulted && kind == Fail5xx {
		s.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "service_unavailable", "injected 503")
		return
	}
	status, body := s.route(r)
	s.mu.Unlock()

	if faulted {
		switch kind {
		case LoseResponse:
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
		case StallAfterSideEffect:
			select {
			case <-r.Context().Done():
			case <-s.stall:
			}
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

type apiError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, id, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{ID: id, Message: msg})
}

func notFound() (int, any) {
	return http.StatusNotFound, apiError{ID: "not_found", Message: "The resource you were accessing could not be found."}
}

// route performs the request under s.mu and returns the response.
func (s *Server) route(r *http.Request) (int, any) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v2" {
		return notFound()
	}
	switch {
	case len(parts) == 2 && parts[1] == "account" && r.Method == http.MethodGet:
		if s.account != nil {
			return http.StatusOK, map[string]any{"account": s.account}
		}
		return http.StatusOK, map[string]any{"account": map[string]any{
			"uuid": "acct-0001", "status": "active", "droplet_limit": 25,
		}}
	case len(parts) == 2 && parts[1] == "regions" && r.Method == http.MethodGet && s.regions != nil:
		return http.StatusOK, map[string]any{"regions": s.regions}
	case len(parts) == 2 && parts[1] == "sizes" && r.Method == http.MethodGet && s.sizes != nil:
		return http.StatusOK, map[string]any{"sizes": s.sizes}
	case len(parts) == 2 && parts[1] == "regions" && r.Method == http.MethodGet:
		return http.StatusOK, map[string]any{"regions": []map[string]any{
			{"slug": "r1", "name": "Region One", "available": true, "sizes": []string{"s-1vcpu-1gb", "s-2vcpu-4gb"}},
			{"slug": "r2", "name": "Region Two", "available": false, "sizes": []string{}},
		}}
	case len(parts) == 2 && parts[1] == "sizes" && r.Method == http.MethodGet:
		return http.StatusOK, map[string]any{"sizes": []map[string]any{
			{"slug": "s-1vcpu-1gb", "vcpus": 1, "memory": 1024, "disk": 25, "price_monthly": 6.0, "price_hourly": 0.00893, "available": true, "regions": []string{"r1"}},
			{"slug": "s-2vcpu-4gb", "vcpus": 2, "memory": 4096, "disk": 80, "price_monthly": 24.0, "price_hourly": 0.03571, "available": true, "regions": []string{"r1"}},
		}}
	case len(parts) == 2 && parts[1] == "droplets":
		switch r.Method {
		case http.MethodGet:
			return s.listDroplets(r)
		case http.MethodPost:
			return s.createDroplet(r)
		}
	case len(parts) == 3 && parts[1] == "droplets":
		id, err := strconv.Atoi(parts[2])
		d := s.droplets[id]
		if err != nil || d == nil {
			return notFound()
		}
		switch r.Method {
		case http.MethodGet:
			s.tick(d)
			return http.StatusOK, map[string]any{"droplet": d}
		case http.MethodDelete:
			delete(s.droplets, id)
			s.deletes++
			return http.StatusNoContent, nil
		}
	case len(parts) == 4 && parts[1] == "droplets" && parts[3] == "actions" && r.Method == http.MethodPost:
		id, err := strconv.Atoi(parts[2])
		d := s.droplets[id]
		if err != nil || d == nil {
			return notFound()
		}
		return s.dropletAction(r, d)
	case len(parts) == 3 && parts[1] == "actions" && r.Method == http.MethodGet:
		id, _ := strconv.Atoi(parts[2])
		a := s.actions[id]
		if a == nil {
			return notFound()
		}
		s.tickAction(a)
		return http.StatusOK, map[string]any{"action": a}
	}
	return notFound()
}

func (s *Server) tick(d *Droplet) {
	if d.Status == "new" {
		if d.polls--; d.polls <= 0 {
			d.Status = "active"
		}
	}
}

func (s *Server) tickAction(a *action) {
	if a.Status != "in-progress" {
		return
	}
	if a.polls--; a.polls > 0 {
		return
	}
	a.Status = "completed"
	if d := s.droplets[a.DropletID]; d != nil {
		applyAction(d, a)
	}
}

func applyAction(d *Droplet, a *action) {
	switch a.Type {
	case "power_off", "shutdown":
		d.Status = "off"
	case "power_on", "reboot":
		d.Status = "active"
	case "resize":
		d.SizeSlug = a.size
	}
}

func (s *Server) listDroplets(r *http.Request) (int, any) {
	q := r.URL.Query()
	tag := q.Get("tag_name")
	gpuOnly := q.Get("type") == "gpus"
	var all []*Droplet
	for _, d := range s.droplets {
		if (tag == "" || slices.Contains(d.Tags, tag)) && (strings.HasPrefix(d.SizeSlug, "gpu-") == gpuOnly) {
			s.tick(d)
			all = append(all, d)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per < 1 {
		per = 20
	}
	if s.maxPer > 0 && per > s.maxPer {
		per = s.maxPer
	}
	lo := min((page-1)*per, len(all))
	hi := min(lo+per, len(all))
	links := map[string]any{}
	if hi < len(all) {
		nq := r.URL.Query()
		nq.Set("page", strconv.Itoa(page+1))
		nq.Set("per_page", strconv.Itoa(per))
		links["pages"] = map[string]string{"next": s.URL + r.URL.Path + "?" + nq.Encode()}
	}
	out := all[lo:hi]
	if out == nil {
		out = []*Droplet{}
	}
	return http.StatusOK, map[string]any{
		"droplets": out,
		"links":    links,
		"meta":     map[string]int{"total": len(all)},
	}
}

func (s *Server) createDroplet(r *http.Request) (int, any) {
	var in struct {
		Name     string   `json:"name"`
		Region   string   `json:"region"`
		Size     string   `json:"size"`
		Image    any      `json:"image"`
		Tags     []string `json:"tags"`
		UserData string   `json:"user_data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return http.StatusUnprocessableEntity, apiError{ID: "unprocessable_entity", Message: err.Error()}
	}
	if in.Name == "" || in.Region == "" || in.Size == "" || in.Image == nil {
		return http.StatusUnprocessableEntity, apiError{ID: "unprocessable_entity", Message: "name, region, size and image are required"}
	}
	image := fmt.Sprint(in.Image)
	d := s.addLocked(in.Name, in.Size, in.Region, image, "new", in.Tags, in.UserData)
	s.creates++
	return http.StatusAccepted, map[string]any{"droplet": d}
}

func (s *Server) dropletAction(r *http.Request, d *Droplet) (int, any) {
	var in struct {
		Type string `json:"type"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return http.StatusUnprocessableEntity, apiError{ID: "unprocessable_entity", Message: err.Error()}
	}
	switch in.Type {
	case "power_on", "power_off", "shutdown", "reboot":
	case "resize":
		if in.Size == "" {
			return http.StatusUnprocessableEntity, apiError{ID: "unprocessable_entity", Message: "resize needs size"}
		}
	default:
		return http.StatusUnprocessableEntity, apiError{ID: "unprocessable_entity", Message: "unknown action type " + in.Type}
	}
	s.nextID++
	a := &action{ID: s.nextID, Status: "in-progress", Type: in.Type, DropletID: d.ID, size: in.Size, polls: s.polls}
	s.actions[a.ID] = a
	if a.polls == 0 {
		a.Status = "completed"
		applyAction(d, a)
	}
	return http.StatusCreated, map[string]any{"action": a}
}
