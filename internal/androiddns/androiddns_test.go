package androiddns

import (
	"context"
	"io/fs"
	"net"
	"slices"
	"testing"
	"time"
)

func files(m map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if data, ok := m[name]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
}

func env(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestInstallLeavesOtherPlatformsAlone(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		r := &net.Resolver{}
		if got := install(r, goos, env(nil), files(nil)); got != nil || r.Dial != nil {
			t.Errorf("%s: install returned %v and set Dial %v, want neither", goos, got, r.Dial != nil)
		}
	}
}

func TestInstallLeavesAndroidWithResolvConfAlone(t *testing.T) {
	r := &net.Resolver{}
	got := install(r, "android", env(nil), files(map[string]string{systemResolvConf: "nameserver 10.0.0.1\n"}))
	if got != nil || r.Dial != nil {
		t.Fatalf("install returned %v and set Dial %v, want neither", got, r.Dial != nil)
	}
}

func TestInstallReadsTermuxResolvConfUnderPrefix(t *testing.T) {
	r := &net.Resolver{}
	conf := "# written by Termux\nnameserver 9.9.9.9\nsearch lan\nnameserver 2620:fe::fe\nnameserver not-an-ip\n"
	got := install(r, "android", env(map[string]string{"PREFIX": "/custom/usr"}), files(map[string]string{"/custom/usr/etc/resolv.conf": conf}))
	want := []string{"9.9.9.9:53", "[2620:fe::fe]:53"}
	if !slices.Equal(got, want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}
	if r.Dial == nil {
		t.Fatal("Dial not set")
	}
}

func TestInstallFallsBackToTermuxDefaultPrefix(t *testing.T) {
	got := install(&net.Resolver{}, "android", env(nil), files(map[string]string{termuxPrefix + "/etc/resolv.conf": "nameserver 1.0.0.1\n"}))
	if want := []string{"1.0.0.1:53"}; !slices.Equal(got, want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}
}

func TestInstallFallsBackToPublicServers(t *testing.T) {
	got := install(&net.Resolver{}, "android", env(nil), files(nil))
	if !slices.Equal(got, publicServers) {
		t.Fatalf("servers = %v, want %v", got, publicServers)
	}
}

func TestRedirect(t *testing.T) {
	two := []string{"a:53", "b:53"}
	one := []string{"a:53"}
	cases := []struct {
		address string
		servers []string
		want    string
	}{
		{"127.0.0.1:53", two, "a:53"},
		{"[::1]:53", two, "b:53"},
		{"[::1]:53", one, "a:53"},
		{"192.168.1.1:53", two, "192.168.1.1:53"},
	}
	for _, c := range cases {
		if got := redirect(c.address, c.servers); got != c.want {
			t.Errorf("redirect(%q, %v) = %q, want %q", c.address, c.servers, got, c.want)
		}
	}
}

// TestInstalledDialReachesChosenServer dials the resolver's loopback fallback
// through the installed Dial and checks the packet arrives at the server
// install chose.
func TestInstalledDialReachesChosenServer(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	saved := publicServers
	publicServers = []string{server.LocalAddr().String()}
	defer func() { publicServers = saved }()

	r := &net.Resolver{}
	install(r, "android", env(nil), files(nil))
	conn, err := r.Dial(context.Background(), "udp", "127.0.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	if err := server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, _, err := server.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "query" {
		t.Fatalf("server read %q, want %q", buf[:n], "query")
	}
}
