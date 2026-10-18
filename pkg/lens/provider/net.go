package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
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
}

// NewNet returns a new read-only Net provider backed by the standard library net package.
func NewNet() *Net {
	return &Net{
		Interfaces: net.Interfaces,
		InterfaceAddrs: func(ifi *net.Interface) ([]net.Addr, error) {
			return ifi.Addrs()
		},
		Routes: nil,
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
		strings.HasPrefix(t, "routes/") || strings.HasPrefix(t, "route/") ||
		strings.HasSuffix(t, "/routes") || strings.HasSuffix(t, "/route")
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

	if addrPart == "routes" || addrPart == "route" {
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
		if n.Routes != nil {
			nodes, rErr := n.Routes(ctx, loc)
			if rErr != nil {
				return Node{}, rErr
			}
			if len(nodes) == 0 {
				return Node{}, os.ErrNotExist
			}
			return nodes[0], nil
		}
		return Node{}, ErrRouteDetailsUnsupported
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
		if n.Routes != nil {
			return n.Routes(ctx, loc)
		}
		return nil, ErrRouteDetailsUnsupported

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
	if n.Routes != nil {
		return n.Routes(ctx, loc)
	}
	return nil, ErrRouteDetailsUnsupported
}
