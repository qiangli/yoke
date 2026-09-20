// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// The Settings-page twin of `bashy app pair`.
//
// WHY IT EXISTS. `bashy app pair` already mints a QR from the terminal. But the
// operator who is looking at the console in a browser has no terminal in front
// of them, and telling them to open one to reach the phone that is in their
// hand is a worse flow than a toggle. This moves the MINT behind a click while
// leaving everything that makes pairing safe exactly where it was.
//
// WHAT IT DOES NOT DO. It does not invent a second, weaker pairing path. The
// ticket is minted by the same pairStore, is single-use and time-boxed the same
// way, confers the same default scope (sprint/mb/meet — not the terminal, not
// files), and writes the same audit event. It does not broaden the listener:
// on a console that was not started with --pair it FAILS CLOSED and hands back
// the exact restart command, because a Settings toggle must never pretend it
// opened a LAN port that only `apps serve --bind … --pair` can open. No
// firewall or router change is ever made, and nothing here reaches the public
// internet.

// lanAddrFn and mdnsHostFn are the address probes, indirected so a test can pin
// them and assert deterministic dual-labelled output without depending on the
// host's real interfaces (primaryLANAddr is empty offline) or hostname.
var (
	lanAddrFn  = primaryLANAddr
	mdnsHostFn = mdnsName
)

// pairAddress is one dial-able address for a paired phone. Both addresses a
// mint returns carry the SAME single-use ticket: the phone scans whichever code
// its network can resolve, and the ticket closes on the first redemption
// regardless of which one was used.
type pairAddress struct {
	Kind      string `json:"kind"`       // "mdns" | "lan"
	Label     string `json:"label"`      // human label shown under the code
	Host      string `json:"host"`       // the bare host the URL dials
	AccessURL string `json:"access_url"` // clean root URL used after pairing
	URL       string `json:"url"`        // the versioned redeem URL the QR encodes
	QR        string `json:"qr"`         // PNG data URI of URL, or "" if it could not render
}

// handlePairMint mints a pairing ticket for the Settings page and returns
// scan-ready QR presentation for every address a phone could dial.
func (s *server) handlePairMint(w http.ResponseWriter, r *http.Request) {
	// Rate-limit on the PEER address, exactly like /login and /pair/redeem: not
	// X-Forwarded-For, which is attacker-supplied.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.limiter != nil && !s.limiter.Allow(host) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"enabled": true,
			"error":   "too many pairing attempts; wait a moment and try again",
		})
		return
	}

	// Minting a ticket is an OPERATOR act. A paired phone must not be able to
	// widen the door it came through, and an anonymous LAN visitor must not mint
	// at all. The gate already refuses a device at /api/pair (it is outside every
	// device scope) — this is the fail-closed check that does not depend on that.
	if !s.isOperator(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"enabled": true,
			"error":   "only the signed-in operator can enable phone access",
		})
		return
	}

	// Fail closed when this console cannot broaden its own listener. The toggle
	// must never PRETEND it opened LAN access: pairing is armed by
	// `bashy app serve --bind <lan-ip> --pair`, and if that was not asked for,
	// the honest answer is the command that would arm it.
	if s.pairing == nil {
		lan := lanAddrFn()
		writeJSON(w, http.StatusConflict, map[string]any{
			"enabled": false,
			"reason":  "phone access is not armed on this console",
			"detail": "The console must be started on the LAN with pairing on before a phone " +
				"can reach it. No firewall or router change is made for you, and this stays on " +
				"your local network — it is never exposed to the internet.",
			"restart":          "bashy app serve --bind " + BindLAN + " --pair",
			"lan_hint_guessed": lan == "",
		})
		return
	}

	// The operator may widen the pass beyond the read-and-communicate default.
	//
	// This is the one place a phone can be granted a shell, so it is bounded on
	// every side rather than trusted: the caller is already the signed-in
	// operator (isOperator, above), the names are validated against the panels
	// this console actually serves, and scope remains a NARROWING — applied
	// after the tier ladder, it can only remove reach from the operator's own
	// session, never add any. An absent or empty scope keeps the default, so
	// the fail-safe is still sprint/mb/meet.
	var req struct {
		Scope []string `json:"scope"`
		// TTLHours is how long the paired device keeps access. Absent or
		// negative means the default; ZERO MEANS NEVER — the operator asking
		// for a phone that stays paired until they revoke it, which is the
		// ordinary case for the machine on their own desk.
		TTLHours *float64 `json:"ttl_hours"`
	}
	if r.Body != nil {
		// Ignore a decode error deliberately: no body is the normal case and
		// means "default scope". A malformed body cannot widen anything,
		// because Scope stays nil and ValidateScope passes it through.
		_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)
	}
	if err := ValidateScope(req.Scope, s.panels); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"enabled": true,
			"error":   err.Error(),
		})
		return
	}

	// window 0: the two-minute scan window a bare `bashy app pair` confers —
	// it only has to survive the walk from the screen to the phone, and it is
	// not the clock anyone was fighting.
	t, secret, err := s.pairing.issueTicket(req.Scope, deviceTTLFrom(req.TTLHours), 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"enabled": true,
			"error":   "could not mint a pairing ticket: " + err.Error(),
		})
		return
	}

	addrs := s.pairAddresses(secret)
	auditPairEvent("pair.issued", map[string]string{
		"ticket": t.ID, "scope": strings.Join(t.Scope, ","),
		"device_ttl": t.TTL, "expires": t.Expires.Format(time.RFC3339),
		"peer": host, "via": "settings",
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"schema":     "bashy-apps-pair-v1",
		"ticket_id":  t.ID,
		"scope":      t.Scope,
		"device_ttl": t.TTL,
		"expires":    t.Expires,
		// Said in words as well as in a date, because "expires 2126-09-04" is a
		// date a reader has to decode before they can tell it means never.
		//
		// Computed from the DEVICE TTL, not from t.Expires: that field is the
		// scan window (about two minutes), so reading it here answered "does
		// the code on screen never expire" — always no — while appearing to
		// answer the question the operator asked.
		"never_expires":   neverExpires(t.TTL),
		"payload_version": pairQRVersion,
		"encrypted":       false,
		"note": "Plaintext HTTP on your LAN: this keeps your OS password off the wire, it does " +
			"not encrypt the link. Fine at home; not on shared wifi.",
		"addresses": addrs,
	})
}

// deviceTTLFrom reads the operator's chosen device lifetime.
//
// Three cases, and the middle one is the point: absent means "whatever the
// default is" (issueTicket applies it), zero means NEVER, and a positive value
// is taken literally. Zero cannot mean "default" here as it does deeper in the
// store, because zero is the spelling the operator picks in the UI for never —
// so it is resolved to the century-long TTL at this boundary and the store
// keeps its own convention unchanged.
func deviceTTLFrom(hours *float64) time.Duration {
	switch {
	case hours == nil || *hours < 0:
		return 0 // the store's default
	case *hours == 0:
		return neverExpiresTTL
	default:
		return time.Duration(*hours * float64(time.Hour))
	}
}

// neverExpires reads a stored device TTL back as the answer the operator gave.
// A TTL that fails to parse is not "never" — an unreadable value must not be
// reported as the most permissive one.
func neverExpires(ttl string) bool {
	d, err := time.ParseDuration(ttl)
	return err == nil && d > neverExpiresAfter
}

// pairAddresses builds the labelled codes for the Settings page. Both addresses
// are DERIVED, never guessed silently: the mDNS name is what the OS reports as
// this host's own name, the LAN address is the route this host would take to
// reach the network. Each is labelled so the operator knows which one their
// phone can resolve, and a single working code is enough.
func (s *server) pairAddresses(secret string) []pairAddress {
	var out []pairAddress
	if h := mdnsHostFn(); h != "" {
		out = append(out, s.pairAddress("mdns", "Hostname (.local)", h, secret))
	}
	if ip := lanAddrFn(); ip != "" {
		out = append(out, s.pairAddress("lan", "LAN address", ip, secret))
	}
	return out
}

func (s *server) pairAddress(kind, label, host, secret string) pairAddress {
	u := redeemURL(host, s.port, secret)
	access := "http://" + net.JoinHostPort(host, fmt.Sprint(s.port)) + "/"
	a := pairAddress{Kind: kind, Label: label, Host: host, AccessURL: access, URL: u}
	if data, err := qrPNGDataURI(u); err == nil {
		a.QR = data
	}
	return a
}

// qrPNGDataURI renders a payload as a PNG data URI for an <img src>. What it
// encodes is the VERSIONED redeem URL — the single-use ticket in that URL's
// query is the only credential, and there is never a password or a bare
// unauthenticated address in the code.
func qrPNGDataURI(payload string) (string, error) {
	png, err := qrcode.Encode(payload, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// isOperator reports whether this request is the console's authenticated
// operator, and NOT a paired device. A device-subject session is refused even
// though its cookie is otherwise valid, so a phone that already paired cannot
// mint further tickets.
func (s *server) isOperator(r *http.Request) bool {
	if s.sessions != nil {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if subject, ok := s.sessions.Validate(c.Value); ok {
				_, _, isDevice := splitDeviceSubject(subject)
				return !isDevice
			}
		}
	}
	if coopauth.ArrivedViaCloud(r) {
		return true
	}
	// The ungated loopback owner (a dev console that requires no login) is the
	// operator by the same rule the gate admits them under.
	return !s.requireLogin && coopauth.IsLoopbackAddr(r.RemoteAddr)
}
