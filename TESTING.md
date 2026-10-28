# Testing & benchmarks

Everything runs locally and is safe to run on your own machine:

- Test servers bind to `127.0.0.1` only, on random ports, and stop when the run ends.
- They use throwaway keys, passwords and temp directories.
- Nothing touches `~/.ssh`, `~/.config/sfync`, your ssh-agent, launchd, rclone, or any outside network.

```bash
make test              # unit tests (race detector)
make test-integration  # end-to-end against real servers (below)
make test-all
make bench-quick       # fast benchmark pass, local network only
make bench             # full matrix, saved to testenv/bench/results/<commit>.txt
make bench-compare OLD=<name> NEW=<name>   # benchstat the difference
```

## Layout

The harness is a separate Go module, `testenv/`. Its test-only dependencies (an FTP server library) stay out of sfync's own `go.mod`, and it can still exercise sfync's internal packages.

| Package | What it does |
|---|---|
| `testenv/servers` | Starts servers on demand. **Real OpenSSH**: the system `/usr/sbin/sshd`, run as you with no root and key-only auth, using `internal-sftp`. **In-process SFTP**: `pkg/sftp`. **In-process FTP**: `ftpserverlib`, optionally without MFMT, or LIST-only like vsftpd. |
| `testenv/netem` | Userspace network emulation: added round-trip latency and bandwidth caps over real loopback TCP. FTP passive data connections are shaped too. |
| `testenv/fixtures` | Deterministic, incompressible file trees, cached in the temp dir: `website` (1200 small files), `mixed`, `deep` (300 nested dirs), `large` (4 × ~28 MB). |
| `testenv/integration` | sfync's sync engine end to end against every server kind. |
| `testenv/bench` | Benchmarks: cold upload, no-op upload, cold download, connect. |

## Benchmark matrix

Benchmarks cover every combination of server, network and fixture. Choose them with make variables:

```bash
make bench SERVERS=openssh NETS=wan FIXTURES=website BENCH=UpNoop
```

| Network | RTT | Bandwidth |
|---|---|---|
| `local` | — | unlimited |
| `lan` | 1 ms | 1 Gbit/s |
| `wan` | 40 ms | 100 Mbit/s |
| `slow` | 120 ms | 10 Mbit/s (opt-in: slow to run) |

Results report time per operation, MB/s and files/s. The workflow for an optimization is:

1. `make bench NAME=before`
2. Change the code.
3. `make bench NAME=after`
4. `make bench-compare OLD=before NEW=after`

Results depend on the machine, so they are git-ignored.
