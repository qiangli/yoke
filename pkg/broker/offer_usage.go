package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/broker/door"
)

const OfferUsageHeader = "X-Bashy-Usage"

// OfferUsage is the price-free measurement shared by the door and caller.
type OfferUsage struct {
	Tool   string  `json:"tool"`
	Class  string  `json:"class"`
	Unit   string  `json:"unit"`
	Units  float64 `json:"units"`
	Digest string  `json:"digest"`
}

func (b *Broker) admitUsage(w http.ResponseWriter, ri *reqInfo, o OfferRecord, d offerDefinition) bool {
	key := ri.principal + "\x00" + o.Name
	if o.Limit > 0 && ri.principal != b.opts.Principal {
		b.offerMu.Lock()
		defer b.offerMu.Unlock()
		if b.offerCalls[key] >= float64(o.Limit) {
			http.Error(w, "offer limit reached", http.StatusTooManyRequests)
			return false
		}
		// A call has a known size at admission. Reserve it across concurrent runs.
		if d.Unit == "call" {
			b.offerCalls[key]++
			ri.reservedCall = true
		}
	}
	ri.offerName = o.Name
	ri.offerUsage = OfferUsage{Tool: o.Name, Class: d.Class, Unit: d.Unit, Digest: o.Digest}
	return true
}

func (ri *reqInfo) measureResult(result json.RawMessage, usage any) error {
	switch ri.offerUsage.Unit {
	case "token":
		var tokens CallUsage
		raw, err := json.Marshal(usage)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &tokens); err != nil {
			return err
		}
		if tokens.Input < 0 || tokens.Output < 0 {
			return fmt.Errorf("negative token usage")
		}
		ri.rec.PromptTok, ri.rec.OutputTok = int(tokens.Input), int(tokens.Output)
	case "bytes":
		ri.rec.Bytes = int64(len(result))
	case "seconds":
		var output struct {
			Seconds *float64 `json:"seconds"`
			Text    string   `json:"text"`
		}
		if err := json.Unmarshal(result, &output); err != nil {
			return err
		}
		// A toolcmd.Result carries the command's JSON output in Text.
		if output.Seconds == nil && output.Text != "" {
			if err := json.Unmarshal([]byte(output.Text), &output); err != nil {
				return fmt.Errorf("invalid seconds result: %w", err)
			}
		}
		if output.Seconds == nil || *output.Seconds < 0 {
			return fmt.Errorf("offer result needs nonnegative numeric seconds")
		}
		ri.offerUsage.Units = *output.Seconds
	}
	ri.measureUnits()
	return nil
}

func (ri *reqInfo) measureUnits() {
	switch ri.offerUsage.Unit {
	case "token":
		ri.offerUsage.Units = float64(ri.rec.PromptTok) + float64(ri.rec.OutputTok)
	case "bytes":
		ri.offerUsage.Units = float64(ri.rec.Bytes)
	case "call":
		ri.offerUsage.Units = 1
	}
}

func (b *Broker) finishOfferUsage(ri *reqInfo, status int) {
	if ri.offerUsage.Tool == "" {
		return
	}
	delta := 0.0
	if status >= 200 && status < 300 || status == http.StatusNotModified && ri.offerUsage.Unit == "bytes" {
		ri.measureUnits()
		u := ri.offerUsage
		ri.rec.Tool, ri.rec.OfferClass, ri.rec.Unit, ri.rec.Units, ri.rec.Digest = u.Tool, u.Class, u.Unit, u.Units, u.Digest
		delta = u.Units
	}
	if ri.reservedCall {
		delta--
	}
	if delta != 0 && ri.principal != b.opts.Principal {
		b.offerMu.Lock()
		b.offerCalls[ri.principal+"\x00"+ri.offerName] += delta
		b.offerMu.Unlock()
	}
}

func (ri *reqInfo) writeUsageHeader(h http.Header) {
	if ri.rec.Tool != "" {
		ri.offerUsage.setHeader(h)
	}
}

func (u OfferUsage) setHeader(h http.Header) {
	raw, _ := json.Marshal(u)
	h.Set(OfferUsageHeader, string(raw))
}

// Some backends report usage directly. Pass-through backends leave it in the response tail.
func (b *Broker) finishCaptured(ri *reqInfo, cw *captureWriter) {
	if ri.finished {
		return
	}
	if cw.status == 0 {
		cw.status = http.StatusOK
	}
	if ri.rec.PromptTok == 0 && ri.rec.OutputTok == 0 {
		ri.rec.PromptTok, ri.rec.OutputTok, ri.rec.CachedTok, ri.rec.CacheWriteTok = parseUsage(cw.tail)
	}
	b.finish(ri, cw.status, 0, refusalReason(cw.status, cw.tail))
}

// An inference request selects a model offer through its app or a named peer grant.
func (b *Broker) admitInferenceOffer(w http.ResponseWriter, ri *reqInfo, commands []string, prefix string, body []byte) bool {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil || !ValidPeerName(req.Model) {
		return true
	}
	ownerApp := ri.principal == b.opts.Principal && offerPrefixMatches(prefix, req.Model)
	if !ownerApp && !slices.Contains(commands, req.Model) {
		return true
	}
	dir, err := b.offersDir()
	if err != nil {
		http.Error(w, "offer store unavailable", 503)
		return false
	}
	o, err := LoadOffer(dir, req.Model)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		http.Error(w, "offer unavailable", 503)
		return false
	}
	d, _ := o.kind()
	if d.Class != "model" {
		return true
	}
	if !o.Expires.IsZero() && !b.opts.Now().Before(o.Expires) {
		http.Error(w, "offer expired", 403)
		return false
	}
	if !b.admitUsage(w, ri, o, d) {
		return false
	}
	ri.offerUsage.Tool = "llm"
	return true
}

func (b *Broker) serveMeteredInference(w http.ResponseWriter, r *http.Request, ri *reqInfo, body []byte) {
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	if request.Stream {
		w.Header().Add("Trailer", OfferUsageHeader)
		cw := newCaptureWriter(usageTrailerWriter{w})
		b.dispatch(cw, r, ri, body, false)
		b.finishCaptured(ri, cw)
		ri.writeUsageHeader(w.Header())
		return
	}
	out := httptest.NewRecorder()
	cw := newCaptureWriter(out)
	b.dispatch(cw, r, ri, body, false)
	b.finishCaptured(ri, cw)
	for k, v := range out.Header() {
		w.Header()[k] = v
	}
	ri.writeUsageHeader(w.Header())
	w.WriteHeader(out.Code)
	_, _ = w.Write(out.Body.Bytes())
}

// ServeContent and proxy responses may supply Content-Length. Trailers on
// HTTP/1.1 need chunked framing so measured bytes/tokens can follow the body.
type usageTrailerWriter struct{ http.ResponseWriter }

func (w usageTrailerWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}
func (w usageTrailerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w usageTrailerWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

// Inline file calls finish before writing their JSON. Streams report actual
// bytes written in a trailer. HEAD and 304 have no body and serve zero bytes.
type fileUsageWriter struct {
	http.ResponseWriter
	ri   *reqInfo
	head bool
}

func (w fileUsageWriter) WriteHeader(status int) {
	if status/100 == 2 || status == http.StatusNotModified {
		if w.head || status == http.StatusNotModified || w.ri.finished {
			w.ri.offerUsage.setHeader(w.Header())
		} else {
			w.Header().Add("Trailer", OfferUsageHeader)
			w.Header().Del("Content-Length")
		}
	}
	w.ResponseWriter.WriteHeader(status)
}
func (b *Broker) admitFileUsage(w http.ResponseWriter, ri *reqInfo, name string) bool {
	dir, err := b.offersDir()
	if err != nil {
		http.Error(w, "offer store unavailable", 503)
		return false
	}
	rec, err := LoadOffer(dir, "files-"+name)
	if errors.Is(err, os.ErrNotExist) {
		def, err := FileShareDefinition(name)
		if err != nil {
			http.Error(w, "invalid files definition", 500)
			return false
		}
		ri.offerUsage = OfferUsage{Tool: def.Name, Class: def.Class, Unit: def.Unit, Digest: def.Digest}
		ri.offerName = def.Name
		return true
	}
	if err != nil {
		http.Error(w, "files offer unavailable", 503)
		return false
	}
	d, _ := rec.kind()
	return b.admitUsage(w, ri, rec, d)
}

// RememberOfferDefinition keeps the public definition addressable after an
// offer is stopped or replaced. No policy or credential enters this store.
func RememberOfferDefinition(dir string, definition json.RawMessage) error {
	digest, err := mcp.CanonicalSHA256(definition)
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "definitions")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, digest+".json")
	if _, err = os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".definition-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(definition); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func RemoteUsagePath() (string, error) {
	dir, err := door.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "remote-usage.jsonl"), nil
}

func AppendRemoteUsage(host string, u OfferUsage) error {
	if !ValidPeerName(host) || u.Tool == "" || u.Digest == "" || u.Units < 0 {
		return fmt.Errorf("invalid remote usage")
	}
	path, err := RemoteUsagePath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	row := struct {
		Time time.Time `json:"time"`
		Host string    `json:"host"`
		OfferUsage
	}{time.Now().UTC(), host, u}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(f).Encode(row); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RecordRemoteResponse runs after the response body reaches EOF, so streaming
// trailers are available as well as ordinary response headers.
func RecordRemoteResponse(host string, resp *http.Response) error {
	raw := resp.Header.Get(OfferUsageHeader)
	if raw == "" {
		raw = resp.Trailer.Get(OfferUsageHeader)
	}
	if raw == "" {
		return nil
	}
	var u OfferUsage
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return err
	}
	return AppendRemoteUsage(host, u)
}
