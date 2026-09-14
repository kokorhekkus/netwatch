package app

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/kokorhekkus/netwatch/internal/netid"
)

// Target is one thing we measure against.
type Target struct {
	ID    int64
	Kind  string
	Addr  string
	Label string
	IP    net.IP
}

// Fixed anycast resolvers. Two of them, from different operators, so that one
// provider's bad day is distinguishable from the connection being at fault.
var anycastTargets = []struct{ addr, label string }{
	{"1.1.1.1", "Cloudflare"},
	{"8.8.8.8", "Google"},
}

// buildTargets assembles the target set for the current network.
//
// The gateway is re-derived per network: a gateway address means nothing once
// you have moved to a different LAN. The ISP hop is discovered once and then
// pinned, via pinnedHop.
func buildTargets(ctx context.Context, n netid.Network, pinnedHop string) []Target {
	var out []Target

	if n.GatewayIP != "" {
		out = append(out, Target{
			Kind:  "gateway",
			Addr:  n.GatewayIP,
			Label: "Router",
			IP:    net.ParseIP(n.GatewayIP),
		})
	}

	hop := pinnedHop
	if hop == "" {
		hop = firstISPHop(ctx, n.GatewayIP)
	}
	if hop != "" {
		out = append(out, Target{
			Kind: "isp_hop",
			Addr: hop,
			// The address is part of the label so two hops can never be
			// confused for one another in a legend.
			Label: "ISP " + hop,
			IP:    net.ParseIP(hop),
		})
	}

	for _, a := range anycastTargets {
		out = append(out, Target{
			Kind:  "anycast",
			Addr:  a.addr,
			Label: a.label,
			IP:    net.ParseIP(a.addr),
		})
	}
	return out
}

// firstISPHop finds the first hop beyond the local gateway that answers.
//
// Splitting "my Wi-Fi and router" from "everything past my front door" is the
// single most useful attribution the tool makes, and it needs a target on the
// far side of the router. Hops that never answer are skipped rather than
// recorded as unreachable, because a silent hop is normal: many routers simply
// do not generate ICMP for transit traffic.
func firstISPHop(ctx context.Context, gateway string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "/usr/sbin/traceroute",
		"-n", "-w", "1", "-q", "1", "-m", "6", "1.1.1.1").Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := net.ParseIP(fields[1])
		if ip == nil || ip.To4() == nil {
			continue
		}
		addr := ip.String()
		if addr == gateway || isPrivate(ip) {
			continue
		}
		return addr
	}
	return ""
}

func isPrivate(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}
