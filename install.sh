#!/bin/sh
# Install stavlos using the Go module's normal verification and build process.
set -eu

tool=stavlos
module=github.com/nicodes/stavlos/cmd/stavlos
release=latest
install_dir=${INSTALL_DIR:-"$HOME/.local/bin"}
work_dir=
stage_dir=
fail() { printf '%s: %s\n' "$tool" "$*" >&2; exit 1; }
cleanup() {
    [ -z "$work_dir" ] || rm -rf "$work_dir"
    [ -z "$stage_dir" ] || rm -rf "$stage_dir"
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|-v|--install-dir)
            [ "$#" -ge 2 ] || fail "$1 needs a value."
            case "$1" in
                --version|-v) release=$2 ;;
                --install-dir) install_dir=$2 ;;
            esac
            shift 2 ;;
        --help|-h)
            printf 'Install %s with Go: [--version VERSION|latest] [--install-dir /absolute/path]\nDefaults: latest Go module, ~/.local/bin. Requires Go.\n' "$tool"
            exit 0 ;;
        *) fail "Unknown option: $1" ;;
    esac
done
case "$install_dir" in /*) ;; *) fail 'The install directory must be an absolute path.' ;; esac
command -v go >/dev/null 2>&1 || fail 'Install Go first, then run this command again.'
case "$release" in
    latest) ;;
    v[0-9]*) ;;
    [0-9]*) release=v$release ;;
    *) fail 'Use latest or a Go module version such as v1.2.3.' ;;
esac
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/$tool-install.XXXXXXXX") || fail 'Could not create a temporary directory.'
mkdir "$work_dir/bin"
printf 'Building %s from %s@%s...\n' "$tool" "$module" "$release"
GOBIN="$work_dir/bin" go install "$module@$release" || fail 'Go installation failed; the installed binary was not changed.'
[ -f "$work_dir/bin/$tool" ] && [ ! -L "$work_dir/bin/$tool" ] || fail 'Go did not produce a regular executable.'
mkdir -p "$install_dir" || fail 'Choose a writable install directory.'
[ ! -d "$install_dir/$tool" ] || fail 'The install destination is a directory.'
stage_dir=$(mktemp -d "$install_dir/.$tool-install.XXXXXXXX") || fail 'Could not stage the executable.'
cp "$work_dir/bin/$tool" "$stage_dir/$tool"
chmod 755 "$stage_dir/$tool"
mv -f "$stage_dir/$tool" "$install_dir/$tool"
printf 'Installed %s at %s/%s\n' "$tool" "$install_dir" "$tool"
case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) printf 'Add %s to your PATH to run %s by name.\n' "$install_dir" "$tool" ;;
esac
