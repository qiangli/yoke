package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

var _ Provider = (*Net)(nil)

// ErrRouteDetailsUnsupported is returned when route details are requested without platform API support.
var ErrRouteDetailsUnsupported = fmt.Errorf("provider: route details: %w", ErrUnsupported)

// Net exposes host network interfaces and addresses as a read-only hierarchical tree.
type Net struct {
	Interfaces     func() ([]net.Interface, error)
	InterfaceAddrs func(ifi *net.Interface) ([]net.Addr, error)
	Routes         func(ctx context.Context, loc ref.Locator) ([]Node, error)
	RouteRunner    func(ctx context.Context) (string, error)
}

// NewNet returns a new read-only Net provider backed by the standard library net package.
func NewNet() *Net {
	return &Net{
		Interfaces: net.Interfaces,
		InterfaceAddrs: func(ifi *net.Interface) ([]net.Addr, error) {
			return ifi.Addrs()
		},
		Routes:      nil,
		RouteRunner: defaultRouteRunner,
	}
}

func (n *Net) fetchInterfaces() ([]net.Interface, error) {
	if n.Interfaces != nil {
		return n.Interfaces()
	}
	return net.Interfaces()
}

func (n *Net) fetchAddrs(ifi *net.Interface) ([]net.Addr, error) {
	if n.InterfaceAddrs != nil {
		return n.InterfaceAddrs(ifi)
	}
	return ifi.Addrs()
}

func isRouteTarget(t string) bool {
	t = strings.TrimPrefix(t, "interfaces/")
	return t == "routes" || t == "route" ||
		strings.HasPrefix(t, "routes/") || strings.HasPrefix(t, "route/")
}

func (n *Net) rootNode(loc ref.Locator, count uint64) Node {
	refStr := loc.String()
	if refStr == "" || refStr == "net:." || refStr == "net:interfaces" {
		refStr = "net:"
	}
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           refStr,
		Path:          "",
		Name:          "net",
		Kind:          "directory",
		Type:          "net/tree",
		Children:      &count,
		Attrs:         map[string]any{},
	}
}

func (n *Net) interfaceNode(ifi net.Interface, childCount uint64) Node {
	attrs := map[string]any{
		"index":         ifi.Index,
		"mtu":           ifi.MTU,
		"name":          ifi.Name,
		"hardware_addr": ifi.HardwareAddr.String(),
		"flags":         ifi.Flags.String(),
		"up":            (ifi.Flags & net.FlagUp) != 0,
		"broadcast":     (ifi.Flags & net.FlagBroadcast) != 0,
		"loopback":      (ifi.Flags & net.FlagLoopback) != 0,
		"pointtopoint":  (ifi.Flags & net.FlagPointToPoint) != 0,
		"multicast":     (ifi.Flags & net.FlagMulticast) != 0,
	}

	node := Node{
		SchemaVersion: NodeSchema,
		Ref:           "net:" + ifi.Name,
		Path:          ifi.Name,
		Name:          ifi.Name,
		Kind:          "interface",
		Type:          "interface",
		Attrs:         attrs,
	}
	if childCount > 0 {
		node.Children = &childCount
	}
	return node
}

func (n *Net) addressNode(ifi net.Interface, addr net.Addr) Node {
	addrStr := addr.String()
	refStr := ref.Locator{Scheme: "net", Target: ifi.Name + "/" + addrStr}.String()
	pathStr := ifi.Name + "/" + addrStr

	attrs := map[string]any{
		"interface": ifi.Name,
		"address":   addrStr,
		"network":   addr.Network(),
	}

	var ip net.IP
	var ipNet *net.IPNet
	switch a := addr.(type) {
	case *net.IPNet:
		ip = a.IP
		ipNet = a
	case *net.IPAddr:
		ip = a.IP
	}

	family := ""
	if ip != nil {
		attrs["ip"] = ip.String()
		if ip.To4() != nil {
			family = "ipv4"
		} else {
			family = "ipv6"
		}
		attrs["family"] = family
		if ip.IsLoopback() {
			attrs["loopback"] = true
		}
		if ip.IsMulticast() {
			attrs["multicast"] = true
		}
		if ip.IsGlobalUnicast() {
			attrs["global_unicast"] = true
		}
		if ip.IsLinkLocalUnicast() {
			attrs["link_local_unicast"] = true
		}
	}
	if ipNet != nil {
		if ones, bits := ipNet.Mask.Size(); bits != 0 {
			attrs["mask_bits"] = ones
			attrs["mask"] = net.IP(ipNet.Mask).String()
		}
	}

	return Node{
		SchemaVersion: NodeSchema,
		Ref:           refStr,
		Path:          pathStr,
		Name:          addrStr,
		Kind:          "address",
		Type:          "address",
		Attrs:         attrs,
	}
}

type targetKind int

const (
	targetRoot targetKind = iota
	targetInterface
	targetAddress
	targetRoute
)

func (n *Net) resolveTarget(loc ref.Locator, interfaces []net.Interface) (
	kind targetKind,
	matchedIfi *net.Interface,
	matchedAddr net.Addr,
	err error,
) {
	if loc.Scheme != "net" || len(loc.Inner) != 0 || loc.ScopeKind != "" {
		return targetRoot, nil, nil, ErrUnsupported
	}

	t := strings.TrimPrefix(loc.Target, "/")
	t = strings.TrimSuffix(t, "/")

	if isRouteTarget(t) {
		return targetRoute, nil, nil, nil
	}

	if t == "" || t == "." {
		return targetRoot, nil, nil, nil
	}

	// If target is "interfaces", check if an actual interface has that name.
	if t == "interfaces" {
		for i := range interfaces {
			if interfaces[i].Name == "interfaces" {
				return targetInterface, &interfaces[i], nil, nil
			}
		}
		return targetRoot, nil, nil, nil
	}

	// Strip optional "interfaces/" prefix if not part of an actual interface name.
	if strings.HasPrefix(t, "interfaces/") {
		hasLiteral := false
		for i := range interfaces {
			if interfaces[i].Name == t {
				hasLiteral = true
				matchedIfi = &interfaces[i]
				break
			}
		}
		if hasLiteral {
			return targetInterface, matchedIfi, nil, nil
		}
		t = strings.TrimPrefix(t, "interfaces/")
	}

	ifiPart, addrPart, hasAddr := strings.Cut(t, "/")

	for i := range interfaces {
		if interfaces[i].Name == ifiPart {
			matchedIfi = &interfaces[i]
			break
		}
	}
	if matchedIfi == nil {
		if idx, convErr := strconv.Atoi(ifiPart); convErr == nil {
			for i := range interfaces {
				if interfaces[i].Index == idx {
					matchedIfi = &interfaces[i]
					break
				}
			}
		}
	}

	if matchedIfi == nil {
		return targetRoot, nil, nil, fmt.Errorf("provider: interface %q: %w", ifiPart, os.ErrNotExist)
	}

	if !hasAddr {
		return targetInterface, matchedIfi, nil, nil
	}

	if addrPart == "routes" || addrPart == "route" || strings.HasPrefix(addrPart, "routes/") || strings.HasPrefix(addrPart, "route/") {
		return targetRoute, matchedIfi, nil, nil
	}

	addrs, err := n.fetchAddrs(matchedIfi)
	if err != nil {
		return targetRoot, nil, nil, err
	}

	for _, a := range addrs {
		if a.String() == addrPart || strings.Split(a.String(), "/")[0] == addrPart {
			matchedAddr = a
			break
		}
	}
	if matchedAddr == nil {
		if idx, convErr := strconv.Atoi(addrPart); convErr == nil && idx >= 0 && idx < len(addrs) {
			matchedAddr = addrs[idx]
		}
	}

	if matchedAddr == nil {
		return targetRoot, nil, nil, fmt.Errorf("provider: address %q on %s: %w", addrPart, matchedIfi.Name, os.ErrNotExist)
	}

	return targetAddress, matchedIfi, matchedAddr, nil
}

// Stat returns metadata for root (net:), an interface (net:<if>), or an address (net:<if>/<addr>).
func (n *Net) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}

	interfaces, err := n.fetchInterfaces()
	if err != nil {
		return Node{}, err
	}

	kind, ifi, addr, err := n.resolveTarget(loc, interfaces)
	if err != nil {
		return Node{}, err
	}

	switch kind {
	case targetRoot:
		return n.rootNode(loc, uint64(len(interfaces))), nil
	case targetInterface:
		addrs, _ := n.fetchAddrs(ifi)
		return n.interfaceNode(*ifi, uint64(len(addrs))), nil
	case targetAddress:
		return n.addressNode(*ifi, addr), nil
	case targetRoute:
		nodes, rErr := n.fetchRouteNodes(ctx, loc, ifi)
		if rErr != nil {
			return Node{}, rErr
		}
		if len(nodes) == 0 {
			return Node{}, os.ErrNotExist
		}
		return nodes[0], nil
	default:
		return Node{}, ErrUnsupported
	}
}

// List returns direct child rows of root (interfaces) or an interface (addresses).
func (n *Net) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	interfaces, err := n.fetchInterfaces()
	if err != nil {
		return nil, err
	}

	kind, ifi, _, err := n.resolveTarget(loc, interfaces)
	if err != nil {
		return nil, err
	}

	switch kind {
	case targetRoot:
		sortedIfis := append([]net.Interface(nil), interfaces...)
		sort.Slice(sortedIfis, func(i, j int) bool {
			if sortedIfis[i].Index != sortedIfis[j].Index {
				return sortedIfis[i].Index < sortedIfis[j].Index
			}
			return sortedIfis[i].Name < sortedIfis[j].Name
		})

		nodes := make([]Node, 0, len(sortedIfis))
		for _, iface := range sortedIfis {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			addrs, _ := n.fetchAddrs(&iface)
			nodes = append(nodes, n.interfaceNode(iface, uint64(len(addrs))))
		}
		return nodes, nil

	case targetInterface:
		addrs, err := n.fetchAddrs(ifi)
		if err != nil {
			return nil, err
		}
		sortedAddrs := append([]net.Addr(nil), addrs...)
		sort.Slice(sortedAddrs, func(i, j int) bool {
			return sortedAddrs[i].String() < sortedAddrs[j].String()
		})

		nodes := make([]Node, 0, len(sortedAddrs))
		for _, a := range sortedAddrs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nodes = append(nodes, n.addressNode(*ifi, a))
		}
		return nodes, nil

	case targetAddress:
		return []Node{}, nil

	case targetRoute:
		if isRouteLeaf(loc.Target, ifi) {
			nodes, rErr := n.fetchRouteNodes(ctx, loc, ifi)
			if rErr != nil {
				return nil, rErr
			}
			if len(nodes) == 0 {
				return nil, os.ErrNotExist
			}
			return []Node{}, nil
		}
		return n.fetchRouteNodes(ctx, loc, ifi)

	default:
		return nil, ErrUnsupported
	}
}

// Edges returns contains edges from loc to each of its direct children.
func (n *Net) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := n.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	edges := make([]Edge, 0, len(children))
	for _, child := range children {
		edges = append(edges, Edge{
			SchemaVersion: EdgeSchema,
			Src:           loc.String(),
			Rel:           "contains",
			Dst:           child.Ref,
			Attrs:         map[string]any{},
		})
	}
	return edges, nil
}

// Open is unsupported for network interfaces.
func (n *Net) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}

// RouteDetails returns route rows, or ErrRouteDetailsUnsupported if platform APIs are unavailable.
func (n *Net) RouteDetails(ctx context.Context, loc ref.Locator) ([]Node, error) {
	return n.fetchRouteNodes(ctx, loc, nil)
}

func (n *Net) fetchRouteNodes(ctx context.Context, loc ref.Locator, ifi *net.Interface) ([]Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n.Routes != nil {
		return n.Routes(ctx, loc)
	}
	if n.RouteRunner == nil {
		return nil, ErrRouteDetailsUnsupported
	}
	raw, err := n.RouteRunner(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := ParseRoutes(raw)
	if err != nil {
		return nil, err
	}
	return filterRouteNodes(nodes, loc, ifi), nil
}

func isRouteLeaf(target string, ifi *net.Interface) bool {
	t := strings.TrimPrefix(target, "/")
	t = strings.TrimPrefix(t, "interfaces/")
	if ifi != nil {
		t = strings.TrimPrefix(t, ifi.Name+"/")
	}
	t = strings.TrimPrefix(t, "routes/")
	t = strings.TrimPrefix(t, "route/")
	return t != "routes" && t != "route" && t != ""
}

func filterRouteNodes(nodes []Node, loc ref.Locator, ifi *net.Interface) []Node {
	t := strings.TrimPrefix(loc.Target, "/")
	t = strings.TrimPrefix(t, "interfaces/")
	if ifi != nil {
		t = strings.TrimPrefix(t, ifi.Name+"/")
	}
	t = strings.TrimPrefix(t, "routes/")
	t = strings.TrimPrefix(t, "route/")

	var filtered []Node
	for _, node := range nodes {
		if ifi != nil {
			if node.Attrs["interface"] != ifi.Name {
				continue
			}
		}
		if t != "" && t != "routes" && t != "route" {
			dest := fmt.Sprint(node.Attrs["destination"])
			if node.Name != t && dest != t {
				if t == "default" && (dest == "0.0.0.0/0" || dest == "default") {
					// match default
				} else if strings.HasPrefix(node.Name, t+"-") {
					// match disambiguated name
				} else {
					continue
				}
			}
		}
		filtered = append(filtered, node)
	}
	return filtered
}

type rawRoute struct {
	Destination string
	Gateway     string
	Interface   string
	Family      string
	Flags       string
	Metric      *int
	Scope       string
	Source      string
	Genmask     string
	Proto       string
}

func buildRouteNodes(routes []rawRoute) []Node {
	destCount := make(map[string]int, len(routes))
	for _, r := range routes {
		destCount[r.Destination]++
	}

	nodes := make([]Node, 0, len(routes))
	usedNames := make(map[string]int, len(routes))

	for _, r := range routes {
		base := r.Destination
		if destCount[r.Destination] > 1 && r.Interface != "" {
			base = r.Destination + "-" + r.Interface
		}
		name := base
		if count := usedNames[base]; count > 0 {
			name = fmt.Sprintf("%s-%d", base, count+1)
		}
		usedNames[base]++

		attrs := map[string]any{
			"destination": r.Destination,
			"gateway":     r.Gateway,
			"interface":   r.Interface,
		}
		if r.Family != "" {
			attrs["family"] = r.Family
		}
		if r.Flags != "" {
			attrs["flags"] = r.Flags
		}
		if r.Metric != nil {
			attrs["metric"] = *r.Metric
		}
		if r.Scope != "" {
			attrs["scope"] = r.Scope
		}
		if r.Source != "" {
			attrs["source"] = r.Source
		}
		if r.Genmask != "" {
			attrs["genmask"] = r.Genmask
		}
		if r.Proto != "" {
			attrs["proto"] = r.Proto
		}

		nodes = append(nodes, Node{
			SchemaVersion: NodeSchema,
			Ref:           "net:routes/" + name,
			Path:          "routes/" + name,
			Name:          name,
			Kind:          "route",
			Type:          "route",
			Attrs:         attrs,
		})
	}

	sort.Slice(nodes, func(i, j int) bool {
		di := fmt.Sprint(nodes[i].Attrs["destination"])
		dj := fmt.Sprint(nodes[j].Attrs["destination"])
		if di != dj {
			return di < dj
		}
		ii := fmt.Sprint(nodes[i].Attrs["interface"])
		ij := fmt.Sprint(nodes[j].Attrs["interface"])
		if ii != ij {
			return ii < ij
		}
		gi := fmt.Sprint(nodes[i].Attrs["gateway"])
		gj := fmt.Sprint(nodes[j].Attrs["gateway"])
		if gi != gj {
			return gi < gj
		}
		return nodes[i].Name < nodes[j].Name
	})

	return nodes
}

// ParseIPRoute parses Linux 'ip route show' or 'ip -6 route show' text output.
func ParseIPRoute(output string) ([]Node, error) {
	var routes []rawRoute
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens := strings.Fields(line)
		if len(tokens) == 0 {
			continue
		}

		switch tokens[0] {
		case "unicast", "local", "broadcast", "multicast", "throw", "unreachable", "prohibit", "blackhole", "nat":
			tokens = tokens[1:]
			if len(tokens) == 0 {
				continue
			}
		}

		dest := tokens[0]
		tokens = tokens[1:]

		var gw, iface, proto, scope, src string
		var metric *int

		for i := 0; i < len(tokens); i++ {
			switch tokens[i] {
			case "via":
				if i+1 < len(tokens) {
					gw = tokens[i+1]
					i++
				}
			case "dev":
				if i+1 < len(tokens) {
					iface = tokens[i+1]
					i++
				}
			case "proto":
				if i+1 < len(tokens) {
					proto = tokens[i+1]
					i++
				}
			case "scope":
				if i+1 < len(tokens) {
					scope = tokens[i+1]
					i++
				}
			case "src":
				if i+1 < len(tokens) {
					src = tokens[i+1]
					i++
				}
			case "metric":
				if i+1 < len(tokens) {
					if m, err := strconv.Atoi(tokens[i+1]); err == nil {
						metric = &m
					}
					i++
				}
			}
		}

		family := "ipv4"
		if strings.Contains(dest, ":") || strings.Contains(gw, ":") {
			family = "ipv6"
		}

		routes = append(routes, rawRoute{
			Destination: dest,
			Gateway:     gw,
			Interface:   iface,
			Family:      family,
			Metric:      metric,
			Scope:       scope,
			Source:      src,
			Proto:       proto,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buildRouteNodes(routes), nil
}

// ParseNetstatDarwin parses macOS/BSD 'netstat -rn' text output.
func ParseNetstatDarwin(output string) ([]Node, error) {
	var routes []rawRoute
	scanner := bufio.NewScanner(strings.NewReader(output))
	currentFamily := "ipv4"

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Routing tables") {
			continue
		}
		if strings.HasPrefix(line, "Internet:") {
			currentFamily = "ipv4"
			continue
		}
		if strings.HasPrefix(line, "Internet6:") {
			currentFamily = "ipv6"
			continue
		}
		if strings.HasPrefix(line, "Destination") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		dest := fields[0]
		gw := fields[1]
		flags := fields[2]
		iface := fields[3]

		family := currentFamily
		if strings.Contains(dest, ":") || strings.Contains(gw, ":") {
			family = "ipv6"
		}

		routes = append(routes, rawRoute{
			Destination: dest,
			Gateway:     gw,
			Interface:   iface,
			Family:      family,
			Flags:       flags,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buildRouteNodes(routes), nil
}

// ParseNetstatLinux parses Linux 'netstat -rn' text output.
func ParseNetstatLinux(output string) ([]Node, error) {
	var routes []rawRoute
	scanner := bufio.NewScanner(strings.NewReader(output))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Kernel IP routing table") {
			continue
		}
		if strings.HasPrefix(line, "Destination") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}

		dest := fields[0]
		gw := fields[1]
		genmask := fields[2]
		flags := fields[3]
		iface := fields[7]

		if dest == "0.0.0.0" && genmask == "0.0.0.0" {
			dest = "default"
		}

		routes = append(routes, rawRoute{
			Destination: dest,
			Gateway:     gw,
			Interface:   iface,
			Family:      "ipv4",
			Flags:       flags,
			Genmask:     genmask,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buildRouteNodes(routes), nil
}

// ParseRoutes autodetects the route table format and parses it into Nodes.
func ParseRoutes(output string) ([]Node, error) {
	if strings.Contains(output, "Kernel IP routing table") || (strings.Contains(output, "Genmask") && strings.Contains(output, "Iface")) {
		return ParseNetstatLinux(output)
	}
	if strings.Contains(output, "Routing tables") || strings.Contains(output, "Internet:") || strings.Contains(output, "Internet6:") || strings.Contains(output, "Netif") {
		return ParseNetstatDarwin(output)
	}
	nodes, err := ParseIPRoute(output)
	if err == nil && len(nodes) > 0 {
		return nodes, nil
	}
	darwinNodes, dErr := ParseNetstatDarwin(output)
	if dErr == nil && len(darwinNodes) > 0 {
		return darwinNodes, nil
	}
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

func defaultRouteRunner(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var runnerErr error
	run := func(bin string, args ...string) (string, error) {
		path, err := exec.LookPath(bin)
		if err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, path, args...)
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
		return string(out), nil
	}

	if runtime.GOOS == "linux" {
		if out, err := run("ip", "route", "show"); err == nil {
			return out, nil
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		} else {
			runnerErr = err
		}
		if out, err := run("netstat", "-rn"); err == nil {
			return out, nil
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		}
	} else if runtime.GOOS == "darwin" || strings.HasSuffix(runtime.GOOS, "bsd") {
		if out, err := run("netstat", "-rn"); err == nil {
			return out, nil
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		} else {
			runnerErr = err
		}
	} else {
		if out, err := run("netstat", "-rn"); err == nil {
			return out, nil
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if out, err := run("ip", "route"); err == nil {
			return out, nil
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		} else {
			runnerErr = err
		}
	}

	if runnerErr != nil {
		return "", fmt.Errorf("%w: %v", ErrRouteDetailsUnsupported, runnerErr)
	}
	return "", ErrRouteDetailsUnsupported
}
