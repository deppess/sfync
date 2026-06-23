package cmd

import (
	"fmt"
	"sort"

	"github.com/deppess/sfync/internal/config"
)

// ListProfiles prints profile names from config, one per line, sorted.
// Used by shell completion scripts at completion time.
// Exits cleanly with no output when the config is missing or unreadable.
func ListProfiles() error {
	cfg, err := config.Load()
	if err != nil {
		return nil // silently suppress — completion scripts expect exit 0 + empty output
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
	return nil
}

// Completions prints a shell completion script for the given shell to stdout.
func Completions(shell string) error {
	switch shell {
	case "fish":
		fmt.Print(fishCompletion)
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	default:
		return fmt.Errorf("unknown shell %q — supported: fish, bash, zsh", shell)
	}
	return nil
}

const fishCompletion = `# fish completion for sfync
# Install: sfync --completions fish > ~/.config/fish/completions/sfync.fish

function __sfync_profiles
    sfync --list-profiles 2>/dev/null
end

function __sfync_needs_subcommand
    test (count (commandline -opc)) -eq 1
end

function __sfync_at
    # True when word at position 2 equals $argv[1]
    set -l w (commandline -opc)
    test (count $w) -ge 2; and test "$w[2]" = "$argv[1]"
end

function __sfync_at_any
    # True when word at position 2 is one of $argv
    set -l w (commandline -opc)
    test (count $w) -ge 2; and contains -- $w[2] $argv
end

function __sfync_diff_needs_profile
    set -l w (commandline -opc)
    test (count $w) -eq 3; and test "$w[2]" = diff; and contains -- $w[3] up down
end

function __sfync_mount_needs_flag
    set -l w (commandline -opc)
    test (count $w) -eq 3; and test "$w[2]" = mount
end

function __sfync_needs_file
    set -l w (commandline -opc)
    test (count $w) -eq 3; and contains -- $w[2] push pull
end

# Subcommands
complete -c sfync -f -n __sfync_needs_subcommand -a up              -d 'Mirror local → remote'
complete -c sfync -f -n __sfync_needs_subcommand -a down            -d 'Mirror remote → local'
complete -c sfync -f -n __sfync_needs_subcommand -a diff            -d 'Dry-run preview'
complete -c sfync -f -n __sfync_needs_subcommand -a push            -d 'Upload single file'
complete -c sfync -f -n __sfync_needs_subcommand -a pull            -d 'Download single file'
complete -c sfync -f -n __sfync_needs_subcommand -a current         -d 'Upload current file (editor)'
complete -c sfync -f -n __sfync_needs_subcommand -a mount           -d 'Mount remote via FUSE'
complete -c sfync -f -n __sfync_needs_subcommand -a unmount         -d 'Unmount profile'
complete -c sfync -f -n __sfync_needs_subcommand -a mounts          -d 'List mounted profiles'
complete -c sfync -f -n __sfync_needs_subcommand -a daemon          -d 'Run auto-sync daemon'
complete -c sfync -f -n __sfync_needs_subcommand -a install-daemon  -d 'Install systemd service'
complete -c sfync -f -n __sfync_needs_subcommand -a uninstall-daemon -d 'Remove systemd service'
complete -c sfync -f -n __sfync_needs_subcommand -a on              -d 'Start daemon'
complete -c sfync -f -n __sfync_needs_subcommand -a off             -d 'Stop daemon'
complete -c sfync -f -n __sfync_needs_subcommand -a version         -d 'Show version'
complete -c sfync -f -n __sfync_needs_subcommand -a help            -d 'Show help'

# Profile at arg 3 for single-profile commands
complete -c sfync -f -n '__sfync_at_any up down push pull current mount' -a '(__sfync_profiles)'

# unmount: profiles + --all
complete -c sfync -f -n '__sfync_at unmount' -a '(__sfync_profiles)'
complete -c sfync -f -n '__sfync_at unmount' -a --all -d 'Unmount all profiles'

# diff: direction at arg 3
complete -c sfync -f -n '__sfync_at diff' -a up   -d 'Show what would be uploaded'
complete -c sfync -f -n '__sfync_at diff' -a down -d 'Show what would be downloaded'

# diff: profile at arg 4 (after direction)
complete -c sfync -f -n __sfync_diff_needs_profile -a '(__sfync_profiles)'

# mount --yazi at arg 4
complete -c sfync -f -n __sfync_mount_needs_flag -a --yazi -d 'Open in yazi file manager'

# push/pull: file completion at arg 4
complete -c sfync -F -n __sfync_needs_file
`

const bashCompletion = `# bash completion for sfync
# Install (user):   sfync --completions bash > ~/.local/share/bash-completion/completions/sfync
# Install (system): sfync --completions bash > /usr/share/bash-completion/completions/sfync
# Requires: bash-completion package

_sfync() {
    local cur prev words cword
    _init_completion || return

    local subcommands="up down diff push pull current mount unmount mounts daemon install-daemon uninstall-daemon on off version help"

    case $cword in
        1)
            COMPREPLY=($(compgen -W "$subcommands" -- "$cur"))
            ;;
        2)
            case ${words[1]} in
                up|down|push|pull|current|mount)
                    local profiles
                    profiles=$(sfync --list-profiles 2>/dev/null)
                    COMPREPLY=($(compgen -W "$profiles" -- "$cur"))
                    ;;
                unmount)
                    local profiles
                    profiles=$(sfync --list-profiles 2>/dev/null)
                    COMPREPLY=($(compgen -W "$profiles --all" -- "$cur"))
                    ;;
                diff)
                    COMPREPLY=($(compgen -W "up down" -- "$cur"))
                    ;;
            esac
            ;;
        3)
            case ${words[1]} in
                push|pull|current)
                    _filedir
                    ;;
                mount)
                    COMPREPLY=($(compgen -W "--yazi" -- "$cur"))
                    ;;
                diff)
                    local profiles
                    profiles=$(sfync --list-profiles 2>/dev/null)
                    COMPREPLY=($(compgen -W "$profiles" -- "$cur"))
                    ;;
            esac
            ;;
    esac
}

complete -F _sfync sfync
`

const zshCompletion = `#compdef sfync
# zsh completion for sfync
# Install (user):   sfync --completions zsh > ~/.zfunc/_sfync
#                   (ensure ~/.zfunc is in $fpath and compinit is called)
# Install (system): sfync --completions zsh > /usr/share/zsh/site-functions/_sfync

_sfync() {
    local state line
    typeset -A opt_args

    _arguments -C \
        '1: :_sfync_subcommands' \
        '*:: :->args'

    case $state in
        args)
            case $words[1] in
                up|down)
                    _arguments '1: :_sfync_profiles'
                    ;;
                push|pull|current)
                    _arguments '1: :_sfync_profiles' '2: :_files'
                    ;;
                mount)
                    _arguments '1: :_sfync_profiles' '--yazi[Open in yazi file manager]'
                    ;;
                unmount)
                    _arguments '1: :_sfync_unmount_target'
                    ;;
                diff)
                    _arguments \
                        '1: :(up down)' \
                        '2: :_sfync_profiles'
                    ;;
            esac
            ;;
    esac
}

_sfync_subcommands() {
    local -a cmds
    cmds=(
        'up:Mirror local → remote'
        'down:Mirror remote → local'
        'diff:Dry-run preview'
        'push:Upload single file'
        'pull:Download single file'
        'current:Upload current file (editor integration)'
        'mount:Mount remote filesystem via FUSE'
        'unmount:Unmount a profile'
        'mounts:List mounted profiles'
        'daemon:Run auto-sync daemon'
        'install-daemon:Install systemd user service'
        'uninstall-daemon:Remove systemd user service'
        'on:Start the daemon'
        'off:Stop the daemon'
        'version:Show version'
        'help:Show help'
    )
    _describe 'command' cmds
}

_sfync_profiles() {
    local -a profiles
    profiles=(${(f)"$(sfync --list-profiles 2>/dev/null)"})
    _describe 'profile' profiles
}

_sfync_unmount_target() {
    local -a items
    items=(${(f)"$(sfync --list-profiles 2>/dev/null)"})
    items+='--all:Unmount all profiles'
    _describe 'target' items
}

_sfync
`
