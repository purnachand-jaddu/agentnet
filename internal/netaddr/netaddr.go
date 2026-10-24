// Package netaddr finds this machine's own LAN-reachable IPv4 addresses.
package netaddr

import (
	"net"
	"strings"
)

// virtualPrefixes are interface name prefixes LocalIPv4 skips: containers, VMs and VPNs rarely reach real LAN peers.
var virtualPrefixes = []string{"docker", "br-", "veth", "virbr", "vmnet", "utun", "tailscale", "zt"}

// isVirtual reports whether name looks like a container, VM or VPN interface rather than a real LAN NIC.
func isVirtual(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// LocalIPv4 lists this machine's IPv4 addresses on up, non-loopback, non-virtual interfaces, skipping link-local (169.254/16).
func LocalIPv4() ([]net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtual(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue // ponytail: skip interfaces we can't enumerate rather than fail entirely
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, ip4)
		}
	}
	return ips, nil
}
