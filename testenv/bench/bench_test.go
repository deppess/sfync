// Package bench benchmarks sfync's sync engine against real and in-process
// servers under emulated network conditions.
//
// The matrix is server × network × fixture, selected with environment
// variables (comma-separated; defaults in parentheses):
//
//	SFYNC_BENCH_SERVERS   openssh, sftp-inproc, ftp   (openssh,ftp)
//	SFYNC_BENCH_NETS      local, lan, wan, slow       (local,lan,wan)
//	SFYNC_BENCH_FIXTURES  website, mixed, deep, large (website,mixed,deep,large)
//
// Each operation is heavy, so run with a fixed iteration count and compare
// runs with benchstat, e.g. (see the Makefile's bench targets):
//
//	go test ./bench -run '^$' -bench . -benchtime 3x -count 6 | tee new.txt
//	benchstat old.txt new.txt
//
// Connection setup and per-iteration cleanup happen outside the timer; the
// connect cost itself is measured separately by BenchmarkConnect.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/fixtures"
	"github.com/deppess/sfync/testenv/netem"
	"github.com/deppess/sfync/testenv/servers"
)

var allServers = map[string]servers.Starter{
	"openssh":     servers.StartOpenSSH,
	"sftp-inproc": servers.StartInProcSFTP,
	"ftp":         servers.StartFTP,
}

// TestMain silences sfync's per-file progress output (written straight to
// os.Stderr) so terminal I/O doesn't distort timings.
func TestMain(m *testing.M) {
	if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = devnull
	}
	os.Exit(m.Run())
}

func selection(env, def string) []string {
	v := os.Getenv(env)
	if v == "" {
		v = def
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// eachCase runs fn for every selected server × network × fixture.
func eachCase(b *testing.B, fn func(b *testing.B, start servers.Starter, net netem.Profile, spec fixtures.Spec)) {
	for _, srvName := range selection("SFYNC_BENCH_SERVERS", "openssh,ftp") {
		start, ok := allServers[srvName]
		if !ok {
			b.Fatalf("unknown server %q", srvName)
		}
		for _, netName := range selection("SFYNC_BENCH_NETS", "local,lan,wan") {
			net, ok := netem.Presets[netName]
			if !ok {
				b.Fatalf("unknown network %q", netName)
			}
			for _, fixName := range selection("SFYNC_BENCH_FIXTURES", "website,mixed,deep,large") {
				spec, ok := fixtures.Specs[fixName]
				if !ok {
					b.Fatalf("unknown fixture %q", fixName)
				}
				b.Run(fmt.Sprintf("server=%s/net=%s/fixture=%s", srvName, netName, fixName), func(b *testing.B) {
					fn(b, start, net, spec)
				})
			}
		}
	}
}

func fixture(b *testing.B, spec fixtures.Spec) (string, fixtures.Stats) {
	b.Helper()
	dir, st, err := fixtures.Get(spec)
	if err != nil {
		b.Fatal(err)
	}
	return dir, st
}

func connect(b *testing.B, srv *servers.Server, context string) transfer.RemoteClient {
	b.Helper()
	c := transfer.NewClient(srv.Profile(context))
	if err := c.Connect(); err != nil {
		b.Fatalf("connect: %v", err)
	}
	b.Cleanup(func() { c.Close() })
	return c
}

// emptyDir removes everything inside dir, keeping dir itself. dir is
// always a server root or local target created under b.TempDir().
func emptyDir(b *testing.B, dir string) {
	b.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			b.Fatal(err)
		}
	}
}

func report(b *testing.B, st fixtures.Stats) {
	b.ReportMetric(float64(st.Files)*float64(b.N)/b.Elapsed().Seconds(), "files/s")
}

func checkResult(b *testing.B, res *transfer.MirrorResult, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
	if !res.Success {
		b.Fatalf("sync errors: %v", res.Errors)
	}
}

// BenchmarkUpCold measures a full upload into an empty remote — the first
// deploy. Bandwidth and per-file round trips both matter.
func BenchmarkUpCold(b *testing.B) {
	eachCase(b, func(b *testing.B, start servers.Starter, net netem.Profile, spec fixtures.Spec) {
		local, st := fixture(b, spec)
		srv := start(b, net)
		profile := srv.Profile(local)
		c := connect(b, srv, local)
		b.SetBytes(st.Bytes)
		b.ResetTimer()
		for b.Loop() {
			b.StopTimer()
			emptyDir(b, srv.Root)
			b.StartTimer()
			res, err := transfer.MirrorUp(c, profile, false)
			checkResult(b, res, err)
		}
		report(b, st)
	})
}

// BenchmarkUpNoop measures syncing an unchanged tree — scan and diff only.
// This is the steady state of repeated `sfync up` and dominates on
// latency-bound links.
func BenchmarkUpNoop(b *testing.B) {
	eachCase(b, func(b *testing.B, start servers.Starter, net netem.Profile, spec fixtures.Spec) {
		local, st := fixture(b, spec)
		srv := start(b, net)
		profile := srv.Profile(local)
		c := connect(b, srv, local)
		res, err := transfer.MirrorUp(c, profile, false) // populate remote
		checkResult(b, res, err)
		b.ResetTimer()
		for b.Loop() {
			res, err := transfer.MirrorUp(c, profile, false)
			checkResult(b, res, err)
			if res.Uploaded != 0 {
				b.Fatalf("no-op sync uploaded %d files", res.Uploaded)
			}
		}
		report(b, st)
	})
}

// BenchmarkDownCold measures a full download into an empty local directory.
func BenchmarkDownCold(b *testing.B) {
	eachCase(b, func(b *testing.B, start servers.Starter, net netem.Profile, spec fixtures.Spec) {
		src, st := fixture(b, spec)
		srv := start(b, net)
		if err := fixtures.Copy(src, filepath.Join(srv.Root, "tree")); err != nil {
			b.Fatal(err)
		}
		local := filepath.Join(b.TempDir(), "local")
		if err := os.Mkdir(local, 0o755); err != nil {
			b.Fatal(err)
		}
		profile := srv.Profile(local)
		profile.RemotePath = transfer.RemotePath(srv.RemoteRoot, "tree")
		c := connect(b, srv, local)
		b.SetBytes(st.Bytes)
		b.ResetTimer()
		for b.Loop() {
			b.StopTimer()
			emptyDir(b, local)
			b.StartTimer()
			res, err := transfer.MirrorDown(c, profile, false)
			checkResult(b, res, err)
		}
		report(b, st)
	})
}

// BenchmarkConnect measures connection setup (TCP + SSH handshake + auth +
// SFTP init, or FTP greeting + login) — paid by every CLI invocation.
func BenchmarkConnect(b *testing.B) {
	for _, srvName := range selection("SFYNC_BENCH_SERVERS", "openssh,ftp") {
		start := allServers[srvName]
		for _, netName := range selection("SFYNC_BENCH_NETS", "local,lan,wan") {
			net := netem.Presets[netName]
			b.Run(fmt.Sprintf("server=%s/net=%s", srvName, netName), func(b *testing.B) {
				srv := start(b, net)
				profile := srv.Profile(b.TempDir())
				b.ResetTimer()
				for b.Loop() {
					c := transfer.NewClient(profile)
					if err := c.Connect(); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					c.Close()
					b.StartTimer()
				}
			})
		}
	}
}
