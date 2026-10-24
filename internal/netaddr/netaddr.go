// Package netaddr finds this machine's own LAN-reachable IPv4 addresses.
package netaddr

import "net"

// LocalIPv4 lists this machine's IPv4 addresses on up, non-loopback interfaces, skipping link-local (169.254/16).
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
