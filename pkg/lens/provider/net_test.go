package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

func newTestNetProvider() *Net {
	hw1, _ := net.ParseMAC("00:11:22:33:44:55")
	hw2, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")

	mockInterfaces := []net.Interface{
		{
			Index:        1,
			MTU:          65536,
			Name:         "lo0",
			HardwareAddr: nil,
			Flags:        net.FlagUp | net.FlagLoopback,
		},
		{
			Index:        2,
			MTU:          1500,
			Name:         "eth0",
			HardwareAddr: hw1,
			Flags:        net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
		},
		{
			Index:        3,
			MTU:          1500,
			Name:         "eth1",
			HardwareAddr: hw2,
			Flags:        net.FlagBroadcast | net.FlagMulticast, // down
		},
	}

	mockAddrs := map[string][]net.Addr{
		"lo0": {
			&net.IPNet{
				IP:   net.ParseIP("127.0.0.1"),
				Mask: net.CIDRMask(8, 32),
			},
			&net.IPNet{
				IP:   net.ParseIP("::1"),
				Mask: net.CIDRMask(128, 128),
			},
		},
		"eth0": {
			&net.IPNet{
				IP:   net.ParseIP("192.168.1.10"),
				Mask: net.CIDRMask(24, 32),
			},
			&net.IPNet{
				IP:   net.ParseIP("fe80::1"),
				Mask: net.CIDRMask(64, 128),
			},
		},
		"eth1": {}, // no addresses
	}

	return &Net{
		Interfaces: func() ([]net.Interface, error) {
			return mockInterfaces, nil
		},
		InterfaceAddrs: func(ifi *net.Interface) ([]net.Addr, error) {
			return mockAddrs[ifi.Name], nil
		},
	}
}

func TestNetTreeHierarchy(t *testing.T) {
	ctx := context.Background()
	p := newTestNetProvider()

	// 1. Stat root container
	rootLoc := ref.Locator{Scheme: "net", Target: ""}
	rootNode, err := p.Stat(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if rootNode.SchemaVersion != NodeSchema || rootNode.Ref != "net:" || rootNode.Kind != "directory" || rootNode.Type != "net/tree" {
		t.Fatalf("unexpected rootNode: %+v", rootNode)
	}
	if rootNode.Children == nil || *rootNode.Children != 3 {
		t.Fatalf("root children count = %v, want 3", rootNode.Children)
	}

	// 2. List root -> returns interfaces (lo0, eth0, eth1) sorted by Index
	ifaces, err := p.List(ctx, rootLoc)
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if len(ifaces) != 3 {
		t.Fatalf("len(ifaces) = %d, want 3", len(ifaces))
	}
	if ifaces[0].Ref != "net:lo0" || ifaces[0].Name != "lo0" || ifaces[0].Kind != "interface" {
		t.Errorf("ifaces[0] = %+v, want net:lo0", ifaces[0])
	}
	if ifaces[0].Children == nil || *ifaces[0].Children != 2 {
		t.Errorf("ifaces[0].Children = %v, want 2", ifaces[0].Children)
	}
	if ifaces[1].Ref != "net:eth0" || ifaces[1].Name != "eth0" {
		t.Errorf("ifaces[1] = %+v, want net:eth0", ifaces[1])
	}
	if ifaces[1].Children == nil || *ifaces[1].Children != 2 {
		t.Errorf("ifaces[1].Children = %v, want 2", ifaces[1].Children)
	}
	if ifaces[2].Ref != "net:eth1" || ifaces[2].Children != nil {
		t.Errorf("ifaces[2] = %+v, want net:eth1 without children", ifaces[2])
	}

	// 3. Stat interface eth0
	eth0Loc := ref.Locator{Scheme: "net", Target: "eth0"}
	eth0Node, err := p.Stat(ctx, eth0Loc)
	if err != nil {
		t.Fatalf("Stat eth0: %v", err)
	}
	if eth0Node.Kind != "interface" || eth0Node.Type != "interface" {
		t.Errorf("eth0Node kind/type = %s/%s", eth0Node.Kind, eth0Node.Type)
	}
	if eth0Node.Attrs["index"] != 2 || eth0Node.Attrs["hardware_addr"] != "00:11:22:33:44:55" {
		t.Errorf("eth0Node attrs = %+v", eth0Node.Attrs)
	}
	if eth0Node.Attrs["up"] != true || eth0Node.Attrs["broadcast"] != true {
		t.Errorf("eth0Node flags = %+v", eth0Node.Attrs)
	}
	if eth0Node.Children == nil || *eth0Node.Children != 2 {
		t.Errorf("eth0Node.Children = %v, want 2", eth0Node.Children)
	}

	// 4. List children of eth0 -> returns address nodes
	addrs, err := p.List(ctx, eth0Loc)
	if err != nil {
		t.Fatalf("List eth0: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("len(addrs) = %d, want 2", len(addrs))
	}
	if addrs[0].Kind != "address" || addrs[0].Type != "address" {
		t.Errorf("addrs[0] kind/type = %s/%s", addrs[0].Kind, addrs[0].Type)
	}
	if addrs[0].Attrs["interface"] != "eth0" || addrs[0].Attrs["family"] != "ipv4" {
		t.Errorf("addrs[0] attrs = %+v", addrs[0].Attrs)
	}

	// 5. Stat specific address (IPv4)
	addrLoc := ref.Locator{Scheme: "net", Target: "eth0/192.168.1.10/24"}
	addrNode, err := p.Stat(ctx, addrLoc)
	if err != nil {
		t.Fatalf("Stat addr: %v", err)
	}
	if addrNode.Name != "192.168.1.10/24" || addrNode.Attrs["ip"] != "192.168.1.10" {
		t.Errorf("addrNode = %+v", addrNode)
	}
	if addrNode.Children != nil {
		t.Errorf("leaf address node should have nil children, got %v", addrNode.Children)
	}

	// Stat specific address without mask
	addrBareLoc := ref.Locator{Scheme: "net", Target: "eth0/192.168.1.10"}
	if _, err := p.Stat(ctx, addrBareLoc); err != nil {
		t.Fatalf("Stat addr bare IP: %v", err)
	}

	// Stat specific address by index
	addrIdxLoc := ref.Locator{Scheme: "net", Target: "eth0/0"}
	if _, err := p.Stat(ctx, addrIdxLoc); err != nil {
		t.Fatalf("Stat addr by index: %v", err)
	}

	// 6. Edges from root
	edges, err := p.Edges(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Edges root: %v", err)
	}
	if len(edges) != 3 {
		t.Fatalf("len(edges) = %d, want 3", len(edges))
	}
	if edges[0].SchemaVersion != EdgeSchema || edges[0].Src != "net:" || edges[0].Rel != "contains" || edges[0].Dst != "net:lo0" {
		t.Errorf("edges[0] = %+v", edges[0])
	}
	if edges[1].Dst != "net:eth0" || edges[2].Dst != "net:eth1" {
		t.Errorf("unexpected edge destinations: %+v", edges)
	}

	// 7. Edges from interface eth0
	ifEdges, err := p.Edges(ctx, eth0Loc)
	if err != nil {
		t.Fatalf("Edges eth0: %v", err)
	}
	if len(ifEdges) != 2 {
		t.Fatalf("len(ifEdges) = %d, want 2", len(ifEdges))
	}
	if ifEdges[0].Src != "net:eth0" || ifEdges[0].Rel != "contains" {
		t.Errorf("ifEdges[0] = %+v", ifEdges[0])
	}

	// 8. Open returns ErrUnsupported
	_, err = p.Open(ctx, rootLoc)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open root err = %v, want ErrUnsupported", err)
	}
	_, err = p.Open(ctx, eth0Loc)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open eth0 err = %v, want ErrUnsupported", err)
	}
}

func TestNetErrorsAndUnsupported(t *testing.T) {
	ctx := context.Background()
	p := newTestNetProvider()

	// Non-existent interface
	missingIf := ref.Locator{Scheme: "net", Target: "eth99"}
	if _, err := p.Stat(ctx, missingIf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat missing interface err = %v, want os.ErrNotExist", err)
	}
	if _, err := p.List(ctx, missingIf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("List missing interface err = %v, want os.ErrNotExist", err)
	}

	// Non-existent address
	missingAddr := ref.Locator{Scheme: "net", Target: "eth0/10.0.0.1"}
	if _, err := p.Stat(ctx, missingAddr); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat missing addr err = %v, want os.ErrNotExist", err)
	}

	// Route details unsupported
	routeLocs := []ref.Locator{
		{Scheme: "net", Target: "routes"},
		{Scheme: "net", Target: "route"},
		{Scheme: "net", Target: "eth0/routes"},
		{Scheme: "net", Target: "eth0/route"},
	}
	for _, rLoc := range routeLocs {
		if _, err := p.Stat(ctx, rLoc); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Stat %s err = %v, want ErrUnsupported", rLoc.Target, err)
		}
		if _, err := p.List(ctx, rLoc); !errors.Is(err, ErrUnsupported) {
			t.Errorf("List %s err = %v, want ErrUnsupported", rLoc.Target, err)
		}
		if _, err := p.Edges(ctx, rLoc); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Edges %s err = %v, want ErrUnsupported", rLoc.Target, err)
		}
	}

	// RouteDetails method
	if _, err := p.RouteDetails(ctx, ref.Locator{Scheme: "net", Target: "routes"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("RouteDetails err = %v, want ErrUnsupported", err)
	}

	// Wrong scheme
	wrongScheme := ref.Locator{Scheme: "proc", Target: "eth0"}
	if _, err := p.Stat(ctx, wrongScheme); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat wrong scheme err = %v, want ErrUnsupported", err)
	}

	// Unsupported inner descent
	inner := ref.Locator{Scheme: "net", Target: "eth0", Inner: []string{"sub"}}
	if _, err := p.Stat(ctx, inner); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat inner err = %v, want ErrUnsupported", err)
	}

	// Unsupported location scope
	scoped := ref.Locator{Scheme: "net", Target: "eth0", ScopeKind: "host", ScopeValue: "remote"}
	if _, err := p.Stat(ctx, scoped); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat scoped err = %v, want ErrUnsupported", err)
	}
}

func TestNetCustomRoutes(t *testing.T) {
	ctx := context.Background()
	p := newTestNetProvider()

	routeNode := Node{
		SchemaVersion: NodeSchema,
		Ref:           "net:routes/default",
		Path:          "routes/default",
		Name:          "default",
		Kind:          "route",
		Type:          "route",
		Attrs: map[string]any{
			"destination": "0.0.0.0/0",
			"gateway":     "192.168.1.1",
			"interface":   "eth0",
		},
	}

	p.Routes = func(ctx context.Context, loc ref.Locator) ([]Node, error) {
		return []Node{routeNode}, nil
	}

	routesLoc := ref.Locator{Scheme: "net", Target: "routes"}
	node, err := p.Stat(ctx, routesLoc)
	if err != nil {
		t.Fatalf("Stat routes with custom provider: %v", err)
	}
	if node.Kind != "route" || node.Attrs["gateway"] != "192.168.1.1" {
		t.Errorf("unexpected route node: %+v", node)
	}

	routes, err := p.List(ctx, routesLoc)
	if err != nil {
		t.Fatalf("List routes with custom provider: %v", err)
	}
	if len(routes) != 1 || routes[0].Ref != "net:routes/default" {
		t.Errorf("unexpected routes list: %+v", routes)
	}
}

func TestNetRegistryAndSerialization(t *testing.T) {
	r := NewRegistry()
	p := newTestNetProvider()
	if err := r.Register("net", p); err != nil {
		t.Fatalf("Register net: %v", err)
	}

	lookup, ok := r.Lookup("net")
	if !ok || lookup == nil {
		t.Fatal("Lookup net failed")
	}

	loc, err := r.Parse("net:eth0")
	if err != nil {
		t.Fatalf("Parse net:eth0: %v", err)
	}

	node, err := lookup.Stat(context.Background(), loc)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	b, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["schema_version"] != NodeSchema || m["ref"] != "net:eth0" || m["attrs"] == nil {
		t.Fatalf("serialized row invalid: %s", b)
	}
}

func TestNetLiveHost(t *testing.T) {
	p := NewNet()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rootLoc := ref.Locator{Scheme: "net", Target: ""}
	ifaces, err := p.List(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Live host List net: %v", err)
	}
	if len(ifaces) == 0 {
		t.Fatal("Live host returned 0 network interfaces")
	}

	statNode, err := p.Stat(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Live host Stat net: %v", err)
	}
	if statNode.Children == nil || *statNode.Children != uint64(len(ifaces)) {
		t.Errorf("statNode.Children = %v, len(ifaces) = %d", statNode.Children, len(ifaces))
	}

	// Stat first interface
	firstIf := ifaces[0]
	ifLoc := ref.Locator{Scheme: "net", Target: firstIf.Name}
	ifNode, err := p.Stat(ctx, ifLoc)
	if err != nil {
		t.Fatalf("Live host Stat %s: %v", firstIf.Name, err)
	}
	if ifNode.Kind != "interface" {
		t.Errorf("ifNode.Kind = %s, want interface", ifNode.Kind)
	}

	// Open returns ErrUnsupported
	if _, err := p.Open(ctx, ifLoc); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open on live host err = %v, want ErrUnsupported", err)
	}

	// Routes: live host routes or graceful ErrUnsupported
	routeNode, err := p.Stat(ctx, ref.Locator{Scheme: "net", Target: "routes"})
	if err == nil {
		if routeNode.Kind != "route" {
			t.Errorf("Stat routes kind = %s, want route", routeNode.Kind)
		}
		routes, err := p.List(ctx, ref.Locator{Scheme: "net", Target: "routes"})
		if err != nil {
			t.Fatalf("List routes: %v", err)
		}
		if len(routes) == 0 {
			t.Logf("List routes returned 0 routes")
		}
		edges, err := p.Edges(ctx, ref.Locator{Scheme: "net", Target: "routes"})
		if err != nil {
			t.Fatalf("Edges routes: %v", err)
		}
		if len(edges) != len(routes) {
			t.Errorf("Edges routes len = %d, want %d", len(edges), len(routes))
		}
	} else if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat routes on live host err = %v, want nil or ErrUnsupported", err)
	}
}

const fixtureLinuxIPRoute = `default via 192.168.1.1 dev eth0 proto dhcp metric 100
10.0.0.0/8 via 10.0.0.1 dev eth1 proto static
192.168.1.0/24 dev eth0 proto kernel scope link src 192.168.1.10 metric 100
169.254.0.0/16 dev eth0 scope link metric 1000
fe80::/64 dev eth0 proto kernel metric 256 pref medium
default via fe80::1 dev eth0 proto ra metric 1024 pref medium`

const fixtureLinuxNetstat = `Kernel IP routing table
Destination     Gateway         Genmask         Flags   MSS Window  irtt Iface
0.0.0.0         192.168.1.1     0.0.0.0         UG        0 0          0 eth0
10.0.0.0        10.0.0.1        255.0.0.0       UG        0 0          0 eth1
192.168.1.0     0.0.0.0         255.255.255.0   U         0 0          0 eth0`

const fixtureDarwinNetstat = `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            192.168.1.1        UGScg                 en0
10.0.0/24          link#15            UCS                   en0      !
127.0.0.1          127.0.0.1          UH                    lo0

Internet6:
Destination        Gateway            Flags               Netif Expire
default            fe80::%utun0       UGcIg               utun0
::1                ::1                UHL                   lo0
fe80::%lo0/64      fe80::1%lo0        UcI                   lo0       `

func TestNetPlatformRoutesLinuxIPRoute(t *testing.T) {
	nodes, err := ParseIPRoute(fixtureLinuxIPRoute)
	if err != nil {
		t.Fatalf("ParseIPRoute: %v", err)
	}
	if len(nodes) != 6 {
		t.Fatalf("len(nodes) = %d, want 6", len(nodes))
	}

	// Verify deterministic sorting and fields
	var defaultIPv4, defaultIPv6 *Node
	for i := range nodes {
		if nodes[i].Kind != "route" || nodes[i].Type != "route" {
			t.Errorf("node[%d] Kind/Type = %s/%s, want route/route", i, nodes[i].Kind, nodes[i].Type)
		}
		if nodes[i].Attrs["destination"] == "default" {
			if nodes[i].Attrs["family"] == "ipv4" {
				defaultIPv4 = &nodes[i]
			} else if nodes[i].Attrs["family"] == "ipv6" {
				defaultIPv6 = &nodes[i]
			}
		}
	}

	if defaultIPv4 == nil {
		t.Fatal("missing default IPv4 route")
	}
	if defaultIPv4.Attrs["gateway"] != "192.168.1.1" || defaultIPv4.Attrs["interface"] != "eth0" {
		t.Errorf("defaultIPv4 attrs = %+v", defaultIPv4.Attrs)
	}
	if defaultIPv4.Attrs["metric"] != 100 {
		t.Errorf("defaultIPv4 metric = %v, want 100", defaultIPv4.Attrs["metric"])
	}

	if defaultIPv6 == nil {
		t.Fatal("missing default IPv6 route")
	}
	if defaultIPv6.Attrs["gateway"] != "fe80::1" || defaultIPv6.Attrs["interface"] != "eth0" {
		t.Errorf("defaultIPv6 attrs = %+v", defaultIPv6.Attrs)
	}
	if defaultIPv6.Attrs["metric"] != 1024 {
		t.Errorf("defaultIPv6 metric = %v, want 1024", defaultIPv6.Attrs["metric"])
	}

	// Also verify ParseRoutes auto-detects it
	autoNodes, err := ParseRoutes(fixtureLinuxIPRoute)
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	if len(autoNodes) != len(nodes) {
		t.Errorf("autoNodes len = %d, want %d", len(autoNodes), len(nodes))
	}
}

func TestNetPlatformRoutesLinuxNetstat(t *testing.T) {
	nodes, err := ParseNetstatLinux(fixtureLinuxNetstat)
	if err != nil {
		t.Fatalf("ParseNetstatLinux: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("len(nodes) = %d, want 3", len(nodes))
	}

	var defaultRoute *Node
	for i := range nodes {
		if nodes[i].Attrs["destination"] == "default" {
			defaultRoute = &nodes[i]
			break
		}
	}
	if defaultRoute == nil {
		t.Fatal("missing default route in Linux netstat output")
	}
	if defaultRoute.Attrs["gateway"] != "192.168.1.1" || defaultRoute.Attrs["flags"] != "UG" || defaultRoute.Attrs["interface"] != "eth0" {
		t.Errorf("defaultRoute attrs = %+v", defaultRoute.Attrs)
	}

	// Auto-detection
	autoNodes, err := ParseRoutes(fixtureLinuxNetstat)
	if err != nil {
		t.Fatalf("ParseRoutes auto: %v", err)
	}
	if len(autoNodes) != 3 {
		t.Errorf("len(autoNodes) = %d, want 3", len(autoNodes))
	}
}

func TestNetPlatformRoutesDarwinNetstat(t *testing.T) {
	nodes, err := ParseNetstatDarwin(fixtureDarwinNetstat)
	if err != nil {
		t.Fatalf("ParseNetstatDarwin: %v", err)
	}
	if len(nodes) != 6 {
		t.Fatalf("len(nodes) = %d, want 6", len(nodes))
	}

	var foundDefaultV4, foundDefaultV6 bool
	for _, n := range nodes {
		if n.Attrs["destination"] == "default" {
			if n.Attrs["interface"] == "en0" && n.Attrs["family"] == "ipv4" {
				foundDefaultV4 = true
				if n.Attrs["gateway"] != "192.168.1.1" || n.Attrs["flags"] != "UGScg" {
					t.Errorf("en0 default attrs = %+v", n.Attrs)
				}
			}
			if n.Attrs["interface"] == "utun0" && n.Attrs["family"] == "ipv6" {
				foundDefaultV6 = true
				if n.Attrs["gateway"] != "fe80::%utun0" || n.Attrs["flags"] != "UGcIg" {
					t.Errorf("utun0 default attrs = %+v", n.Attrs)
				}
			}
		}
	}
	if !foundDefaultV4 {
		t.Error("missing Darwin IPv4 default route")
	}
	if !foundDefaultV6 {
		t.Error("missing Darwin IPv6 default route")
	}

	// Auto-detection
	autoNodes, err := ParseRoutes(fixtureDarwinNetstat)
	if err != nil {
		t.Fatalf("ParseRoutes Darwin auto: %v", err)
	}
	if len(autoNodes) != 6 {
		t.Errorf("len(autoNodes) = %d, want 6", len(autoNodes))
	}
}

func TestNetRouteEdgesAndInterfaceScoping(t *testing.T) {
	ctx := context.Background()
	p := newTestNetProvider()
	p.RouteRunner = func(ctx context.Context) (string, error) {
		return fixtureLinuxIPRoute, nil
	}

	// 1. List all routes
	routesLoc := ref.Locator{Scheme: "net", Target: "routes"}
	allRoutes, err := p.List(ctx, routesLoc)
	if err != nil {
		t.Fatalf("List routes: %v", err)
	}
	if len(allRoutes) != 6 {
		t.Fatalf("len(allRoutes) = %d, want 6", len(allRoutes))
	}

	// 2. Edges from net:routes
	routeEdges, err := p.Edges(ctx, routesLoc)
	if err != nil {
		t.Fatalf("Edges routes: %v", err)
	}
	if len(routeEdges) != 6 {
		t.Fatalf("len(routeEdges) = %d, want 6", len(routeEdges))
	}
	for i, e := range routeEdges {
		if e.SchemaVersion != EdgeSchema || e.Src != "net:routes" || e.Rel != "contains" || e.Dst != allRoutes[i].Ref {
			t.Errorf("routeEdges[%d] = %+v, want Src=net:routes Dst=%s", i, e, allRoutes[i].Ref)
		}
	}

	// 3. Interface scoping: eth1/routes
	eth1RoutesLoc := ref.Locator{Scheme: "net", Target: "eth1/routes"}
	eth1Routes, err := p.List(ctx, eth1RoutesLoc)
	if err != nil {
		t.Fatalf("List eth1/routes: %v", err)
	}
	if len(eth1Routes) != 1 {
		t.Fatalf("len(eth1Routes) = %d, want 1", len(eth1Routes))
	}
	if eth1Routes[0].Attrs["destination"] != "10.0.0.0/8" || eth1Routes[0].Attrs["interface"] != "eth1" {
		t.Errorf("unexpected eth1 route: %+v", eth1Routes[0])
	}

	// Edges from eth1/routes
	eth1Edges, err := p.Edges(ctx, eth1RoutesLoc)
	if err != nil {
		t.Fatalf("Edges eth1/routes: %v", err)
	}
	if len(eth1Edges) != 1 || eth1Edges[0].Src != "net:eth1/routes" || eth1Edges[0].Dst != eth1Routes[0].Ref {
		t.Errorf("unexpected eth1Edges: %+v", eth1Edges)
	}

	// 4. Interface scoping: eth0/routes
	eth0RoutesLoc := ref.Locator{Scheme: "net", Target: "eth0/routes"}
	eth0Routes, err := p.List(ctx, eth0RoutesLoc)
	if err != nil {
		t.Fatalf("List eth0/routes: %v", err)
	}
	if len(eth0Routes) != 5 {
		t.Fatalf("len(eth0Routes) = %d, want 5", len(eth0Routes))
	}

	// 5. Stat specific route
	statSpecific, err := p.Stat(ctx, ref.Locator{Scheme: "net", Target: "eth0/routes/192.168.1.0/24"})
	if err != nil {
		t.Fatalf("Stat specific route: %v", err)
	}
	if statSpecific.Attrs["destination"] != "192.168.1.0/24" {
		t.Errorf("statSpecific destination = %v", statSpecific.Attrs["destination"])
	}

	// Specific route leaf List returns empty
	leafList, err := p.List(ctx, ref.Locator{Scheme: "net", Target: "eth0/routes/192.168.1.0/24"})
	if err != nil {
		t.Fatalf("List specific route leaf: %v", err)
	}
	if len(leafList) != 0 {
		t.Errorf("leafList len = %d, want 0", len(leafList))
	}

	// Specific route on wrong interface returns os.ErrNotExist
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "net", Target: "eth1/routes/192.168.1.0/24"}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat route on wrong interface err = %v, want os.ErrNotExist", err)
	}

	// RouteDetails
	detailNodes, err := p.RouteDetails(ctx, routesLoc)
	if err != nil {
		t.Fatalf("RouteDetails: %v", err)
	}
	if len(detailNodes) != 6 {
		t.Errorf("len(detailNodes) = %d, want 6", len(detailNodes))
	}
}

func TestNetRouteErrorsAndGracefulFallback(t *testing.T) {
	ctx := context.Background()

	// 1. RouteRunner error
	pErr := newTestNetProvider()
	pErr.RouteRunner = func(ctx context.Context) (string, error) {
		return "", errors.New("exec: command not found")
	}
	if _, err := pErr.Stat(ctx, ref.Locator{Scheme: "net", Target: "routes"}); err == nil {
		t.Fatal("Stat with failing RouteRunner expected error")
	}

	// 2. Context canceled
	pCanceled := newTestNetProvider()
	pCanceled.RouteRunner = func(ctx context.Context) (string, error) {
		return fixtureLinuxIPRoute, nil
	}
	cancCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pCanceled.Stat(cancCtx, ref.Locator{Scheme: "net", Target: "routes"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Stat canceled context err = %v, want context.Canceled", err)
	}

	// 3. Empty route output
	pEmpty := newTestNetProvider()
	pEmpty.RouteRunner = func(ctx context.Context) (string, error) {
		return "   \n\n  ", nil
	}
	if _, err := pEmpty.Stat(ctx, ref.Locator{Scheme: "net", Target: "routes"}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat empty routes err = %v, want os.ErrNotExist", err)
	}
	emptyList, err := pEmpty.List(ctx, ref.Locator{Scheme: "net", Target: "routes"})
	if err != nil {
		t.Fatalf("List empty routes: %v", err)
	}
	if len(emptyList) != 0 {
		t.Errorf("len(emptyList) = %d, want 0", len(emptyList))
	}

	// 4. Non-existent route
	pOk := newTestNetProvider()
	pOk.RouteRunner = func(ctx context.Context) (string, error) {
		return fixtureLinuxIPRoute, nil
	}
	if _, err := pOk.Stat(ctx, ref.Locator{Scheme: "net", Target: "routes/99.99.99.99"}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat non-existent route err = %v, want os.ErrNotExist", err)
	}
}
