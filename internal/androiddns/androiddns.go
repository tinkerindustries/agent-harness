// Package androiddns gives Go's own DNS resolver real name servers on
// Android. A CGO_ENABLED=0 build always uses that resolver, which reads
// /etc/resolv.conf. Android has no such file, so the resolver falls back to
// 127.0.0.1:53 and [::1]:53, where nothing listens, and every provider
// request fails to resolve. Install redirects those two loopback addresses to
// the name servers in Termux's resolv.conf, or to public resolvers when
// Termux has none. It does nothing on any other GOOS, and nothing on Android
// when /etc/resolv.conf exists.
package androiddns

import (
	"context"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
)

const (
	systemResolvConf = "/etc/resolv.conf"
	termuxPrefix     = "/data/data/com.termux/files/usr"
)

var publicServers = []string{"8.8.8.8:53", "1.1.1.1:53"}

// Install points net.DefaultResolver at usable name servers when the process
// runs on Android without /etc/resolv.conf. It returns the servers it chose,
// or nil when it left the resolver alone.
func Install() []string {
	return install(net.DefaultResolver, runtime.GOOS, os.Getenv, os.ReadFile)
}

func install(r *net.Resolver, goos string, getenv func(string) string, readFile func(string) ([]byte, error)) []string {
	if goos != "android" {
		return nil
	}
	if _, err := readFile(systemResolvConf); err == nil {
		return nil
	}
	servers := termuxServers(getenv, readFile)
	if len(servers) == 0 {
		servers = publicServers
	}
	var d net.Dialer
	r.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return d.DialContext(ctx, network, redirect(address, servers))
	}
	return servers
}

// termuxServers reads the nameserver lines of Termux's resolv.conf, under
// $PREFIX when Termux set it and under Termux's fixed install prefix when not.
func termuxServers(getenv func(string) string, readFile func(string) ([]byte, error)) []string {
	prefix := getenv("PREFIX")
	if prefix == "" {
		prefix = termuxPrefix
	}
	data, err := readFile(prefix + "/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if _, err := netip.ParseAddr(fields[1]); err == nil {
			servers = append(servers, net.JoinHostPort(fields[1], "53"))
		}
	}
	return servers
}

// redirect maps the resolver's two loopback fallbacks onto the first two
// servers and passes any other address through.
func redirect(address string, servers []string) string {
	switch address {
	case "127.0.0.1:53":
		return servers[0]
	case "[::1]:53":
		return servers[1%len(servers)]
	}
	return address
}
