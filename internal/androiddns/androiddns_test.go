package androiddns

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeFS is an in-memory filesystem behind the readFile and stat seams. Each
// write moves the file's mtime forward, and it counts the reads of each path.
type fakeFS struct {
	mu    sync.Mutex
	files map[string]fakeFile
	reads map[string]int
	clock time.Time
}

type fakeFile struct {
	data       string
	mtime      time.Time
	unreadable bool
}

func newFS(m map[string]string) *fakeFS {
	f := &fakeFS{files: map[string]fakeFile{}, reads: map[string]int{}, clock: time.Unix(1_700_000_000, 0)}
	for name, data := range m {
		f.write(name, data)
	}
	return f
}

func (f *fakeFS) write(name, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(time.Second)
	f.files[name] = fakeFile{data: data, mtime: f.clock}
}

func (f *fakeFS) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, name)
}

func (f *fakeFS) makeUnreadable(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file := f.files[name]
	file.unreadable = true
	f.files[name] = file
}

func (f *fakeFS) readCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[name]
}

func (f *fakeFS) readFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[name]++
	file, ok := f.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if file.unreadable {
		return nil, fs.ErrPermission
	}
	return []byte(file.data), nil
}

func (f *fakeFS) stat(name string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return fakeInfo{name: name, size: int64(len(file.data)), mtime: file.mtime}, nil
}

type fakeInfo struct {
	name  string
	size  int64
	mtime time.Time
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return i.mtime }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }

func env(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// logs collects what install logs.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

func installFS(r *net.Resolver, goos string, getenv func(string) string, f *fakeFS) []string {
	return install(r, goos, getenv, f.readFile, f.stat, func(string, ...any) {})
}

var termuxConf = termuxPrefix + "/etc/resolv.conf"

func TestInstallLeavesOtherPlatformsAlone(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		r := &net.Resolver{}
		if got := installFS(r, goos, env(nil), newFS(nil)); got != nil || r.Dial != nil {
			t.Errorf("%s: install returned %v and set Dial %v, want neither", goos, got, r.Dial != nil)
		}
	}
}

func TestInstallLeavesAndroidWithResolvConfAlone(t *testing.T) {
	r := &net.Resolver{}
	got := installFS(r, "android", env(nil), newFS(map[string]string{systemResolvConf: "nameserver 10.0.0.1\n"}))
	if got != nil || r.Dial != nil {
		t.Fatalf("install returned %v and set Dial %v, want neither", got, r.Dial != nil)
	}
}

func TestInstallReadsTermuxResolvConfUnderPrefix(t *testing.T) {
	r := &net.Resolver{}
	conf := "# written by Termux\nnameserver 9.9.9.9\nsearch lan\nnameserver 2620:fe::fe\nnameserver not-an-ip\n"
	got := installFS(r, "android", env(map[string]string{"PREFIX": "/custom/usr"}), newFS(map[string]string{"/custom/usr/etc/resolv.conf": conf}))
	want := []string{"9.9.9.9:53", "[2620:fe::fe]:53"}
	if !slices.Equal(got, want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}
	if r.Dial == nil {
		t.Fatal("Dial not set")
	}
}

func TestInstallFallsBackToTermuxDefaultPrefix(t *testing.T) {
	got := installFS(&net.Resolver{}, "android", env(nil), newFS(map[string]string{termuxConf: "nameserver 1.0.0.1\n"}))
	if want := []string{"1.0.0.1:53"}; !slices.Equal(got, want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}
}

func TestInstallFallsBackToPublicServers(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"missing":       nil,
		"empty":         {termuxConf: ""},
		"no nameserver": {termuxConf: "search lan\nnameserver not-an-ip\n"},
	} {
		if got := installFS(&net.Resolver{}, "android", env(nil), newFS(files)); !slices.Equal(got, publicServers) {
			t.Errorf("%s: servers = %v, want %v", name, got, publicServers)
		}
	}
}

// An unreadable file falls back to the public servers. It fails if a read
// error leaves the list empty instead, which would leave redirect nothing to
// index.
func TestInstallFallsBackWhenUnreadable(t *testing.T) {
	f := newFS(map[string]string{termuxConf: "nameserver 1.0.0.1\n"})
	f.makeUnreadable(termuxConf)
	if got := installFS(&net.Resolver{}, "android", env(nil), f); !slices.Equal(got, publicServers) {
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

	usePublicServers(t, server.LocalAddr().String())

	r := &net.Resolver{}
	installFS(r, "android", env(nil), newFS(nil))
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

// fallback stands in for the public servers in the tests that follow, so a
// dial to it is told apart from a dial to the file's 127.0.0.1:53 without
// either leaving the machine. A UDP dial only sets the peer address, so
// nothing need listen on either.
const fallback = "127.0.0.1:5353"

func usePublicServers(t *testing.T, servers ...string) {
	t.Helper()
	saved := publicServers
	publicServers = servers
	t.Cleanup(func() { publicServers = saved })
}

// dialed dials the resolver's IPv4 loopback fallback through r and returns
// the address the installed Dial actually connected to.
func dialed(t *testing.T, r *net.Resolver) string {
	t.Helper()
	conn, err := r.Dial(context.Background(), "udp", "127.0.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.RemoteAddr().String()
}

// A server list written after install reaches the next dial. It fails if the
// Dial closure keeps the list install read at startup, which is the bug that
// left a session on a Wi-Fi resolver after the phone moved to mobile data.
func TestDialUsesServersWrittenAfterInstall(t *testing.T) {
	usePublicServers(t, fallback)
	f := newFS(map[string]string{termuxConf: "nameserver 127.0.0.2\n"})
	r := &net.Resolver{}
	var l logs
	install(r, "android", env(nil), f.readFile, f.stat, l.logf)
	if lines := l.all(); len(lines) != 0 {
		t.Fatalf("install logged %q; the caller logs the startup servers", lines)
	}

	f.write(termuxConf, "nameserver 127.0.0.1\n")
	if got := dialed(t, r); got != "127.0.0.1:53" {
		t.Fatalf("dial after rewrite reached %s, want 127.0.0.1:53", got)
	}
	want := []string{termuxConf + " changed; resolving DNS through 127.0.0.1:53"}
	if lines := l.all(); !slices.Equal(lines, want) {
		t.Fatalf("logged %q, want %q", lines, want)
	}
}

// An unchanged file costs a stat, not a read. It fails if the Dial closure
// re-reads the file on every dial, or if the stamp it compares is never
// recorded.
func TestDialDoesNotRereadUnchangedFile(t *testing.T) {
	f := newFS(map[string]string{termuxConf: "nameserver 127.0.0.1\n"})
	r := &net.Resolver{}
	installFS(r, "android", env(nil), f)
	if n := f.readCount(termuxConf); n != 1 {
		t.Fatalf("install read the file %d times, want 1", n)
	}
	for range 5 {
		dialed(t, r)
	}
	if n := f.readCount(termuxConf); n != 1 {
		t.Fatalf("file read %d times after five dials of an unchanged file, want 1", n)
	}
	f.write(termuxConf, "nameserver 127.0.0.1\n")
	dialed(t, r)
	if n := f.readCount(termuxConf); n != 2 {
		t.Fatalf("file read %d times after one rewrite, want 2", n)
	}
}

// A file that goes missing or loses its servers falls back to the public
// servers, and one that gains servers leaves the fallback. It fails if the
// fallback is chosen only at install, or if a missing file is taken as
// unchanged because its last stamp was a missing file too.
func TestDialFollowsFileIntoAndOutOfFallback(t *testing.T) {
	usePublicServers(t, fallback)
	f := newFS(nil)
	r := &net.Resolver{}
	if got := installFS(r, "android", env(nil), f); !slices.Equal(got, []string{fallback}) {
		t.Fatalf("install with no file chose %v, want %v", got, []string{fallback})
	}
	steps := []struct {
		name   string
		change func()
		want   string
	}{
		{"file appears with a server", func() { f.write(termuxConf, "nameserver 127.0.0.1\n") }, "127.0.0.1:53"},
		{"file removed", func() { f.remove(termuxConf) }, fallback},
		{"file written again", func() { f.write(termuxConf, "nameserver 127.0.0.1\n") }, "127.0.0.1:53"},
		{"file emptied", func() { f.write(termuxConf, "") }, fallback},
		{"file gains a server", func() { f.write(termuxConf, "nameserver 127.0.0.1\n") }, "127.0.0.1:53"},
		{"file has no usable nameserver", func() { f.write(termuxConf, "search lan\n") }, fallback},
		{"file becomes unreadable", func() { f.write(termuxConf, "nameserver 127.0.0.1\n"); f.makeUnreadable(termuxConf) }, fallback},
		{"file readable again", func() { f.write(termuxConf, "nameserver 127.0.0.1\n") }, "127.0.0.1:53"},
	}
	for _, s := range steps {
		s.change()
		if got := dialed(t, r); got != s.want {
			t.Fatalf("%s: dial reached %s, want %s", s.name, got, s.want)
		}
	}
}

// Dials from many goroutines while the file is rewritten underneath them.
// Under -race it fails if the cached list or its stamp is touched outside
// the lock; without -race it still fails if a dial reaches anything but one
// of the two lists the file alternates between.
func TestConcurrentDialsWhileFileChanges(t *testing.T) {
	usePublicServers(t, fallback)
	f := newFS(map[string]string{termuxConf: "nameserver 127.0.0.1\n"})
	r := &net.Resolver{}
	installFS(r, "android", env(nil), f)

	done := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			if i%2 == 0 {
				f.write(termuxConf, "")
			} else {
				f.write(termuxConf, "nameserver 127.0.0.1\n")
			}
		}
	})

	var dialers sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		dialers.Go(func() {
			for range 50 {
				conn, err := r.Dial(context.Background(), "udp", "127.0.0.1:53")
				if err != nil {
					errs <- err
					return
				}
				got := conn.RemoteAddr().String()
				conn.Close()
				if got != "127.0.0.1:53" && got != fallback {
					errs <- fmt.Errorf("dial reached %s", got)
					return
				}
			}
		})
	}
	dialers.Wait()
	close(done)
	writer.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
