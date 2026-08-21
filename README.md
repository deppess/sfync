# sfync

SFTP/FTP sync, mounting, and auto-upload daemon for Linux and macOS — bidirectional mirror sync, single-file push/pull, mounting, and editor integration.

## Dependencies

Linux:

```
libnotify
sshfs  rclone   # mounting only
```

macOS:

```
rclone   # mounting only — sftp, ftp, and ftps profiles all mount via `rclone nfsmount`.
         # Homebrew's rclone has no FUSE support on macOS, so sshfs/macFUSE
         # are not used and not needed.
```

Install with `brew install rclone`. Desktop notifications ship as a bundled helper app (embedded in the `sfync` binary, extracted on first use) — no extra install needed. `sfync install-daemon` prompts macOS for notification permission once; grant it and notifications work from the background daemon from then on.

## Install

```bash
go build -o sfync .
sudo cp sfync /usr/local/bin/sfync    # Linux, or macOS on Intel
cp sfync /opt/homebrew/bin/sfync      # macOS on Apple Silicon
```

## Config

`~/.config/sfync/config.json`

```json
{
  "myserver": {
    "host": "example.com",
    "username": "user",
    "sshKey": "/home/user/.ssh/id_ed25519",
    "port": 22,
    "protocol": "sftp",
    "remotePath": "/var/www/html",
    "context": "/home/user/projects/website",
    "autoSync": true,
    "autoSyncDebounce": 2000,
    "verifyHostKey": true
  }
}
```

`protocol` is `sftp` (default), `ftp`, or `ftps` (explicit AUTH TLS — same plaintext-`password` requirement as `ftp`, but the connection is encrypted; prefer this over `ftp` whenever the server supports it). `password` or `sshKey` required for SFTP. FTP/FTPS require `password`. Passwords are stored in plaintext — `chmod 600 ~/.config/sfync/config.json`.

## Commands

```
sfync up <profile>              mirror local → remote
sfync down <profile>            mirror remote → local
sfync diff up <profile>         dry-run: show what would be uploaded
sfync diff down <profile>       dry-run: show what would be downloaded
sfync push <profile> <file>     upload a single file
sfync pull <profile> <file>     download a single file
sfync current <profile> <file>  upload by absolute path (editor integration)
```

```
sfync mount <profile>           mount remote filesystem
sfync mount <profile> --yazi    mount and open in yazi
sfync unmount <profile>         unmount
sfync unmount --all             unmount everything
sfync mounts                    list active mounts
```

```
sfync install-daemon            install background service (systemd on Linux, launchd on macOS)
sfync uninstall-daemon          remove background service
sfync on                        start the daemon
sfync off                       stop the daemon
```

## .syncignore

Place a `.syncignore` in your project root. Supports [doublestar](https://github.com/bmatcuk/doublestar) glob syntax.

```gitignore
.git/
node_modules/
dist/
*.log
.env
```

Ignored directories are skipped by the file watcher. Changes to `.syncignore` are picked up live without restarting the daemon.

## Daemon

Watches all profiles with `autoSync: true`. Debounces per-file, retries with exponential backoff, batches desktop notifications, and hot-reloads config on save.

Linux: `install-daemon` creates a systemd user unit. Enable it with `systemctl --user enable sfync` to auto-start at login.

```bash
journalctl --user -u sfync -f
```

macOS: `install-daemon` creates a launchd LaunchAgent (`~/Library/LaunchAgents/com.deppess.sfync.plist`), which auto-starts at every login on its own — no separate enable step.

```bash
tail -f ~/Library/Logs/sfync.log
tail -f ~/Library/Logs/sfync.error.log
```

## Editor Integration

`sfync current` takes an absolute path, walks up to find the git root, and uploads relative to `context`. Works with any editor that can invoke shell commands.

```toml
# Helix
[keys.normal.space]
u = ":run-shell-command sfync up myserver %{buffer_name}"
c = ":run-shell-command sfync current myserver %{buffer_name}"
```

## Shell Completions

```bash
sfync --completions fish > ~/.config/fish/completions/sfync.fish
sfync --completions bash > ~/.local/share/bash-completion/completions/sfync
sfync --completions zsh  > ~/.zfunc/_sfync
```

## License

MIT
