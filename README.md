# sfync

SFTP/FTP sync, mounting, and auto-upload daemon for Linux. Bidirectional mirror sync, file watching, FUSE mounting, and editor integration — single binary, no external sync tools.

## Dependencies

```
libnotify
```

```
# mounting only
sshfs  rclone
```

```
# --yazi only
yazi  kitty
```

## Installation

```bash
cd sfync
go build -o sfync .
sudo cp sfync /usr/local/bin/sfync
```

## Configuration

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
    "autoSyncDebounce": 2000
  }
}
```

| Field | Required | Default | Description |
|---|---|---|---|
| `host` | yes | — | Hostname or IP |
| `username` | yes | — | Login username |
| `password` | no* | — | Login password |
| `sshKey` | no* | — | Path to SSH private key (SFTP only) |
| `port` | no | 22 / 21 | Port number |
| `protocol` | no | `"sftp"` | `"sftp"` or `"ftp"` |
| `remotePath` | no | `"/"` | Remote directory to sync |
| `context` | no | git root | Local project root |
| `autoSync` | no | `false` | Watch and auto-upload on save |
| `autoSyncDebounce` | no | `2000` | ms to wait after last save before uploading |
| `verifyHostKey` | no | `true` | TOFU host key verification (SFTP only) |

\* `password` or `sshKey` required for SFTP. FTP requires `password`.

Passwords are stored in plaintext — `chmod 600 ~/.config/sfync/config.json`.

## Usage

```
sfync up <profile>              mirror local → remote
sfync down <profile>            mirror remote → local
sfync diff up <profile>         dry-run: show what would be uploaded
sfync diff down <profile>       dry-run: show what would be downloaded
sfync push <profile> <file>     upload a single file
sfync pull <profile> <file>     download a single file
sfync current <profile> <file>  upload file by absolute path (editor integration)
```

Diff output: `+` new · `~` changed · `-` deleted

```
sfync mount <profile>           mount remote filesystem via FUSE
sfync mount <profile> --yazi    mount and open in yazi (floating kitty window)
sfync unmount <profile>         unmount
sfync unmount --all             unmount everything
sfync mounts                    list active mounts
```

```
sfync install-daemon            install systemd user service
sfync uninstall-daemon          remove systemd user service
sfync on                        start the daemon
sfync off                       stop the daemon
```

```
journalctl --user -u sfync -f   follow daemon logs
```

## .syncignore

Place a `.syncignore` in your project root. Uses [doublestar](https://github.com/bmatcuk/doublestar) glob syntax — same rules as `.gitignore`.

```gitignore
.git/
node_modules/
dist/
*.log
*.tmp
.env
**/*.backup
```

Ignored directories are skipped by the file watcher entirely (saves inotify watches). The `.syncignore` file itself is always excluded. Changes to `.syncignore` are picked up live without restarting the daemon.

## Daemon

- Watches all profiles with `autoSync: true`
- Debounces per-file (default 2s) — won't upload mid-save
- Retries with exponential backoff (1s → 2s → 4s, 3 attempts)
- Connection pool with reconnect-on-use
- Batched desktop notifications (groups after 5 files or 30s)
- Hot-reloads config on save — add/remove profiles without restart
- Auto-starts on login after `systemctl --user enable sfync`

## Editor Integration

`sfync current` takes an absolute file path, walks up to find the `.git` root, and uploads relative to the configured `context`. Works with any editor that can invoke shell commands.

### Helix

```toml
[keys.normal.space]
u = ":run-shell-command sfync up myserver %{buffer_name}"
d = ":run-shell-command sfync down myserver %{buffer_name}"
c = ":run-shell-command sfync current myserver %{buffer_name}"
```

## Window Manager Integration

### Niri — floating yazi mount windows

```kdl
window-rule {
    match title="^SFTP-Mount-.*"
    default-floating true
    default-width 1400
    default-height 900
}
```

## Troubleshooting

**Profile not found** — check `~/.config/sfync/config.json` exists and is valid JSON.

**Authentication failed** — verify key path or password. Test with `ssh -i ~/.ssh/key user@host`.

**Server identity changed** — host key mismatch in `~/.ssh/known_hosts`. If expected (server rebuilt), remove the old entry and reconnect.

**SSH key is passphrase-protected** — use an unencrypted key or add it to ssh-agent first.

**Auto-sync not uploading:**
```bash
sfync off && sfync on
journalctl --user -u sfync -f
```
Check `autoSync: true` and that `context` path exists.

**Mount won't unmount:**
```bash
fusermount -u ~/.mounted/profilename
```

## Author

Deppes
