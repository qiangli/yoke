// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/meet"
	"github.com/spf13/cobra"
)

// Messenger peers: the same tickets and devices, for a sender on a chat
// channel instead of a phone browser.
//
// `bashy meet dm --channel C --sender S` is the front door a gateway feeds
// external messages through. Who may speak there is a pairing question, and
// this file answers it with the store above rather than a second allowlist:
//
//   - an unknown sender's first message MINTS a ticket bound to (channel,
//     sender). The sender is shown the code; the store keeps only its hash.
//   - the OPERATOR approves the code on this host (`bashy app pair approve
//     CODE --allow ask,approve`). That redeems the ticket — single use — into
//     a device bound to the same (channel, sender), with a scope and an
//     expiry, hanging off the operator grant like every other device.
//   - `bashy app devices` lists it, `bashy app revoke ID` (or `meet dm
//     --revoke`) ends it, and `app revoke --all` ends it with everything else.
//
// The direction is inverted from a QR on purpose: a browser proves possession
// by presenting the secret the operator minted; a messenger sender cannot be
// handed one out of band, so the sender is handed the code and the operator —
// the only party who can run this CLI — presents it.
//
// A peer device is never a browser session: the gate refuses one, it does not
// hold the LAN listener open, and its scope names meet's verbs (ask, control,
// approve), not panels.

// peer is the messenger identity a ticket or device is bound to.
type peer struct {
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
}

// peerTicketWindow is how long a sender's code stays approvable. Longer than a
// QR's two minutes: it has to wait for an operator, not a walk to the couch.
const peerTicketWindow = 30 * time.Minute

var defaultPeerScope = []string{meet.PeerScopeAsk}

var errPeerDenied = errors.New("pairing code was denied")

func init() { meet.SetPeerPairing(peerPairing{}) }

// normalizePeerScope validates and canonicalizes a sender scope. Unknown names
// are refused, never dropped: a scope is authority.
func normalizePeerScope(names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || slices.Contains(out, n) {
			continue
		}
		if !slices.Contains(meet.PeerScopes, n) {
			return nil, fmt.Errorf("unknown sender scope %q (choose from %s)", n, strings.Join(meet.PeerScopes, ", "))
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return append([]string(nil), defaultPeerScope...), nil
	}
	slices.Sort(out)
	return out, nil
}

// newPeerCode is a code a person can read off a chat and type: 40 random bits
// as eight base32 characters, shown as XXXX-XXXX.
func newPeerCode() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	c := base32.StdEncoding.EncodeToString(b)
	return c[:4] + "-" + c[4:], nil
}

// canonPeerCode forgives how the operator typed it back.
func canonPeerCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

func peerOf(channel, sender string) peer {
	return peer{Channel: strings.TrimSpace(channel), Sender: strings.TrimSpace(sender)}
}

// peerDevice returns the sender's newest live device.
func (s *pairStore) peerDevice(channel, sender string) (device, bool, error) {
	st, err := s.load()
	if err != nil {
		return device{}, false, err
	}
	want, now := peerOf(channel, sender), s.now().UTC()
	var best device
	found := false
	for _, d := range st.Devices {
		if d.Peer == nil || *d.Peer != want || !st.deviceLive(d, now) {
			continue
		}
		if !found || d.Issued.After(best.Issued) {
			best, found = d, true
		}
	}
	return best, found, nil
}

// requestPeer is an unpaired sender asking to be let in. The newest ticket for
// the sender decides: open → pending (no second code), denied → denied, and
// otherwise (none, expired, or redeemed into a device since revoked) a fresh
// ticket is minted and its code returned once.
func (s *pairStore) requestPeer(channel, sender string) (meet.PeerRequest, error) {
	want, now := peerOf(channel, sender), s.now().UTC()
	var out meet.PeerRequest
	err := s.mutate(func(st *pairState) error {
		var last *ticket
		for i := range st.Tickets {
			t := &st.Tickets[i]
			if t.Peer != nil && *t.Peer == want && (last == nil || t.Issued.After(last.Issued)) {
				last = t
			}
		}
		if last != nil {
			switch {
			case last.open(now):
				out = meet.PeerRequest{Ticket: last.ID, Expires: last.Expires, Pending: true}
				return errSkipWrite
			case last.Denied != nil:
				out = meet.PeerRequest{Ticket: last.ID, Expires: last.Expires, Denied: true}
				return errSkipWrite
			}
		}
		code, err := newPeerCode()
		if err != nil {
			return err
		}
		id, err := randToken(6)
		if err != nil {
			return err
		}
		p := want
		st.Tickets = append(st.Tickets, ticket{
			ID: id, Hash: hashSecret(canonPeerCode(code)),
			Issued: now, Expires: now.Add(peerTicketWindow), Peer: &p,
		})
		out = meet.PeerRequest{Code: code, Ticket: id, Expires: now.Add(peerTicketWindow)}
		return nil
	})
	if errors.Is(err, errSkipWrite) {
		err = nil
	}
	if err == nil && out.Code != "" {
		auditPairEvent("pair.peer_requested", map[string]string{
			"ticket": out.Ticket, "channel": want.Channel, "sender": want.Sender,
		})
	}
	return out, err
}

// findPeerTicket locates a sender's ticket by its code, refusing a browser
// ticket: a QR secret is redeemed by the phone that scanned it, not approved.
func (st *pairState) findPeerTicket(code string, now time.Time) (*ticket, error) {
	want := hashSecret(canonPeerCode(code))
	for i := range st.Tickets {
		t := &st.Tickets[i]
		if t.Hash != want || t.Peer == nil {
			continue
		}
		switch {
		case t.Redeemed != nil:
			return nil, errTicketUsed
		case t.Denied != nil:
			return nil, errPeerDenied
		case !now.Before(t.Expires):
			return nil, errTicketStale
		}
		return t, nil
	}
	return nil, errNoTicket
}

// approvePeer is the operator redeeming a sender's code into a device. It is
// the operator acting, so — exactly as issueTicket does for a QR — it opens an
// operator grant when none is live and extends it to the device's lifetime.
func (s *pairStore) approvePeer(code string, scope []string, ttl time.Duration) (device, error) {
	return s.approvePeerAs(code, scope, ttl, "")
}

func (s *pairStore) approvePeerAs(code string, scope []string, ttl time.Duration, principal string) (device, error) {
	principal = strings.TrimSpace(principal)
	if principal != "" {
		if err := validatePrincipalName(principal); err != nil {
			return device{}, err
		}
	}
	scope, err := normalizePeerScope(scope)
	if err != nil {
		return device{}, err
	}
	if ttl <= 0 {
		ttl = defaultDeviceTTL
	}
	now := s.now().UTC()
	var out device
	err = s.mutate(func(st *pairState) error {
		t, err := st.findPeerTicket(code, now)
		if err != nil {
			return err
		}
		if !st.Grant.live(now) {
			gid, err := randToken(9)
			if err != nil {
				return err
			}
			st.Grant = &grant{ID: gid, Issued: now, Expires: now.Add(operatorGrantTTL)}
		}
		exp := now.Add(ttl)
		if exp.After(st.Grant.Expires) {
			st.Grant.Expires = exp
		}
		id, err := randToken(8)
		if err != nil {
			return err
		}
		p := *t.Peer
		out = device{
			ID: id, Name: p.Channel + ":" + p.Sender, Grant: st.Grant.ID,
			Scope: scope, Issued: now, Expires: exp, Peer: &p, Principal: principal,
		}
		st.Devices = append(st.Devices, out)
		t.Grant, t.Scope, t.TTL = st.Grant.ID, scope, ttl.String()
		t.Redeemed, t.DeviceID = &now, id
		return nil
	})
	if err != nil {
		return device{}, err
	}
	auditPairEvent("pair.peer_approved", map[string]string{
		"device": out.ID, "channel": out.Peer.Channel, "sender": out.Peer.Sender,
		"scope": strings.Join(out.Scope, ","), "expires": out.Expires.Format(time.RFC3339),
	})
	return out, nil
}

// denyPeer closes a sender's code without a device. The denial is remembered
// for as long as the ticket is kept, so resending does not mint a fresh code.
func (s *pairStore) denyPeer(code string) (peer, error) {
	now := s.now().UTC()
	var out peer
	err := s.mutate(func(st *pairState) error {
		t, err := st.findPeerTicket(code, now)
		if err != nil {
			return err
		}
		t.Denied = &now
		out = *t.Peer
		return nil
	})
	if err == nil {
		auditPairEvent("pair.peer_denied", map[string]string{"channel": out.Channel, "sender": out.Sender})
	}
	return out, err
}

// revokePeer ends every live device bound to the sender and closes any code
// still waiting, so a revoke cannot be undone by an approval already in
// flight.
func (s *pairStore) revokePeer(channel, sender string) (int, error) {
	want, now := peerOf(channel, sender), s.now().UTC()
	n := 0
	err := s.mutate(func(st *pairState) error {
		changed := false
		for i := range st.Devices {
			d := &st.Devices[i]
			if d.Peer != nil && *d.Peer == want && !d.Revoked {
				if st.deviceLive(*d, now) {
					n++
				}
				d.Revoked, d.RevokedAt, changed = true, now, true
			}
		}
		for i := range st.Tickets {
			t := &st.Tickets[i]
			if t.Peer != nil && *t.Peer == want && t.open(now) {
				t.Expires, changed = now, true
			}
		}
		if !changed {
			return errSkipWrite
		}
		return nil
	})
	if errors.Is(err, errSkipWrite) {
		err = nil
	}
	if err == nil && n > 0 {
		auditPairEvent("pair.peer_revoked", map[string]string{"channel": want.Channel, "sender": want.Sender})
	}
	return n, err
}

// peerPairing is meet.PeerPairing over the default store.
type peerPairing struct{}

func (peerPairing) Paired(channel, sender string) (meet.PairedPeer, bool, error) {
	store, err := openPairStore()
	if err != nil {
		return meet.PairedPeer{}, false, err
	}
	d, ok, err := store.peerDevice(channel, sender)
	if err != nil || !ok {
		return meet.PairedPeer{}, false, err
	}
	store.touch(d.ID)
	return meet.PairedPeer{Device: d.ID, Scope: append([]string(nil), d.Scope...), Expires: d.Expires}, true, nil
}

func (peerPairing) Request(channel, sender string) (meet.PeerRequest, error) {
	store, err := openPairStore()
	if err != nil {
		return meet.PeerRequest{}, err
	}
	return store.requestPeer(channel, sender)
}

func (peerPairing) Revoke(channel, sender string) (int, error) {
	store, err := openPairStore()
	if err != nil {
		return 0, err
	}
	return store.revokePeer(channel, sender)
}

// ---------------------------------------------------------------------------
// `bashy app pair approve|deny CODE`
// ---------------------------------------------------------------------------

func newPairApproveCmd() *cobra.Command {
	var (
		allow     []string
		ttl       time.Duration
		principal string
	)
	cmd := &cobra.Command{
		Use:   "approve CODE",
		Short: "admit a messenger sender that was shown a pair code by `bashy meet dm`",
		Long: "approve redeems a sender's single-use code into a device bound to that\n" +
			"channel and sender. --allow sets what it may do through `bashy meet dm`:\n" +
			"  ask      message an agent; the turn runs read-only (the default)\n" +
			"  control  message an agent with ordinary turn authority; /meet ROOM TEXT\n" +
			"  approve  /approve ID and /deny ID on the shared approval queue\n" +
			"End it with `bashy app revoke ID` or `bashy meet dm --channel C --sender S --revoke`.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			store, err := openPairStore()
			if err != nil {
				return err
			}
			d, err := store.approvePeerAs(args[0], allow, ttl, principal)
			if err != nil {
				return fmt.Errorf("apps pair approve: %w", err)
			}
			fmt.Fprintf(c.OutOrStdout(), "paired %s on channel %s as %s\n  scope:  %s\n  until:  %s\n  revoke: bashy app revoke %s\n",
				d.Peer.Sender, d.Peer.Channel, d.ID, strings.Join(d.Scope, ", "),
				d.Expires.Local().Format(time.RFC1123), d.ID)
			if d.Principal != "" {
				fmt.Fprintf(c.OutOrStdout(), "  principal: %s\n", d.Principal)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "sender scope: ask, control, approve (default ask)")
	cmd.Flags().DurationVar(&ttl, "ttl", defaultDeviceTTL, "how long the sender stays paired")
	cmd.Flags().StringVar(&principal, "principal", "", "named web principal for this pairing (empty means base-only)")
	return cmd
}

func newPairDenyCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "deny CODE",
		Short:         "refuse a messenger sender's pair code",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			store, err := openPairStore()
			if err != nil {
				return err
			}
			p, err := store.denyPeer(args[0])
			if err != nil {
				return fmt.Errorf("apps pair deny: %w", err)
			}
			fmt.Fprintf(c.OutOrStdout(), "denied %s on channel %s\n", p.Sender, p.Channel)
			return nil
		},
	}
}
