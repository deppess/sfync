package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/testenv/netem"
	"github.com/deppess/sfync/testenv/servers"
)

// The CLI tests run the real sfync binary as a user would, against the test
// servers. Each gets its own HOME, so the binary reads a throwaway config
// and known_hosts and never the user's own; SFYNC_NO_NOTIFY keeps it from
// posting desktop notifications, and SSH_AUTH_SOCK is unset so it can't
// reach the user's ssh-agent.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// TestMain removes the sfync binary built for the CLI tests.
func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		os.RemoveAll(filepath.Dir(binPath))
	}
	os.Exit(code)
}

// sfyncBinary builds sfync once per test run.
func sfyncBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sfync-cli-test-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "sfync")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = filepath.Join("..", "..") // the sfync module root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatalf("build sfync: %v", buildErr)
	}
	return binPath
}

// cli runs the sfync binary in an isolated home directory.
type cli struct {
	t    *testing.T
	bin  string
	home string
	dir  string // working directory for commands
}

func newCLI(t *testing.T, profiles map[string]*config.Profile) *cli {
	t.Helper()
	c := &cli{t: t, bin: sfyncBinary(t), home: t.TempDir()}
	c.dir = c.home
	if profiles != nil {
		c.writeConfig(profiles)
	}
	return c
}

func (c *cli) writeConfig(profiles map[string]*config.Profile) {
	c.t.Helper()
	data, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		c.t.Fatal(err)
	}
	dir := filepath.Join(c.home, config.DefaultConfigDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.DefaultConfigFile), data, 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// run executes sfync with args and returns its output and exit code.
func (c *cli) run(args ...string) (stdout, stderr string, code int) {
	c.t.Helper()
	// Bounded, so a hung command fails its test rather than the suite.
	ctx, cancel := context.WithTimeout(c.t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = c.dir
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "HOME", "SSH_AUTH_SOCK", "SFYNC_NO_NOTIFY":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "HOME="+c.home, "SFYNC_NO_NOTIFY=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code = 0
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		c.t.Fatalf("run sfync %v: %v", args, err)
	}
	return out.String(), errOut.String(), code
}

// ok runs sfync expecting success and returns its stdout.
func (c *cli) ok(args ...string) string {
	c.t.Helper()
	stdout, stderr, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("sfync %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// fails runs sfync expecting exit status 1 with wantErr on stderr.
func (c *cli) fails(wantErr string, args ...string) {
	c.t.Helper()
	stdout, stderr, code := c.run(args...)
	if code != 1 {
		c.t.Fatalf("sfync %s: exit %d, want 1\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, stdout, stderr)
	}
	if !strings.Contains(stderr, wantErr) {
		c.t.Errorf("sfync %s: stderr lacks %q:\n%s", strings.Join(args, " "), wantErr, stderr)
	}
}

func contains(t *testing.T, what, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s lacks %q:\n%s", what, want, got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileIs(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

// TestCLISyncCommands drives every sync command through the binary against
// each server kind: diff, up, push, pull (by local and by remote path),
// down, and the no-op and out-of-context cases.
func TestCLISyncCommands(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Profile{Name: "local"})
			local := filepath.Join(t.TempDir(), "site")
			writeFile(t, filepath.Join(local, "index.html"), "<h1>home</h1>")
			writeFile(t, filepath.Join(local, "sub", "page.html"), "<p>page</p>")
			writeFile(t, filepath.Join(local, "sub", "deep", "style.css"), "body{}")

			c := newCLI(t, map[string]*config.Profile{"site": srv.Profile(local)})
			c.dir = local

			out := c.ok("diff", "up", "site")
			contains(t, "diff up", out, "+ index.html")
			contains(t, "diff up", out, "+ sub/deep/style.css")
			contains(t, "diff up", out, "changes would be uploaded")

			contains(t, "up", c.ok("up", "site"), "✓ Upload complete")
			treesEqual(t, local, srv.Root)
			noPartials(t, srv.Root)

			contains(t, "second up", c.ok("up", "site"), "already in sync")
			contains(t, "diff up", c.ok("diff", "up", "site"), "No differences found")

			// push: a relative path resolves against the working directory.
			writeFile(t, filepath.Join(local, "index.html"), "<h1>home, edited</h1>")
			contains(t, "diff up", c.ok("diff", "up", "site"), "~ index.html")
			contains(t, "push", c.ok("push", "site", "index.html"), "✓ Uploaded: index.html")
			fileIs(t, filepath.Join(srv.Root, "index.html"), "<h1>home, edited</h1>")

			// current is push by absolute path, into a directory that
			// doesn't exist remotely yet.
			writeFile(t, filepath.Join(local, "new", "post.html"), "<p>new</p>")
			c.ok("current", "site", filepath.Join(local, "new", "post.html"))
			fileIs(t, filepath.Join(srv.Root, "new", "post.html"), "<p>new</p>")

			// pull by local relative path, then by absolute remote path.
			writeFile(t, filepath.Join(srv.Root, "sub", "page.html"), "<p>edited remotely</p>")
			contains(t, "pull", c.ok("pull", "site", "sub/page.html"), "✓ Downloaded: sub/page.html")
			fileIs(t, filepath.Join(local, "sub", "page.html"), "<p>edited remotely</p>")

			writeFile(t, filepath.Join(srv.Root, "sub", "deep", "style.css"), "body{color:red}")
			c.ok("pull", "site", path.Join(srv.RemoteRoot, "sub", "deep", "style.css"))
			fileIs(t, filepath.Join(local, "sub", "deep", "style.css"), "body{color:red}")

			// down brings a remote-only file and restores a deleted one.
			writeFile(t, filepath.Join(srv.Root, "remote-only.txt"), "from server")
			if err := os.Remove(filepath.Join(local, "index.html")); err != nil {
				t.Fatal(err)
			}
			contains(t, "diff down", c.ok("diff", "down", "site"), "+ remote-only.txt")
			contains(t, "down", c.ok("down", "site"), "✓ Download complete")
			treesEqual(t, local, srv.Root)

			// A file outside the context is refused before connecting.
			outside := filepath.Join(t.TempDir(), "elsewhere.txt")
			writeFile(t, outside, "x")
			c.fails("not within context", "push", "site", outside)
		})
	}
}

// TestCLIErrorsAreReported checks that failures print a reason on stderr
// and exit 1 — not only a desktop notification, which a terminal user (or
// a script) never sees.
func TestCLIErrorsAreReported(t *testing.T) {
	// A port nothing listens on: bind one, note it, close it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := l.Addr().(*net.TCPAddr).Port
	l.Close()

	local := t.TempDir()
	c := newCLI(t, nil)
	c.dir = local

	c.fails("config file not found", "up", "site")

	c.writeConfig(map[string]*config.Profile{"down": {
		Host: "127.0.0.1", Port: deadPort, Protocol: "ftp", Username: "u", Password: "p",
		RemotePath: "/", Context: local,
	}})
	c.fails("profile not found", "up", "nope")
	c.fails("profile not found", "diff", "up", "nope")
	c.fails("profile not found", "push", "nope", "x")
	c.fails("Error: ", "up", "down") // connection refused
	c.fails("unknown diff direction", "diff", "sideways", "down")

	// Usage errors print usage and exit 1.
	for _, args := range [][]string{{}, {"up"}, {"push", "down"}, {"diff", "up"}, {"bogus"}} {
		if _, _, code := c.run(args...); code != 1 {
			t.Errorf("sfync %v: exit %d, want 1", args, code)
		}
	}
	contains(t, "version", c.ok("version"), "sfync version ")
}

// TestCLIHostKeyTrustOnFirstUse checks host key verification through the
// binary: the first connection records the key in ~/.ssh/known_hosts, later
// ones accept it, and a changed key is refused.
func TestCLIHostKeyTrustOnFirstUse(t *testing.T) {
	srv := servers.StartOpenSSH(t, netem.Profile{Name: "local"})
	local := t.TempDir()
	writeFile(t, filepath.Join(local, "a.txt"), "a")
	profile := srv.Profile(local)
	verify := true
	profile.VerifyHostKey = &verify

	c := newCLI(t, map[string]*config.Profile{"site": profile})
	c.dir = local

	_, stderr, code := c.run("diff", "up", "site")
	if code != 0 {
		t.Fatalf("first connect: exit %d\n%s", code, stderr)
	}
	contains(t, "first connect stderr", stderr, "Adding")
	knownHostsPath := filepath.Join(c.home, ".ssh", "known_hosts")
	recorded, err := os.ReadFile(knownHostsPath)
	if err != nil || len(bytes.TrimSpace(recorded)) == 0 {
		t.Fatalf("known_hosts not written: %v", err)
	}

	_, stderr, code = c.run("diff", "up", "site")
	if code != 0 || strings.Contains(stderr, "Adding") {
		t.Fatalf("second connect: exit %d, re-added key?\n%s", code, stderr)
	}

	// Swap in a different ed25519 key for the same host.
	_, hosts, _, _, _, err := ssh.ParseKnownHosts(recorded)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	impostor, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line(hosts, impostor) + "\n"
	if err := os.WriteFile(knownHostsPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	c.fails("host key mismatch", "diff", "up", "site")
}
