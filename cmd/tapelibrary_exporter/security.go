package main

import (
	"net"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// isLoopbackHost reports whether host (the address part of a
// --web.listen-address value, port already stripped) only reaches this
// machine. An empty host (from a bare ":9999") binds every interface, so it
// is deliberately NOT loopback despite also covering localhost traffic.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() // covers 127.0.0.0/8 and ::1
	}
	return false // unresolvable/unknown host: don't assume it's safe
}

// warnIfExposedAndUnauthenticated implements security-and-hardening.md's
// Rule 3: log one visible, non-fatal warning at startup if any listenAddress
// is reachable from outside this host AND no --web.config.file is set to
// enable TLS/Basic Auth. It never blocks startup or alters a default
// (Rule 2). It only makes an already-exposed posture visible.
func warnIfExposedAndUnauthenticated(log *logger.Logger, listenAddresses []string, webConfigFile string) {
	if webConfigFile != "" {
		return // TLS/Basic Auth already configured
	}
	for _, addr := range listenAddresses {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr // no "host:port" shape found; treat the whole value as host
		}
		if !isLoopbackHost(host) {
			log.Warn("/metrics is served unauthenticated on a reachable address; "+
				"set --web.config.file to enable TLS/Basic Auth if this exporter is reachable from an untrusted network",
				"listen_address", addr)
			return // one warning is enough even if more than one address is exposed
		}
	}
}
