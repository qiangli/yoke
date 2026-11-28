package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/coopauth"
)

// OfferRecord is the owner-only policy and immutable typed definition served
// by one outpost app. The secret is never returned by the definition route.
type OfferRecord struct {
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	Digest     string          `json:"digest"`
	Audience   struct {
		Circles []string `json:"circles,omitempty"`
		Groups  []string `json:"groups,omitempty"`
		Emails  []string `json:"emails,omitempty"`
	} `json:"audience"`
	Limit     uint64    `json:"limit,omitempty"`
	Expires   time.Time `json:"expires,omitempty"`
	TokenCap  int64     `json:"token_cap,omitempty"`
	SSOSecret string    `json:"sso_secret"`
}

type offerDefinition struct {
	Name  string `json:"name"`
	Class string `json:"class"`
	Unit  string `json:"unit"`
}

func (o OfferRecord) kind() (offerDefinition, error) {
	var d offerDefinition
	if err := json.Unmarshal(o.Definition, &d); err != nil {
		return d, err
	}
	if d.Name != o.Name || (d.Class != "model" && d.Class != "tool" && d.Class != "agent") {
		return d, errors.New("invalid offer definition")
	}
	return d, nil
}

// OffersDir resolves the records shared by the CLI and the door.
func OffersDir() (string, error) {
	state, err := door.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(state), "offers"), nil
}

func offerPath(dir, name string) (string, error) {
	if !ValidPeerName(name) || strings.HasSuffix(name, ".json") {
		return "", fmt.Errorf("invalid offer name %q", name)
	}
	return filepath.Join(dir, name+".json"), nil
}

// SaveOffer writes a record owner-only. Its audience is enforced by outpost.
func SaveOffer(dir string, o OfferRecord) error {
	path, err := offerPath(dir, o.Name)
	if err != nil {
		return err
	}
	if _, err := o.kind(); err != nil {
		return err
	}
	if o.SSOSecret == "" {
		return errors.New("offer has no SSO secret")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".offer-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadOffer reads a record by its route name.
func LoadOffer(dir, name string) (OfferRecord, error) {
	path, err := offerPath(dir, name)
	if err != nil {
		return OfferRecord{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return OfferRecord{}, err
	}
	var o OfferRecord
	if err := json.Unmarshal(data, &o); err != nil {
		return OfferRecord{}, err
	}
	if o.Name != name {
		return OfferRecord{}, errors.New("offer record name does not match route")
	}
	if _, err := o.kind(); err != nil {
		return OfferRecord{}, err
	}
	return o, nil
}

func (b *Broker) offersDir() (string, error) {
	if b.opts.OffersDir != "" {
		return b.opts.OffersDir, nil
	}
	return OffersDir()
}

func (b *Broker) admitOfferLimit(w http.ResponseWriter, ri *reqInfo, name string) bool {
	dir, err := b.offersDir()
	if err != nil {
		http.Error(w, "offer unavailable", 503)
		return false
	}
	rec, err := LoadOffer(dir, name)
	if err != nil {
		http.Error(w, "offer unavailable", 403)
		return false
	}
	d, err := rec.kind()
	if err != nil {
		http.Error(w, "invalid offer", 503)
		return false
	}
	ri.offerName, ri.offerUnit = name, d.Unit
	if rec.Limit == 0 || ri.principal == b.opts.Principal {
		return true
	}
	b.offerMu.Lock()
	used := b.offerCalls[ri.principal+"\x00"+name]
	b.offerMu.Unlock()
	if used >= rec.Limit {
		http.Error(w, "offer limit reached", 429)
		return false
	}
	return true
}

func offerRoute(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, "/offers/")
	return name, ok && ValidPeerName(name)
}

func peerOfferAllowed(method, path string, commands []string) bool {
	name, ok := offerRoute(path)
	return ok && (method == http.MethodGet || method == http.MethodPost) && slices.Contains(commands, name)
}

func peerFilesOfferAllowed(method, path string, commands []string) bool {
	tail, ok := strings.CutPrefix(path, "/files/")
	if !ok {
		return false
	}
	name, sub, hasSlash := strings.Cut(tail, "/")
	if !ValidPeerName(name) || !slices.Contains(commands, "files-"+name) {
		return false
	}
	if hasSlash {
		return sub != "" && (method == http.MethodGet || method == http.MethodHead)
	}
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodPost
}

func offerPrefixMatches(prefix, name string) bool {
	return strings.HasSuffix(strings.TrimSuffix(prefix, "/"), "/app/offer-"+name)
}

func (b *Broker) authorizeOfferMember(r *http.Request) (peerAuthorization, error) {
	prefix := strings.TrimSuffix(r.Header.Get(coopauth.HdrForwardedPrefix), "/")
	_, tail, ok := strings.Cut(prefix, "/app/offer-")
	if !ok || !ValidPeerName(tail) || !offerPrefixMatches(prefix, tail) {
		return peerAuthorization{}, errors.New("invalid offer app prefix")
	}
	dir, err := b.offersDir()
	if err != nil {
		return peerAuthorization{}, err
	}
	rec, err := LoadOffer(dir, tail)
	if err != nil {
		return peerAuthorization{}, errors.New("offer unavailable")
	}
	if !rec.Expires.IsZero() && !b.opts.Now().Before(rec.Expires) {
		return peerAuthorization{}, errors.New("offer expired")
	}
	if !coopauth.VerifyOutpost(r, []byte(rec.SSOSecret)) {
		return peerAuthorization{}, errors.New("invalid offer identity stamp")
	}
	rel, ok := coopauth.VerifyRelationship(r, []byte(rec.SSOSecret))
	if !ok || (rel.Kind != coopauth.RelMember && rel.Kind != coopauth.RelOwner) {
		return peerAuthorization{}, errors.New("offer requires a signed member relationship")
	}
	email := strings.ToLower(strings.TrimSpace(r.Header.Get(coopauth.HdrRemoteUser)))
	if _, err := SelfPeerName(email); err != nil {
		return peerAuthorization{}, errors.New("invalid member email")
	}
	return peerAuthorization{principal: "member:" + email, offerName: tail}, nil
}

func (b *Broker) memberOfferRouteAllowed(w http.ResponseWriter, r *http.Request, path, name string) bool {
	if strings.HasPrefix(name, "files-") && peerFilesOfferAllowed(r.Method, path, []string{name}) {
		return true
	}
	if offered, ok := offerRoute(path); ok && offered == name && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		return true
	}
	if r.Method == http.MethodPost && isInferencePath(path) {
		dir, err := b.offersDir()
		if err != nil {
			http.Error(w, "offer unavailable", 403)
			return false
		}
		rec, err := LoadOffer(dir, name)
		if err != nil {
			http.Error(w, "offer unavailable", 403)
			return false
		}
		d, err := rec.kind()
		if err != nil || d.Class != "model" {
			http.Error(w, "offer does not admit inference", 403)
			return false
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid inference body", 400)
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &request) != nil || request.Model != name || r.Header.Get("X-Bashy-Filter") != "" {
			http.Error(w, "inference model does not match offer", 403)
			return false
		}
		return true
	}
	http.Error(w, "member may reach only the matching offer", 403)
	return false
}

// OfferRunner is installed by pkg/offer, whose imports would otherwise cycle
// through broker. It receives the authenticated principal in its context.
type OfferRunner func(context.Context, OfferRecord, map[string]any) (json.RawMessage, any, error)

var offerRunner struct {
	sync.RWMutex
	run OfferRunner
}

func RegisterOfferRunner(run OfferRunner) {
	offerRunner.Lock()
	offerRunner.run = run
	offerRunner.Unlock()
}

func (b *Broker) serveOffer(w http.ResponseWriter, r *http.Request, ri *reqInfo, auth peerAuthorization) {
	name, ok := offerRoute(r.URL.Path)
	if !ok {
		http.Error(w, "invalid offer route", 404)
		return
	}
	dir, err := b.offersDir()
	if err != nil {
		http.Error(w, "offer store unavailable", 503)
		return
	}
	o, err := LoadOffer(dir, name)
	if err != nil {
		http.Error(w, "offer unavailable", 404)
		return
	}
	if !o.Expires.IsZero() && !b.opts.Now().Before(o.Expires) {
		http.Error(w, "offer expired", 403)
		return
	}
	if strings.HasPrefix(auth.principal, PeerPrincipalPrefix) && !slices.Contains(auth.commands, name) {
		http.Error(w, "offer not shared with peer", 403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Query().Has("definition") {
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"definition": json.RawMessage(o.Definition), "digest": o.Digest})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST or GET ?definition", 405)
		return
	}
	if !b.admitOfferLimit(w, ri, name) {
		return
	}
	var input struct {
		Args map[string]any `json:"args"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		http.Error(w, "invalid offer args", 400)
		return
	}
	if strings.HasPrefix(name, "files-") {
		body, err := json.Marshal(input.Args)
		if err != nil {
			http.Error(w, "invalid file args", 400)
			return
		}
		inner := r.Clone(r.Context())
		inner.URL.Path = "/files/" + strings.TrimPrefix(name, "files-")
		inner.Body = io.NopCloser(bytes.NewReader(body))
		capture := httptest.NewRecorder()
		b.serveFiles(capture, inner, ri)
		if capture.Code >= 300 {
			w.WriteHeader(capture.Code)
			_, _ = w.Write(capture.Body.Bytes())
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": json.RawMessage(capture.Body.Bytes()), "usage": map[string]int64{"bytes": ri.rec.Bytes}})
		return
	}
	offerRunner.RLock()
	run := offerRunner.run
	offerRunner.RUnlock()
	if run == nil {
		http.Error(w, "offer runner unavailable", 503)
		return
	}
	runPrincipal := ri.principal
	if runPrincipal == b.opts.Principal {
		runPrincipal = "owner"
	}
	result, usage, err := run(cligw.WithPrincipal(r.Context(), runPrincipal), o, input.Args)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if d, err := o.kind(); err == nil {
		if d.Unit == "bytes" {
			ri.rec.Bytes = int64(len(result))
		}
		if d.Unit == "token" {
			var u struct {
				Input  int `json:"input"`
				Output int `json:"output"`
			}
			data, _ := json.Marshal(usage)
			_ = json.Unmarshal(data, &u)
			ri.rec.PromptTok, ri.rec.OutputTok = u.Input, u.Output
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "usage": usage})
}
