package netid

import (
	"bufio"
	"context"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Snapshot reads the current network attachment.
//
// This runs on startup and on routing-table changes, never per sample, so the
// two subprocesses it uses (arp, networksetup) are not on any hot path. The
// default route comes from the routing socket directly since that is both
// cheap and the thing we are reacting to anyway.
func Snapshot(ctx context.Context) Network {
	iface, gw := defaultRoute()
	if iface == "" {
		return Network{}
	}
	n := Network{
		Iface:     iface,
		Kind:      kindOf(iface),
		GatewayIP: gw,
		CIDR:      cidrFor(iface),
	}
	if gw != "" {
		n.GatewayMAC = arpLookup(ctx, gw)
	}
	n.DHCPDomain = dhcpDomain(ctx, iface)
	return n
}

// defaultRoute returns the interface name and gateway address of the current
// default route, read from the kernel routing table.
func defaultRoute() (iface, gateway string) {
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return "", ""
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return "", ""
	}
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || len(rm.Addrs) <= unix.RTAX_GATEWAY {
			continue
		}
		// A default route has a wildcard destination.
		dst, ok := rm.Addrs[unix.RTAX_DST].(*route.Inet4Addr)
		if !ok || dst.IP != [4]byte{0, 0, 0, 0} {
			continue
		}
		gwAddr, ok := rm.Addrs[unix.RTAX_GATEWAY].(*route.Inet4Addr)
		if !ok {
			continue
		}
		ifi, err := net.InterfaceByIndex(rm.Index)
		if err != nil {
			continue
		}
		return ifi.Name, net.IP(gwAddr.IP[:]).String()
	}
	return "", ""
}

func arpLookup(ctx context.Context, ip string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/arp", "-n", ip).Output()
	if err != nil {
		return ""
	}
	// "? (192.168.1.254) at b8:6a:f1:58:ca:20 on en0 ifscope [ethernet]"
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "at" && i+1 < len(fields) {
			mac := fields[i+1]
			if _, err := net.ParseMAC(mac); err != nil {
				return ""
			}
			return mac
		}
	}
	return ""
}

func dhcpDomain(ctx context.Context, iface string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ipconfig", "getoption", iface, "domain_name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var (
	kindOnce sync.Once
	kindMap  map[string]Kind
)

// kindOf reports whether an interface is Wi-Fi or wired.
//
// The mapping only changes when hardware is added or removed, so it is read
// once. Knowing this matters because Wi-Fi and Ethernet fail in completely
// different ways, and a Wi-Fi link's problems are usually radio problems.
func kindOf(iface string) Kind {
	kindOnce.Do(func() {
		kindMap = hardwarePorts(context.Background())
	})
	if k, ok := kindMap[iface]; ok {
		return k
	}
	return KindOther
}

func hardwarePorts(ctx context.Context) map[string]Kind {
	m := map[string]Kind{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/networksetup", "-listallhardwareports").Output()
	if err != nil {
		return m
	}
	var port string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			port = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		case strings.HasPrefix(line, "Device:"):
			dev := strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
			if dev == "" {
				continue
			}
			switch {
			case strings.Contains(strings.ToLower(port), "wi-fi"),
				strings.Contains(strings.ToLower(port), "airport"):
				m[dev] = KindWiFi
			case strings.Contains(strings.ToLower(port), "ethernet"),
				strings.Contains(strings.ToLower(port), "lan"):
				m[dev] = KindEthernet
			default:
				m[dev] = KindOther
			}
		}
	}
	return m
}
