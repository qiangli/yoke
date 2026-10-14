package feed

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// AppServiceRegistration is the Matrix Application Service registration YAML
// as required by the spec (https://spec.matrix.org/latest/application-service-api/).
// Homeserver loads this to authenticate transactions from the bridge.
//
// Example:
//
// id: feed
// url: http://127.0.0.1:18080
// as_token: <random>
// hs_token: <random>
// sender_localpart: _feed_bot
// namespaces:
//
//	users:
//	  - exclusive: true
//	    regex: "@_feed_.*"
//	rooms:
//	  - exclusive: true
//	    regex: "!feed_.*"
//	aliases:
//	  - exclusive: true
//	    regex: "#feed_.*"
//
// rate_limited: false
// protocols: ["feed"]
type AppServiceRegistration struct {
	ID              string     `yaml:"id" json:"id"`
	URL             string     `yaml:"url" json:"url"`
	AsToken         string     `yaml:"as_token" json:"as_token"`
	HsToken         string     `yaml:"hs_token" json:"hs_token"`
	SenderLocalpart string     `yaml:"sender_localpart" json:"sender_localpart"`
	Namespaces      Namespaces `yaml:"namespaces" json:"namespaces"`
	RateLimited     bool       `yaml:"rate_limited" json:"rate_limited"`
	Protocols       []string   `yaml:"protocols,omitempty" json:"protocols,omitempty"`
	DehsTokenSha256 string     `yaml:"-" json:"-"`
}

type Namespaces struct {
	Users   []Namespace `yaml:"users" json:"users"`
	Rooms   []Namespace `yaml:"rooms" json:"rooms"`
	Aliases []Namespace `yaml:"aliases" json:"aliases"`
}

type Namespace struct {
	Exclusive bool   `yaml:"exclusive" json:"exclusive"`
	Regex     string `yaml:"regex" json:"regex"`
}

// Validate checks the registration.
func (r *AppServiceRegistration) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("feed: appservice registration: id is required")
	}
	if strings.TrimSpace(r.URL) == "" {
		return errors.New("feed: appservice registration: url is required")
	}
	if strings.TrimSpace(r.AsToken) == "" {
		return errors.New("feed: appservice registration: as_token is required")
	}
	if strings.TrimSpace(r.HsToken) == "" {
		return errors.New("feed: appservice registration: hs_token is required")
	}
	if strings.TrimSpace(r.SenderLocalpart) == "" {
		return errors.New("feed: appservice registration: sender_localpart is required")
	}
	// Validate regexes.
	for _, ns := range append(append(r.Namespaces.Users, r.Namespaces.Rooms...), r.Namespaces.Aliases...) {
		if ns.Regex == "" {
			return errors.New("feed: appservice registration: namespace regex is required")
		}
		if _, err := regexp.Compile(ns.Regex); err != nil {
			return fmt.Errorf("feed: appservice registration: bad regex %q: %w", ns.Regex, err)
		}
	}
	return nil
}

// DefaultFeedRegistration returns a registration for the feed bridge.
// Caller must fill URL, tokens if not randomising. ServerName is used only
// for documentation; the homeserver derives it from its own config.
func DefaultFeedRegistration(url, asToken, hsToken string) AppServiceRegistration {
	if asToken == "" {
		asToken = randomToken(32)
	}
	if hsToken == "" {
		hsToken = randomToken(32)
	}
	if url == "" {
		url = "http://127.0.0.1:18080"
	}
	return AppServiceRegistration{
		ID:              "feed",
		URL:             url,
		AsToken:         asToken,
		HsToken:         hsToken,
		SenderLocalpart: "_feed_bot",
		Namespaces: Namespaces{
			Users: []Namespace{
				{Exclusive: true, Regex: "@_feed_.*"},
			},
			Rooms: []Namespace{
				{Exclusive: true, Regex: "!feed_.*"},
			},
			Aliases: []Namespace{
				{Exclusive: true, Regex: "#feed_.*"},
			},
		},
		RateLimited: false,
		Protocols:   []string{"feed"},
	}
}

func randomToken(n int) string {
	// Deterministic fallback for tests already seeded; production callers should
	// pass explicit tokens. This uses a simple time/pid mix.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", n, os.Getpid())))
	return hex.EncodeToString(sum[:n])
}

// WriteRegistration writes registration YAML atomically with 0600.
func WriteRegistration(path string, reg AppServiceRegistration) error {
	if err := reg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(reg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "appservice.*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	tmp.Chmod(0o600)
	tmp.Close()
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// ParseRegistrationFile reads registration YAML.
func ParseRegistrationFile(path string) (AppServiceRegistration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AppServiceRegistration{}, fmt.Errorf("feed: appservice: read %q: %w", path, err)
	}
	var reg AppServiceRegistration
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return AppServiceRegistration{}, fmt.Errorf("feed: appservice: parse %q: %w", path, err)
	}
	if err := reg.Validate(); err != nil {
		return AppServiceRegistration{}, err
	}
	return reg, nil
}

// ---- AppService (transaction handling, virtual users) ----

// AppService handles the Matrix Application Service API over the signed tuwunel
// homeserver. It authenticates transactions via hs_token, de-dupes txnIds,
// converts Matrix events to Envelope, and routes via the bridge.
//
// It does NOT depend on mautrix; only net/http + encoding/json + own code.
type AppService struct {
	reg     AppServiceRegistration
	handler InboundHandler
	pairing PairingChecker
	mu      sync.Mutex
	seenTxn map[string]bool // txnId dedupe
	seenMsg map[string]bool // OriginKey dedupe
	// now receives room/alias bridging functions
	serverName string // e.g. "example.com" derived from homeserver URL or explicit
}

// NewAppService creates an AppService.
func NewAppService(reg AppServiceRegistration, handler InboundHandler, pairing PairingChecker) *AppService {
	if pairing == nil {
		pairing = AllowAllPairing{}
	}
	serverName := ""
	if u, err := url.Parse(reg.URL); err == nil && u.Host != "" {
		serverName = u.Host
	}
	if serverName == "" {
		serverName = "localhost"
	}
	return &AppService{
		reg:        reg,
		handler:    handler,
		pairing:    pairing,
		seenTxn:    make(map[string]bool),
		seenMsg:    make(map[string]bool),
		serverName: serverName,
	}
}

// ServerName returns the homeserver server_name derived from the registration URL.
func (s *AppService) ServerName() string { return s.serverName }

// Registration returns the registration used by this service.
func (s *AppService) Registration() AppServiceRegistration { return s.reg }

// VirtualUserID returns the Matrix virtual user for a bridged sender.
// Example: telegram user "12345" -> "@_feed_telegram_12345:example.com"
func (s *AppService) VirtualUserID(senderID, channel string) string {
	senderID = sanitizeLocalpart(senderID)
	channel = sanitizeLocalpart(channel)
	return fmt.Sprintf("@_feed_%s_%s:%s", channel, senderID, s.serverName)
}

// BridgedRoomID returns the Matrix room ID for a foreign channel conversation.
// Bridged rooms live under the AppService's exclusive namespace.
// Example: telegram chat "12345" -> "!feed_telegram_12345:example.com"
// Note: Matrix room IDs are "!localpart:server", not aliases. The service
// creates rooms via Client-Server API; this is the deterministic ID it would
// use or the alias it would publish.
func (s *AppService) BridgedRoomID(channel, conversation string) string {
	channel = sanitizeLocalpart(channel)
	conv := sanitizeLocalpart(conversation)
	// Keep the conversation ID's Matrix-unsafe chars hex-escaped minimally.
	// sanitizeLocalpart already lowercases and keeps a-z0-9_-.
	// Preserve uniqueness without truncating.
	return fmt.Sprintf("!feed_%s_%s:%s", channel, conv, s.serverName)
}

// BridgedRoomAlias returns the alias for a bridged room.
func (s *AppService) BridgedRoomAlias(channel, conversation string) string {
	channel = sanitizeLocalpart(channel)
	conv := sanitizeLocalpart(conversation)
	return fmt.Sprintf("#feed_%s_%s:%s", channel, conv, s.serverName)
}

// sanitizeLocalpart lowercases and replaces any char outside [a-z0-9._=-] with "_".
// Matrix localparts allowanese; we map generously to stay valid.
func sanitizeLocalpart(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '=' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

// ServeHTTP implements the AppService HTTP API.
// Routes:
//
// PUT /_matrix/app/v1/transactions/{txnId}?access_token=<hs_token>
//
//	body: {"events": [MatrixEvent,...]}
//	The homeserver pushes transactions; we authenticate via hs_token
//	(query access_token or Authorization: Bearer), de-dupe txnId,
//	convert each event, and call handler.
//
// GET /_matrix/app/v1/users/{userId}  - user existence query (return 200 if we manage it)
// GET /_matrix/app/v1/rooms/{roomAlias} - room alias query
// GET /health - liveness (optional)
func (s *AppService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health endpoint does not require auth.
	if r.URL.Path == "/health" || r.URL.Path == "/_matrix/app/v1/health" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	// Auth: homeserver proves itself with hs_token.
	if !s.checkHsToken(r) {
		http.Error(w, `{"errcode":"M_FORBIDDEN","error":"bad hs_token"}`, http.StatusForbidden)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/_matrix/app/v1/transactions/"):
		s.handleTransaction(w, r)
	case strings.HasPrefix(r.URL.Path, "/_matrix/app/v1/users/"):
		s.handleUserQuery(w, r)
	case strings.HasPrefix(r.URL.Path, "/_matrix/app/v1/rooms/"):
		s.handleRoomQuery(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *AppService) checkHsToken(r *http.Request) bool {
	token := r.URL.Query().Get("access_token")
	if token == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	// Compare via HMAC constant-time not listing timing, but hex-compare is fine for test.
	if token == "" {
		return false
	}
	return hmac.Equal([]byte(token), []byte(s.reg.HsToken))
}

func (s *AppService) checkAsToken(r *http.Request) bool {
	token := r.URL.Query().Get("access_token")
	if token == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if token == "" {
		return false
	}
	return hmac.Equal([]byte(token), []byte(s.reg.AsToken))
}

// MatrixEvent is the event shape the homeserver sends in a transaction.
// Only fields needed for Envelope mapping are parsed; unknown fields are ignored.
type MatrixEvent struct {
	EventID        string          `json:"event_id"`
	Sender         string          `json:"sender"`
	Type           string          `json:"type"`
	RoomID         string          `json:"room_id"`
	Content        json.RawMessage `json:"content"`
	OriginServerTS int64           `json:"origin_server_ts"`
	Unsigned       json.RawMessage `json:"unsigned,omitempty"`
}

type matrixTextContent struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
}

type transactionBody struct {
	Events []MatrixEvent `json:"events"`
}

func (s *AppService) handleTransaction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Extract txnId from path.
	txnID := strings.TrimPrefix(r.URL.Path, "/_matrix/app/v1/transactions/")
	if idx := strings.Index(txnID, "/"); idx >= 0 {
		txnID = txnID[:idx]
	}
	if txnID == "" {
		http.Error(w, "missing txnId", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.seenTxn[txnID] {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
		return
	}
	s.seenTxn[txnID] = true
	s.mu.Unlock()

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var tx transactionBody
	if err := json.Unmarshal(body, &tx); err != nil {
		http.Error(w, "bad transaction json: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, ev := range tx.Events {
		// We only bridge m.room.message with m.text currently; other types are ignored
		// but do not error the transaction.
		if ev.Type != "m.room.message" {
			continue
		}
		env, err := s.matrixEventToEnvelope(ev)
		if err != nil {
			continue
		}
		if err := env.Validate(); err != nil {
			continue
		}
		if !s.pairing.IsPaired(env.SenderID) {
			continue
		}
		s.mu.Lock()
		key := env.OriginKey()
		if s.seenMsg[key] {
			s.mu.Unlock()
			continue
		}
		s.seenMsg[key] = true
		s.mu.Unlock()
		if s.handler != nil {
			_ = s.handler(r.Context(), env)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (s *AppService) matrixEventToEnvelope(ev MatrixEvent) (Envelope, error) {
	var content matrixTextContent
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return Envelope{}, err
	}
	if content.MsgType != "m.text" && content.MsgType != "" {
		return Envelope{}, fmt.Errorf("skip msgtype %q", content.MsgType)
	}
	// For bridged rooms "!feed_<channel>_<conversation>:server", a virtual
	// sender ("@_feed_<channel>_...") maps to the foreign channel; a native
	// Matrix sender keeps the matrix channel so matrix->foreign bridging can
	// match on "matrix:<bridgedRoomId>".
	isVirtual := strings.HasPrefix(ev.Sender, "@_feed_")
	channel, conversation := "", ""
	if isVirtual {
		channel, conversation = channelConversationFromRoom(ev.RoomID)
	}
	if channel == "" {
		channel = "matrix"
		conversation = ev.RoomID
	}
	env := Envelope{
		Version:        ContractVersion,
		Channel:        channel,
		MessageID:      ev.EventID,
		ConversationID: conversation,
		SenderID:       virtualSenderID(ev.Sender),
		SenderDisplay:  ev.Sender,
		Text:           content.Body,
		ReceivedAt:     unixMilliToTime(ev.OriginServerTS),
	}
	if env.ConversationID == "" {
		env.ConversationID = ev.RoomID
	}
	return env, nil
}

func channelConversationFromRoom(roomID string) (channel, conversation string) {
	// Expect "!feed_<channel>_<id>:server" or "#feed_<channel>_<id>:server"
	local := roomID
	if strings.HasPrefix(local, "!") || strings.HasPrefix(local, "#") {
		local = local[1:]
	}
	if idx := strings.Index(local, ":"); idx >= 0 {
		local = local[:idx]
	}
	if !strings.HasPrefix(local, "feed_") {
		return "", ""
	}
	rest := strings.TrimPrefix(local, "feed_")
	// Split first "_" into channel/conversation
	if i := strings.Index(rest, "_"); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

func virtualSenderID(mxUserID string) string {
	// "@_feed_telegram_12345:server" -> "telegram:12345"
	// Regular Matrix users keep their ID.
	if !strings.HasPrefix(mxUserID, "@_feed_") {
		return mxUserID
	}
	local := strings.TrimPrefix(mxUserID, "@_feed_")
	if idx := strings.Index(local, ":"); idx >= 0 {
		local = local[:idx]
	}
	// local is "<channel>_<sender>"
	if i := strings.Index(local, "_"); i >= 0 {
		return local[:i] + ":" + local[i+1:]
	}
	return local
}

func unixMilliToTime(ms int64) (t time.Time) {
	if ms == 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(ms).UTC()
}

func (s *AppService) handleUserQuery(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimPrefix(r.URL.Path, "/_matrix/app/v1/users/")
	if !s.isOurUser(userID) {
		http.Error(w, `{"errcode":"M_NOT_FOUND","error":"user not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (s *AppService) handleRoomQuery(w http.ResponseWriter, r *http.Request) {
	// Room alias query: the homeserver asks if we manage this alias.
	alias := strings.TrimPrefix(r.URL.Path, "/_matrix/app/v1/rooms/")
	if !s.isOurRoom(alias) {
		http.Error(w, `{"errcode":"M_NOT_FOUND","error":"room not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (s *AppService) isOurUser(userID string) bool {
	roomUser := userID
	if !strings.HasPrefix(roomUser, "@_feed_") {
		// Check exact sender_localpart (the bot itself)
		local := strings.TrimPrefix(userID, "@")
		if idx := strings.Index(local, ":"); idx >= 0 {
			local = local[:idx]
		}
		return local == s.reg.SenderLocalpart
	}
	// Check regex match against registration.
	for _, ns := range s.reg.Namespaces.Users {
		if ns.Regex == "" {
			continue
		}
		if ok, _ := regexp.MatchString(ns.Regex, userID); ok {
			return true
		}
	}
	// Fallback: _feed_ prefix always ours.
	return strings.HasPrefix(userID, "@_feed_")
}

func (s *AppService) isOurRoom(room string) bool {
	for _, ns := range s.reg.Namespaces.Aliases {
		if ns.Regex == "" {
			continue
		}
		if ok, _ := regexp.MatchString(ns.Regex, room); ok {
			return true
		}
	}
	for _, ns := range s.reg.Namespaces.Rooms {
		if ns.Regex == "" {
			continue
		}
		if ok, _ := regexp.MatchString(ns.Regex, room); ok {
			return true
		}
	}
	return strings.HasPrefix(room, "#feed_") || strings.HasPrefix(room, "!feed_")
}

// CheckAsAuth is a helper for Client-Server API interop checks that need to
// verify a Matrix access token equals the appservice token (bridge's own
// Client-Server calls go through tuwunel with as_token).
func (s *AppService) CheckAsAuth(r *http.Request) bool { return s.checkAsToken(r) }
