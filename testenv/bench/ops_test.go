package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/netem"
)

// BenchmarkRemoteOps times single remote operations on an unshaped link,
// separating server-side cost from round trips: Stat (one request, no
// listing), and ReadDir on directories of several sizes.
func BenchmarkRemoteOps(b *testing.B) {
	for _, srvName := range selection("SFYNC_BENCH_SERVERS", "openssh,ftp") {
		b.Run("server="+srvName, func(b *testing.B) {
			srv := allServers[srvName](b, netem.Local)
			c := connect(b, srv, b.TempDir())

			for _, n := range []int{0, 20, 200} {
				dir := filepath.Join(srv.Root, fmt.Sprintf("dir%d", n))
				os.Mkdir(dir, 0o755)
				for i := range n {
					os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", i)), []byte("x"), 0o644)
				}
			}
			b.Run("op=stat", func(b *testing.B) {
				for b.Loop() {
					if _, err := c.Stat(transfer.RemotePath(srv.RemoteRoot, "dir0")); err != nil {
						b.Fatal(err)
					}
				}
			})
			for _, n := range []int{0, 20, 200} {
				b.Run(fmt.Sprintf("op=readdir/entries=%d", n), func(b *testing.B) {
					p := transfer.RemotePath(srv.RemoteRoot, fmt.Sprintf("dir%d", n))
					for b.Loop() {
						if _, err := c.ReadDir(p); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// knownDownloader mirrors transfer's optional fast-path interface.
type knownDownloader interface {
	DownloadKnown(remotePath, localPath string, size int64, mtime time.Time) error
}

// BenchmarkSmallFileRoundTrips measures how many network round trips one
// small-file transfer costs: over a 50ms-RTT link with unlimited bandwidth,
// time per operation divided by the RTT is the round-trip count (reported
// as "rtts"). Pins down per-file protocol overhead independent of
// bandwidth.
func BenchmarkSmallFileRoundTrips(b *testing.B) {
	const rtt = 50 * time.Millisecond
	link := netem.Profile{Name: "rtt50", RTT: rtt}
	for _, srvName := range selection("SFYNC_BENCH_SERVERS", "openssh,ftp") {
		b.Run("server="+srvName, func(b *testing.B) {
			srv := allServers[srvName](b, link)
			c := connect(b, srv, b.TempDir())
			local := filepath.Join(b.TempDir(), "small.html")
			if err := os.WriteFile(local, make([]byte, 4096), 0o644); err != nil {
				b.Fatal(err)
			}
			remote := transfer.RemotePath(srv.RemoteRoot, "small.html")
			if err := c.Upload(local, remote); err != nil { // seed for downloads
				b.Fatal(err)
			}
			dst := filepath.Join(b.TempDir(), "dl.html")

			report := func(b *testing.B) {
				b.ReportMetric(float64(b.Elapsed())/float64(b.N)/float64(rtt), "rtts")
			}
			b.Run("op=upload", func(b *testing.B) {
				for b.Loop() {
					if err := c.Upload(local, remote); err != nil {
						b.Fatal(err)
					}
				}
				report(b)
			})
			b.Run("op=download", func(b *testing.B) {
				for b.Loop() {
					if err := c.Download(remote, dst); err != nil {
						b.Fatal(err)
					}
				}
				report(b)
			})
			// What `sfync push` and the daemon do per saved file: ensure the
			// (existing) directory, then upload into it.
			nested := transfer.RemotePath(srv.RemoteRoot, "site", "blog", "2026", "10")
			if err := c.MkdirAll(nested); err != nil {
				b.Fatal(err)
			}
			b.Run("op=save", func(b *testing.B) {
				for b.Loop() {
					if err := c.MkdirAll(nested); err != nil {
						b.Fatal(err)
					}
					if err := c.Upload(local, transfer.RemotePath(nested, "post.html")); err != nil {
						b.Fatal(err)
					}
				}
				report(b)
			})
			if kd, ok := c.(knownDownloader); ok {
				b.Run("op=download-known", func(b *testing.B) {
					for b.Loop() {
						if err := kd.DownloadKnown(remote, dst, 4096, time.Now()); err != nil {
							b.Fatal(err)
						}
					}
					report(b)
				})
			}
		})
	}
}
