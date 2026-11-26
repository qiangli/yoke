// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
)

const (
	serviceJSONFile  = "service.json"
	serviceTokenFile = "service.token"
)

// ServiceEndpoint is persisted at <root>/service.json so local callers discover
// the active single-writer loopback service.
type ServiceEndpoint struct {
	Addr      string    `json:"addr"`
	PID       int       `json:"pid"`
	StartID   string    `json:"start_id,omitempty"`
	Epoch     uint64    `json:"epoch"`
	StartedAt time.Time `json:"started_at"`
}

// ServerConfig configures the single-writer loopback HTTP service.
type ServerConfig struct {
	Root                 string
	Addr                 string // default "127.0.0.1:0"
	AllowMeshBind        bool   // required to bind non-loopback addresses
	Store                *Store // optional pre-opened Store; opened at Root when nil
	StoreOptions         StoreOptions
	CloseStoreOnShutdown bool
}

// Server exposes a *Store over authenticated loopback HTTP.
type Server struct {
	root       string
	addr       string
	token      string
	store      *Store
	closeStore bool
	ln         net.Listener
	httpSrv    *http.Server

	closeOnce sync.Once
	closeErr  error
	serveDone chan struct{}
}

func validateBindAddr(addr string, allowMesh bool) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("artifact: invalid bind address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
		addr = net.JoinHostPort(host, port)
	}
	if allowMesh {
		return addr, nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%w: %q", ErrNonLoopbackBind, addr)
	}
	return addr, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp_*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Chmod(path, perm)
}

// StartServer starts the loopback single-writer service and writes <root>/service.token
// (0600) and <root>/service.json.
func StartServer(ctx context.Context, cfg ServerConfig) (*Server, error) {
	bindAddr, err := validateBindAddr(cfg.Addr, cfg.AllowMeshBind)
	if err != nil {
		return nil, err
	}

	store := cfg.Store
	closeStore := cfg.CloseStoreOnShutdown
	if store == nil {
		sopts := cfg.StoreOptions
		if sopts.Root == "" {
			sopts.Root = cfg.Root
		}
		store, err = OpenStore(ctx, sopts)
		if err != nil {
			return nil, err
		}
		closeStore = true
	}

	root := store.Root()
	token, err := randomHex(32)
	if err != nil {
		if closeStore {
			_ = store.Close()
		}
		return nil, err
	}

	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		if closeStore {
			_ = store.Close()
		}
		return nil, err
	}

	s := &Server{
		root:       root,
		addr:       ln.Addr().String(),
		token:      token,
		store:      store,
		closeStore: closeStore,
		ln:         ln,
		serveDone:  make(chan struct{}),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.httpSrv = &http.Server{
		Handler:           s.authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	tokenPath := filepath.Join(root, serviceTokenFile)
	if err := writeFileAtomic(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		_ = ln.Close()
		if closeStore {
			_ = store.Close()
		}
		return nil, err
	}

	pid, startID := CurrentProcessIdentity(ctx)
	ep := ServiceEndpoint{
		Addr:      s.addr,
		PID:       pid,
		StartID:   startID,
		Epoch:     store.ServiceEpoch(),
		StartedAt: store.now(),
	}
	epRaw, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		_ = ln.Close()
		if closeStore {
			_ = store.Close()
		}
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(root, serviceJSONFile), epRaw, 0o600); err != nil {
		_ = ln.Close()
		if closeStore {
			_ = store.Close()
		}
		return nil, err
	}

	go func() {
		defer close(s.serveDone)
		_ = s.httpSrv.Serve(ln)
	}()

	return s, nil
}

func (s *Server) Addr() string       { return s.addr }
func (s *Server) Token() string      { return s.token }
func (s *Server) Epoch() uint64      { return s.store.ServiceEpoch() }
func (s *Server) Store() *Store      { return s.store }
func (s *Server) NewClient() *Client { return NewClient(s.addr, s.token) }

func (s *Server) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.removeDiscoveryFilesIfOwned()
		err := s.httpSrv.Shutdown(ctx)
		<-s.serveDone
		if s.closeStore && s.store != nil {
			err = errors.Join(err, s.store.Close())
		}
		s.closeErr = err
	})
	return s.closeErr
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

func (s *Server) removeDiscoveryFilesIfOwned() {
	epPath := filepath.Join(s.root, serviceJSONFile)
	raw, err := os.ReadFile(epPath)
	if err != nil {
		return
	}
	var ep ServiceEndpoint
	if json.Unmarshal(raw, &ep) == nil && ep.Epoch == s.store.ServiceEpoch() {
		_ = os.Remove(epPath)
		_ = os.Remove(filepath.Join(s.root, serviceTokenFile))
	}
}

type apiError struct {
	Code    string `json:"error_code"`
	Message string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
		code = "not_found"
	case errors.Is(err, ErrPayloadEvicted):
		status = http.StatusNotFound
		code = "payload_evicted"
	case errors.Is(err, ErrImmutablePublicationConflict):
		status = http.StatusConflict
		code = "immutable_publication_conflict"
	case errors.Is(err, ErrCorruptPayload):
		status = http.StatusBadGateway
		code = "corrupt_payload"
	case errors.Is(err, ErrProtectedArtifact):
		status = http.StatusConflict
		code = "protected_artifact"
	case errors.Is(err, ErrStoreBusy):
		status = http.StatusServiceUnavailable
		code = "store_busy"
	default:
		msg := err.Error()
		if strings.HasPrefix(msg, "artifact: ") {
			status = http.StatusBadRequest
			code = "bad_request"
		}
	}
	writeJSON(w, status, apiError{Code: code, Message: err.Error()})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	expected := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
			writeJSON(w, http.StatusUnauthorized, apiError{
				Code:    "unauthorized",
				Message: "missing or invalid bearer token",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type leaseRequest struct {
	LeaseID       string   `json:"lease_id,omitempty"`
	HolderPID     int      `json:"holder_pid"`
	HolderStartID string   `json:"holder_start_id,omitempty"`
	TTLMs         int64    `json:"ttl_ms"`
	Digests       []string `json:"digests,omitempty"`
}

type pruneRequest struct {
	Apply   bool         `json:"apply"`
	OlderNs int64        `json:"older_ns,omitempty"`
	Items   []prune.Item `json:"items,omitempty"`
}

type pruneResponse struct {
	Applied bool         `json:"applied"`
	Items   []prune.Item `json:"items"`
	Counts  prune.Counts `json:"counts"`
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		dgst := r.PathValue("digest")
		mediaType := r.Header.Get("Content-Type")
		size := r.ContentLength
		n, err := s.store.PutBlob(r.Context(), dgst, size, mediaType, r.Body)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"digest": dgst, "size": n})
	})

	mux.HandleFunc("GET /v1/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		dgst := r.PathValue("digest")
		rc, size, mediaType, err := s.store.openBlobWithMeta(r.Context(), dgst)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("X-Content-Digest", dgst)
		if size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
	})

	mux.HandleFunc("POST /v1/records", func(w http.ResponseWriter, r *http.Request) {
		var rec CatalogRecordV1
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_json", Message: err.Error()})
			return
		}
		st, err := s.store.PutRecord(r.Context(), rec)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, st)
	})

	mux.HandleFunc("GET /v1/records/{digest}", func(w http.ResponseWriter, r *http.Request) {
		dgst := r.PathValue("digest")
		st, err := s.store.GetRecordStatus(r.Context(), dgst)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("GET /v1/records", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		ver := r.URL.Query().Get("version")
		if name == "" || ver == "" {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_request", Message: "name and version query params required"})
			return
		}
		st, err := s.store.LookupLogical(r.Context(), name, ver)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("POST /v1/publications", func(w http.ResponseWriter, r *http.Request) {
		var pub PublicationRecord
		if err := json.NewDecoder(r.Body).Decode(&pub); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_json", Message: err.Error()})
			return
		}
		idem, err := s.store.PublishModule(r.Context(), pub)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		status := http.StatusCreated
		if idem {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"idempotent": idem, "publication": pub})
	})

	mux.HandleFunc("GET /v1/publications", func(w http.ResponseWriter, r *http.Request) {
		modPath := r.URL.Query().Get("module")
		ver := r.URL.Query().Get("version")
		if modPath != "" || ver != "" {
			pub, err := s.store.GetPublication(r.Context(), modPath, ver)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, pub)
			return
		}
		pubs, err := s.store.ListPublications(r.Context())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if pubs == nil {
			pubs = []PublicationRecord{}
		}
		writeJSON(w, http.StatusOK, pubs)
	})

	mux.HandleFunc("POST /v1/manifests", func(w http.ResponseWriter, r *http.Request) {
		var m ManifestV1
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_json", Message: err.Error()})
			return
		}
		if err := s.store.PutManifest(r.Context(), m); err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, m)
	})

	mux.HandleFunc("GET /v1/manifests", func(w http.ResponseWriter, r *http.Request) {
		genStr := r.URL.Query().Get("generation")
		if genStr != "" {
			if genStr == "latest" {
				m, err := s.store.LatestManifest(r.Context())
				if err != nil {
					writeAPIError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, m)
				return
			}
			gen, err := strconv.ParseUint(genStr, 10, 64)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_request", Message: "invalid generation"})
				return
			}
			m, err := s.store.GetManifest(r.Context(), gen)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, m)
			return
		}
		manifests, err := s.store.ListManifests(r.Context())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if manifests == nil {
			manifests = []ManifestV1{}
		}
		writeJSON(w, http.StatusOK, manifests)
	})

	mux.HandleFunc("POST /v1/leases", func(w http.ResponseWriter, r *http.Request) {
		var req leaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_json", Message: err.Error()})
			return
		}
		ttl := time.Duration(req.TTLMs) * time.Millisecond
		if req.LeaseID != "" {
			l, err := s.store.RenewLease(r.Context(), req.LeaseID, ttl)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, l)
			return
		}
		l, err := s.store.AcquireLease(r.Context(), req.HolderPID, req.HolderStartID, ttl, req.Digests)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, l)
	})

	mux.HandleFunc("GET /v1/leases", func(w http.ResponseWriter, r *http.Request) {
		leases, err := s.store.ListActiveLeases(r.Context())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if leases == nil {
			leases = []Lease{}
		}
		writeJSON(w, http.StatusOK, leases)
	})

	mux.HandleFunc("DELETE /v1/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.ReleaseLease(r.Context(), r.PathValue("id")); err != nil {
			writeAPIError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/prune", func(w http.ResponseWriter, r *http.Request) {
		var req pruneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Code: "bad_json", Message: err.Error()})
			return
		}
		if !req.Apply {
			items, err := s.store.DryRunPrune(r.Context(), time.Duration(req.OlderNs))
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, pruneResponse{Applied: false, Items: items})
			return
		}
		items := req.Items
		if items == nil {
			var err error
			items, err = s.store.DryRunPrune(r.Context(), time.Duration(req.OlderNs))
			if err != nil {
				writeAPIError(w, err)
				return
			}
		}
		counts, err := s.store.ApplyPrune(r.Context(), items)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pruneResponse{Applied: true, Items: items, Counts: counts})
	})

	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.store.Status(r.Context())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}

// Client speaks HTTP JSON + binary blob streaming to a running artifact Server.
type Client struct {
	baseURL string
	token   string
	httpc   *http.Client
}

var _ Backend = (*Client)(nil)

// ErrUnauthorized indicates missing or invalid bearer authentication on the loopback service.
var ErrUnauthorized = errors.New("artifact: unauthorized bearer token")

// NewClient constructs a Client targeting addr (host:port or http://host:port) with bearer token.
func NewClient(addr, token string) *Client {
	base := strings.TrimSpace(addr)
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	base = strings.TrimRight(base, "/")
	return &Client{
		baseURL: base,
		token:   strings.TrimSpace(token),
		httpc:   &http.Client{Transport: &http.Transport{DisableCompression: true}},
	}
}

func (c *Client) Close() error {
	c.httpc.CloseIdleConnections()
	return nil
}

func (c *Client) doRequest(ctx context.Context, method, relPath string, contentType string, contentLen int64, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+relPath, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentLen >= 0 {
		req.ContentLength = contentLen
	}
	return c.httpc.Do(req)
}

func decodeHTTPError(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var ae apiError
	_ = json.Unmarshal(raw, &ae)
	msg := ae.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
	}
	switch ae.Code {
	case "not_found":
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case "payload_evicted":
		return fmt.Errorf("%w: %s", ErrPayloadEvicted, msg)
	case "immutable_publication_conflict":
		return fmt.Errorf("%w: %s", ErrImmutablePublicationConflict, msg)
	case "corrupt_payload":
		return fmt.Errorf("%w: %s", ErrCorruptPayload, msg)
	case "protected_artifact":
		return fmt.Errorf("%w: %s", ErrProtectedArtifact, msg)
	case "store_busy":
		return fmt.Errorf("%w: %s", ErrStoreBusy, msg)
	default:
		return fmt.Errorf("artifact: http %d: %s", resp.StatusCode, msg)
	}
}

func (c *Client) doJSON(ctx context.Context, method, relPath string, in any, out any) (int, error) {
	var body io.Reader
	var contentLen int64 = -1
	contentType := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
		contentLen = int64(len(raw))
		contentType = "application/json"
	}
	resp, err := c.doRequest(ctx, method, relPath, contentType, contentLen, body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, decodeHTTPError(resp)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) PutBlob(ctx context.Context, dgst string, size int64, mediaType string, r io.Reader) (int64, error) {
	if mediaType == "" {
		mediaType = defaultMediaType
	}
	resp, err := c.doRequest(ctx, http.MethodPut, "/v1/blobs/"+url.PathEscape(dgst), mediaType, size, r)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		return 0, decodeHTTPError(resp)
	}
	defer resp.Body.Close()
	var res struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, err
	}
	return res.Size, nil
}

func (c *Client) PutRecord(ctx context.Context, rec CatalogRecordV1) (RecordStatus, error) {
	var st RecordStatus
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/records", rec, &st)
	return st, err
}

func (c *Client) PutArtifact(ctx context.Context, rec CatalogRecordV1, r io.Reader) (RecordStatus, error) {
	if rec.SchemaVersion == "" {
		rec.SchemaVersion = CatalogRecordSchemaV1
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if rec.LastAccessedAt.IsZero() {
		rec.LastAccessedAt = rec.CreatedAt
	}
	if err := rec.Validate(); err != nil {
		return RecordStatus{}, err
	}
	if _, err := c.PutBlob(ctx, rec.Digest, rec.Size, rec.MediaType, r); err != nil {
		return RecordStatus{}, err
	}
	return c.PutRecord(ctx, rec)
}

func (c *Client) GetRecordStatus(ctx context.Context, dgst string) (RecordStatus, error) {
	var st RecordStatus
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/records/"+url.PathEscape(dgst), nil, &st)
	return st, err
}

func (c *Client) GetRecord(ctx context.Context, dgst string) (CatalogRecordV1, error) {
	st, err := c.GetRecordStatus(ctx, dgst)
	if err != nil {
		return CatalogRecordV1{}, err
	}
	return st.Record, nil
}

func (c *Client) LookupLogical(ctx context.Context, name, version string) (RecordStatus, error) {
	var st RecordStatus
	q := url.Values{"name": {name}, "version": {version}}
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/records?"+q.Encode(), nil, &st)
	return st, err
}

func (c *Client) OpenBlob(ctx context.Context, dgst string) (io.ReadCloser, error) {
	if !ValidDigest(dgst) {
		return nil, fmt.Errorf("artifact: invalid digest %q", dgst)
	}
	resp, err := c.doRequest(ctx, http.MethodGet, "/v1/blobs/"+url.PathEscape(dgst), "", -1, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, decodeHTTPError(resp)
	}
	expectedSize := resp.ContentLength
	return &verifyingReadCloser{
		rc:           resp.Body,
		dgst:         dgst,
		expectedSize: expectedSize,
		hasher:       sha256.New(),
	}, nil
}

func (c *Client) FetchVerified(ctx context.Context, dgst string) ([]byte, error) {
	rc, err := c.OpenBlob(ctx, dgst)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if DigestBytes(data) != dgst {
		return nil, fmt.Errorf("%w: %s digest mismatch", ErrCorruptPayload, dgst)
	}
	return data, nil
}

func (c *Client) PublishModule(ctx context.Context, pub PublicationRecord) (bool, error) {
	var res struct {
		Idempotent bool `json:"idempotent"`
	}
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/publications", pub, &res)
	return res.Idempotent, err
}

func (c *Client) GetPublication(ctx context.Context, modPath, version string) (PublicationRecord, error) {
	var pub PublicationRecord
	q := url.Values{"module": {modPath}, "version": {version}}
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/publications?"+q.Encode(), nil, &pub)
	return pub, err
}

func (c *Client) ListPublications(ctx context.Context) ([]PublicationRecord, error) {
	var pubs []PublicationRecord
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/publications", nil, &pubs)
	return pubs, err
}

func (c *Client) PutManifest(ctx context.Context, m ManifestV1) error {
	var out ManifestV1
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/manifests", m, &out)
	return err
}

func (c *Client) GetManifest(ctx context.Context, generation uint64) (ManifestV1, error) {
	var m ManifestV1
	q := url.Values{"generation": {strconv.FormatUint(generation, 10)}}
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/manifests?"+q.Encode(), nil, &m)
	return m, err
}

func (c *Client) ListManifests(ctx context.Context) ([]ManifestV1, error) {
	var out []ManifestV1
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/manifests", nil, &out)
	return out, err
}

func (c *Client) AcquireLease(ctx context.Context, holderPID int, holderStartID string, ttl time.Duration, digests []string) (Lease, error) {
	var l Lease
	req := leaseRequest{
		HolderPID:     holderPID,
		HolderStartID: holderStartID,
		TTLMs:         max(int64(1), ttl.Milliseconds()),
		Digests:       digests,
	}
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/leases", req, &l)
	return l, err
}

func (c *Client) RenewLease(ctx context.Context, leaseID string, ttl time.Duration) (Lease, error) {
	var l Lease
	req := leaseRequest{
		LeaseID: leaseID,
		TTLMs:   max(int64(1), ttl.Milliseconds()),
	}
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/leases", req, &l)
	return l, err
}

func (c *Client) ReleaseLease(ctx context.Context, leaseID string) error {
	_, err := c.doJSON(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(leaseID), nil, nil)
	return err
}

func (c *Client) ListActiveLeases(ctx context.Context) ([]Lease, error) {
	var out []Lease
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/leases", nil, &out)
	return out, err
}

func (c *Client) WithReadLease(ctx context.Context, holderPID int, holderStartID string, ttl time.Duration, digests []string, fn func(context.Context) error) error {
	if holderPID <= 0 {
		holderPID, holderStartID = CurrentProcessIdentity(ctx)
	}
	l, err := c.AcquireLease(ctx, holderPID, holderStartID, ttl, digests)
	if err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.ReleaseLease(rctx, l.ID)
	}()
	return fn(ctx)
}

func (c *Client) DryRunPrune(ctx context.Context, older time.Duration) ([]prune.Item, error) {
	var res pruneResponse
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/prune", pruneRequest{Apply: false, OlderNs: int64(older)}, &res)
	return res.Items, err
}

func (c *Client) ApplyPrune(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	for _, item := range items {
		if item.Noun != "" && item.Noun != "artifact" {
			return prune.Counts{}, fmt.Errorf("artifact prune: foreign item noun %q (%s)", item.Noun, item.ID)
		}
	}
	if len(items) == 0 {
		return prune.Counts{}, nil
	}
	var res pruneResponse
	_, err := c.doJSON(ctx, http.MethodPost, "/v1/prune", pruneRequest{Apply: true, Items: items}, &res)
	return res.Counts, err
}

func (c *Client) Status(ctx context.Context) (StoreStatus, error) {
	var st StoreStatus
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// TryConnectService probes <root>/service.json + <root>/service.token and returns
// a live *Client if the loopback service is reachable and matches the recorded epoch.
func TryConnectService(ctx context.Context, root string) (*Client, bool) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = DefaultRoot()
	}
	epRaw, err := os.ReadFile(filepath.Join(root, serviceJSONFile))
	if err != nil {
		return nil, false
	}
	tokRaw, err := os.ReadFile(filepath.Join(root, serviceTokenFile))
	if err != nil {
		return nil, false
	}
	var ep ServiceEndpoint
	if err := json.Unmarshal(epRaw, &ep); err != nil || ep.Addr == "" || ep.Epoch == 0 {
		return nil, false
	}
	client := NewClient(ep.Addr, strings.TrimSpace(string(tokRaw)))
	probeCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	st, err := client.Status(probeCtx)
	if err != nil || st.ServiceEpoch != ep.Epoch {
		_ = client.Close()
		return nil, false
	}
	return client, true
}

// ConnectOrOpen transparently connects to the running loopback service at root
// if <root>/service.json is live, or opens the local *Store directly otherwise.
func ConnectOrOpen(ctx context.Context, root string) (Backend, error) {
	if client, ok := TryConnectService(ctx, root); ok {
		return client, nil
	}
	return OpenStore(ctx, StoreOptions{Root: root})
}
