package sso

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

var reservedPrefixes = func() []netip.Prefix {
	out := make([]netip.Prefix, 0, 11)

	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32", "fd00:ec2::/32",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}

	return out
}()

// blockedIP reports whether a connection to ip must be refused: loopback,
// private, link-local (the cloud metadata address included), CGNAT and other
// reserved ranges.
func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()

	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}

	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}

	return false
}

// guardDial runs after DNS resolution, on the address about to be dialed, so a
// name that resolves to an internal address (or is rebound to one) is refused
// too.
func guardDial(network, address string, _ syscall.RawConn) error {
	if allowPrivateNetworks() {
		return nil
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("refusing to dial %q", host)
	}

	if blockedIP(ip) {
		return fmt.Errorf("refusing to connect to the non-public address %s", ip)
	}

	return nil
}
