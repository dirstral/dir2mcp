// Package netutil holds small, dependency-free networking helpers shared across
// packages that cannot import one another (e.g. config and x402).
package netutil

import (
	"net"
	"strings"
)

// IsLoopbackHost reports whether host refers to the local machine: an IP in the
// IPv4 127.0.0.0/8 block, the IPv6 ::1 address, or the "localhost" name.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
