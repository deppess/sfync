package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/internal/watcher"
	"github.com/deppess/sfync/testenv/netem"
)

// BenchmarkDaemonBurst measures the auto-sync daemon's upload path: the
// real upload queue and connection pool, fed a burst of changed files the
// way the watcher would after debouncing (a formatter rewriting files, a
// git pull, a build), timed until every file is live on the server.
// Reports "ms/file" alongside time per burst. Default: 30 files, 40ms
// links.
func BenchmarkDaemonBurst(b *testing.B) {
	const files = 30
	for _, srvName := range selection("SFYNC_BENCH_SERVERS", "openssh,ftp") {
		for _, netName := range selection("SFYNC_BENCH_DAEMON_NETS", "wan") {
			net := netem.Presets[netName]
			b.Run(fmt.Sprintf("server=%s/net=%s", srvName, netName), func(b *testing.B) {
				srv := allServers[srvName](b, net)
				local := b.TempDir()
				var paths []string
				for i := range files {
					p := filepath.Join(local, "assets", fmt.Sprintf("f%02d.css", i))
					os.MkdirAll(filepath.Dir(p), 0o755)
					if err := os.WriteFile(p, make([]byte, 2048), 0o644); err != nil {
						b.Fatal(err)
					}
					paths = append(paths, p)
				}

				profile := srv.Profile(local)
				profiles := map[string]*config.Profile{"p": profile}
				pool := transfer.NewConnectionPool(profiles)
				defer pool.CloseAll()
				q := watcher.NewUploadQueue(profiles, pool)

				var mu sync.Mutex
				var wg sync.WaitGroup
				var failures []string
				q.Start(
					func(_, _ string) { wg.Done() },
					func(_, rel string, err error, _ int) {
						mu.Lock()
						failures = append(failures, rel+": "+err.Error())
						mu.Unlock()
						wg.Done()
					},
				)
				defer q.Stop()

				// Warm the connection, as a running daemon would have it.
				wg.Add(1)
				q.Enqueue("p", paths[0])
				wg.Wait()

				b.ResetTimer()
				for b.Loop() {
					wg.Add(len(paths))
					for _, p := range paths {
						q.Enqueue("p", p)
					}
					done := make(chan struct{})
					go func() { wg.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(5 * time.Minute):
						b.Fatal("burst never completed")
					}
				}
				b.StopTimer()
				if len(failures) > 0 {
					b.Fatalf("%d uploads failed: %v", len(failures), failures[0])
				}
				b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N*files), "ms/file")
			})
		}
	}
}
