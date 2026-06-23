# sfync

SFTP/FTP sync, mounting, and auto-upload daemon for Linux — bidirectional mirror sync, single-file push/pull, FUSE mounting, and editor integration.

## Dependencies

```
libnotify
sshfs  rclone   # mounting only
```

## Install

```bash
go build -o sfync .
sudo cp sfync /usr/local/bin/sfync
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

`password` or `sshKey` required for SFTP. FTP requires `password`. Passwords are stored in plaintext — `chmod 600 ~/.config/sfync/config.json`.

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
sfync mount <profile>           mount remote via FUSE
sfync mount <profile> --yazi    mount and open in yazi
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

```bash
journalctl --user -u sfync -f
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
