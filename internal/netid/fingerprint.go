// Package netid identifies which network the machine is attached to, so that
// samples taken on different networks are never averaged together.
package netid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

type Kind string

const (
	KindWiFi     Kind = "wifi"
	KindEthernet Kind = "ethernet"
	KindOther    Kind = "other"
)

// Network is a snapshot of the current attachment.
//
// Identity deliberately does not use SSID. macOS gates SSID behind Location
// Services (and `networksetup -getairportnetwork` is broken outright on macOS
// 26), so a background agent cannot read it without prompting. Gateway MAC is
// available with no permission at all and is a better key besides: it tells
// apart two APs sharing an SSID, survives a rename, and works on Ethernet.
type Network struct {
	Iface      string
	Kind       Kind
	GatewayIP  string
	GatewayMAC string
	CIDR       string
	DHCPDomain string
}

// Fingerprint is the stable identity used to key stored samples.
func (n Network) Fingerprint() string {
	parts := []string{
		string(n.Kind),
		n.GatewayIP,
		n.GatewayMAC,
		n.CIDR,
		n.DHCPDomain,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:16]
}

// Label is a human-readable description for the dashboard.
func (n Network) Label() string {
	if n.GatewayIP == "" {
		return "offline"
	}
	return fmt.Sprintf("%s via %s", n.Kind, n.GatewayIP)
}

func (n Network) Online() bool { return n.GatewayIP != "" }

// cidrFor returns the network prefix of the first IPv4 address on iface.
// The host portion is masked off so that a DHCP lease renewal handing out a
// different address on the same LAN does not read as a different network.
func cidrFor(ifaceName string) string {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil {
			continue
		}
		return (&net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}).String()
	}
	return ""
}
