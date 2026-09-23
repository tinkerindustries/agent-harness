// Package androiddns gives Go's own DNS resolver real name servers on
// Android. A CGO_ENABLED=0 build always uses that resolver, which reads
// /etc/resolv.conf. Android has no such file, so the resolver falls back to
// 127.0.0.1:53 and [::1]:53, where nothing listens, and every provider
// request fails to resolve. Install redirects those two loopback addresses to
// the name servers in Termux's resolv.conf, or to public resolvers when
// Termux has none. It does nothing on any other GOOS, and nothing on Android
// when /etc/resolv.conf exists.
//
// The host rewrites Termux's resolv.conf whenever the active network
// changes, so the servers are resolved on each dial rather than once: a
// dial stats the file and re-reads it only when its mtime or size moved.
package androiddns

import (
	"context"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	systemResolvConf = "/etc/resolv.conf"
	termuxPrefix     = "/data/data/com.termux/files/usr"
)

var publicServers = []string{"8.8.8.8:53", "1.1.1.1:53"}

// Install points net.DefaultResolver at usable name servers when the process
// runs on Android without /etc/resolv.conf. It returns the servers it chose
// at startup, or nil when it left the resolver alone. When a later dial finds
// Termux's resolv.conf has changed the servers, it logs the new ones.
func Install() []string {
	return install(net.DefaultResolver, runtime.GOOS, os.Getenv, os.ReadFile, os.Stat, log.Printf)
}

func install(r *net.Resolver, goos string, getenv func(string) string, readFile func(string) ([]byte, error), stat func(string) (fs.FileInfo, error), logf func(string, ...any)) []string {
	if goos != "android" {
		return nil
	}
	if _, err := readFile(systemResolvConf); err == nil {
		return nil
	}
	prefix := getenv("PREFIX")
	if prefix == "" {
		prefix = termuxPrefix
	}
	c := &conf{path: prefix + "/etc/resolv.conf", readFile: readFile, stat: stat, logf: logf}
	servers := c.current()
	var d net.Dialer
	r.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return d.DialContext(ctx, network, redirect(address, c.current()))
	}
	return servers
}

// conf is Termux's resolv.conf as the dials last saw it. The resolver dials
// from many goroutines, so every field below mu is read and written under it.
type conf struct {
	path     string
	readFile func(string) ([]byte, error)
	stat     func(string) (fs.FileInfo, error)
	logf     func(string, ...any)

	mu      sync.Mutex
	read    bool      // servers reflects a read at stamp
	missing bool      // the stat at stamp failed
	mtime   time.Time // of the file at stamp
	size    int64
	servers []string // never empty; never mutated once stored
}

// current returns the servers to dial: the nameserver lines of the file, or
// the public servers when the file is missing, unreadable or has none. It
// re-reads the file only when a stat shows it changed since the last read.
func (c *conf) current() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := c.stat(c.path)
	missing := err != nil
	var mtime time.Time
	var size int64
	if !missing {
		mtime, size = info.ModTime(), info.Size()
	}
	if c.read && missing == c.missing && mtime.Equal(c.mtime) && size == c.size {
		return c.servers
	}
	var servers []string
	// A file that stats but cannot be read is not recorded as read, so the
	// next dial tries it again rather than waiting for its mtime to move.
	read := true
	if !missing {
		data, err := c.readFile(c.path)
		if err != nil {
			read = false
		} else {
			servers = parse(data)
		}
	}
	if len(servers) == 0 {
		servers = publicServers
	}
	if c.servers != nil && !slices.Equal(servers, c.servers) {
		c.logf("%s changed; resolving DNS through %s", c.path, strings.Join(servers, ", "))
	}
	c.read, c.missing, c.mtime, c.size, c.servers = read, missing, mtime, size, servers
	return servers
}

// parse returns the usable nameserver lines of a resolv.conf as host:53.
func parse(data []byte) []string {
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
