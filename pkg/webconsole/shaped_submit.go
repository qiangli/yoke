package webconsole

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	core "github.com/qiangli/yoke/pkg/shape"
	"github.com/qiangli/yoke/pkg/todo"
)

// handleShapeSubmit submits an already approved version for ordinary review.
// Identity and filesystem locations come from the authenticated server context,
// never the request body. Sharing remains the separate offer/adopt operation.
func (s *server) handleShapeSubmit(w http.ResponseWriter, r *http.Request) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	var in struct {
		Rev   string `json:"rev"`
		Story string `json:"story,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil || strings.TrimSpace(in.Rev) == "" {
		http.Error(w, "send {\"rev\":\"approved revision\"} with optional story", http.StatusBadRequest)
		return
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		http.Error(w, "send one submission object", http.StatusBadRequest)
		return
	}
	if in.Story != "" && !s.operatorSession(r) {
		http.Error(w, "linking a story requires an operator session", http.StatusForbidden)
		return
	}
	if _, e := a.srcRepo(); e != nil {
		http.Error(w, "submission unavailable: "+e.Error(), http.StatusServiceUnavailable)
		return
	}
	opts := core.SubmitOptions{Story: in.Story}
	if in.Story != "" {
		store, _, e := todo.ResolveStore(principal, false, false, "")
		if e != nil {
			http.Error(w, "submission tracking unavailable: "+e.Error(), http.StatusServiceUnavailable)
			return
		}
		opts.TrackingRoot = store.Dir()
	}
	result, e := a.candidateService().Submit(r.Context(), a.caller(principal), strings.TrimSpace(in.Rev), opts)
	if e != nil {
		http.Error(w, "submission refused: "+e.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
