// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/meshagent"
)

// The Neighborhood section of the launcher (sprint 220, story ed857a89): the
// hosts on the same LAN, reached directly — never through cloudbox — from two
// discovery paths outpost already runs: the libp2p mesh (a paired host's own
// peers, direct links classified `lan`) and the `_outpost._tcp` mDNS browse
// (any outpost advertising on this network, this user's or another's). An
// unpaired host still has the browse — it is a sphere-tier feature. bashy
// adds no transport of its own: it reads `outpost mesh status --json` and
// `outpost scan --json` through the mesh agent it can resolve or provision.
//
// The name was decided here: "Neighborhood". "Sphere" is the tier's name and
// would overload it; "Nearby" and "LAN" say less about what the list is for.

type neighbor struct {
	Name    string   `json:"name"`              // the host's outpost name (or its peer id when unnamed)
	Via     []string `json:"via"`               // mesh · mdns
	Link    string   `json:"link,omitempty"`    // lan · wan · "" (mesh link class)
	Address string   `json:"address,omitempty"` // first direct IPv4/IPv6 seen
	Owner   string   `json:"owner"`             // this-account · discovered
	User    string   `json:"user,omitempty"`    // OS user the mDNS record carried, when any
	Paired  bool     `json:"paired"`            // the neighbour reports itself paired (mDNS) or is a mesh peer
	PeerID  string   `json:"peer_id,omitempty"`
}

type neighborhoodView struct {
	Hosts   []neighbor `json:"hosts"`
	Sources struct {
		Mesh string `json:"mesh"` // ok · off · error text
		MDNS string `json:"mdns"`
	} `json:"sources"`
	Agent string    `json:"agent"` // installed · missing
	At    time.Time `json:"at"`
}

// meshStatusJSON / scanJSON are the two wire shapes, decoded minimally.
type meshStatusJSON struct {
	Status *struct {
		Peers []struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			Direct    bool     `json:"direct"`
			LinkClass string   `json:"link_class"`
			Remote    []string `json:"remote"`
		} `json:"peers"`
	} `json:"status"`
}

type scanPeerJSON struct {
	AgentName  string `json:"agent_name"`
	OSUsername string `json:"os_username"`
	Paired     bool   `json:"paired"`
	Endpoints  []struct {
		Kind string `json:"kind"`
		Host string `json:"host"` // IP literal, DNS name, or <assigned_hostname>.local
		Port int    `json:"port"`
	} `json:"endpoints"`
}

// outpostJSON is the exec seam: a test points it at a stand-in.
var outpostJSON = func(ctx context.Context, args ...string) ([]byte, error) {
	bin, ok := meshagent.Resolve()
	if !ok {
		return nil, meshagent.ErrNotFound
	}
	c := exec.CommandContext(ctx, bin, args...)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		if s := strings.TrimSpace(errOut.String()); s != "" {
			return nil, &execError{msg: firstLine(s, err)}
		}
		return nil, err
	}
	return out.Bytes(), nil
}

type execError struct{ msg string }

func (e *execError) Error() string { return e.msg }

// neighborhood composes the list. Each source fails on its own: a host with
// no daemon (unpaired, or just not running) still gets the mDNS answer.
func neighborhood(ctx context.Context) neighborhoodView {
	v := neighborhoodView{At: time.Now().UTC(), Hosts: []neighbor{}}
	if _, ok := meshagent.Resolve(); !ok {
		v.Agent = "missing"
		v.Sources.Mesh, v.Sources.MDNS = "off", "off"
		return v
	}
	v.Agent = "installed"
	byName := map[string]*neighbor{}
	add := func(n neighbor) {
		key := strings.ToLower(n.Name)
		if cur, ok := byName[key]; ok {
			for _, via := range n.Via {
				if !contains(cur.Via, via) {
					cur.Via = append(cur.Via, via)
				}
			}
			if cur.Link == "" {
				cur.Link = n.Link
			}
			if cur.Address == "" {
				cur.Address = n.Address
			}
			if cur.User == "" {
				cur.User = n.User
			}
			cur.Paired = cur.Paired || n.Paired
			if cur.Owner != "this-account" {
				cur.Owner = n.Owner
			}
			return
		}
		c := n
		byName[key] = &c
	}

	mctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	raw, err := outpostJSON(mctx, "mesh", "status", "--json")
	cancel()
	switch {
	case err != nil:
		v.Sources.Mesh = firstLine(err.Error(), err)
	default:
		var ms meshStatusJSON
		if jerr := json.Unmarshal(raw, &ms); jerr != nil || ms.Status == nil {
			v.Sources.Mesh = "off"
		} else {
			v.Sources.Mesh = "ok"
			for _, p := range ms.Status.Peers {
				if !p.Direct || p.LinkClass != "lan" {
					continue // a WAN or relayed peer is not a neighbour
				}
				name := p.Name
				if name == "" {
					name = p.ID
					if len(name) > 12 {
						name = name[:12] + "…"
					}
				}
				add(neighbor{Name: name, Via: []string{"mesh"}, Link: p.LinkClass, Address: firstIP(p.Remote),
					Owner: "this-account", Paired: true, PeerID: p.ID})
			}
		}
	}

	sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	raw, err = outpostJSON(sctx, "scan", "--json", "--timeout", "2s")
	cancel()
	switch {
	case err != nil:
		v.Sources.MDNS = firstLine(err.Error(), err)
	default:
		var peers []scanPeerJSON
		if jerr := json.Unmarshal(raw, &peers); jerr != nil {
			v.Sources.MDNS = "unreadable"
		} else {
			v.Sources.MDNS = "ok"
			for _, p := range peers {
				if p.AgentName == "" {
					continue
				}
				addr := ""
				for _, e := range p.Endpoints {
					if e.Host != "" {
						addr = e.Host
						break
					}
				}
				add(neighbor{Name: p.AgentName, Via: []string{"mdns"}, Link: "lan", Address: addr,
					Owner: "discovered", User: p.OSUsername, Paired: p.Paired})
			}
		}
	}
	for _, n := range byName {
		v.Hosts = append(v.Hosts, *n)
	}
	sort.Slice(v.Hosts, func(i, j int) bool { return v.Hosts[i].Name < v.Hosts[j].Name })
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// firstIP pulls the address out of a multiaddr like
// /ip4/10.0.0.210/udp/55087/quic-v1 — an IPv4 when the peer has one (the
// spelling a person recognises), else the first IPv6.
func firstIP(remote []string) string {
	v6 := ""
	for _, r := range remote {
		parts := strings.Split(r, "/")
		for i := 0; i+1 < len(parts); i++ {
			switch parts[i] {
			case "ip4":
				return parts[i+1]
			case "ip6":
				if v6 == "" {
					v6 = parts[i+1]
				}
			}
		}
	}
	return v6
}

func (s *server) handleNeighborhood(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, neighborhood(r.Context()))
}
