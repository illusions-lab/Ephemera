#!/bin/bash
set -euo pipefail

die() { printf 'install-macos: %s\n' "$*" >&2; exit 1; }
root="$HOME/Library/Application Support/Ephemera"
label=com.illusions-lab.ephemera
domain="gui/$(id -u)"
plist="$HOME/Library/LaunchAgents/$label.plist"
journal="$root/.install-transaction"
releases=https://github.com/illusions-lab/Ephemera/releases
fetch=(/usr/bin/curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2)

fetch_tart() {
  local tart="$root/tools/tart.app/Contents/MacOS/tart"
  if [[ -x "$tart" && "$("$tart" --version)" == 2.42.0 ]]; then return; fi
  [[ -z "$bundle" ]] || die 'offline bundle requires Tart 2.42.0 under Ephemera/tools/tart.app'
  [[ ! -e "$root/tools/tart.app" ]] || die 'move the existing Tart.app aside before installing the pinned version'
  "${fetch[@]}" -o "$work/tart.tar.gz" https://github.com/openai/tart/releases/download/2.42.0/tart.tar.gz
  (cd "$work" && printf '%s  %s\n' a95dda214b5f676298430d665a8174992e6b300dd20c653385b60481ae819ddf tart.tar.gz | /usr/bin/shasum -a 256 -c -)
  tar -xzf "$work/tart.tar.gz" -C "$work"
  [[ "$("$work/tart.app/Contents/MacOS/tart" --version)" == 2.42.0 ]] || die 'unexpected Tart version'
  mkdir -p "$root/tools"
  mv "$work/tart.app" "$root/tools/tart.app"
}

set_current() {
  rm -f "$work/current"
  ln -s "$1" "$work/current"
  /bin/mv -fh "$work/current" "$root/current"
}

rollback_install() {
  local previous
  previous=$(cat "$work/previous") || return 1
  if launchctl print "$domain/$label" >/dev/null 2>&1; then
    launchctl bootout "$domain/$label" || return 1
  fi
  if [[ -n "$previous" ]]; then set_current "$previous" || return 1
  else rm -f "$root/current" || return 1; fi
  if [[ -f "$work/previous.plist" ]]; then cp "$work/previous.plist" "$plist" || return 1
  else rm -f "$plist" || return 1; fi
  if [[ -f "$work/was-running" ]]; then launchctl bootstrap "$domain" "$plist" || return 1; fi
  return 0
}

finish_install() {
  local result=$?
  if [[ -f "$journal" ]]; then
    if ! rollback_install; then
      printf 'install-macos: could not restore the previous installation; recovery files remain in %s\n' "$work" >&2
      return 1
    fi
    rm -f "$journal"
  fi
  rm -rf "$work"
  return "$result"
}

ensure_cli_links() {
  local name
  for name in ephemera ephe; do
    if [[ ! -L "$HOME/.local/bin/$name" ]]; then
      ln -s "$root/current/ephemera" "$HOME/.local/bin/$name"
    fi
  done
}

install_mac() {
  version='' bundle='' config='' key=''
  local automatic=false existing_app=false app_name='' previous target identity digest archive name snapshot
  while (($#)); do
    case "$1" in
      --version|--bundle|--config|--github-app-key|--app-name)
        [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
        case "$1" in
          --version) version=$2 ;; --bundle) bundle=$2 ;; --config) config=$2 ;;
          --github-app-key) key=$2 ;; --app-name) app_name=$2 ;;
        esac
        shift 2 ;;
      --non-interactive) automatic=true; shift ;;
      --existing-app) existing_app=true; shift ;;
      --help|-h) printf 'Usage: install-macos.sh [--version TAG | --bundle DIR] [--existing-app | --app-name NAME | --non-interactive --config FILE --github-app-key FILE]\n'; return ;;
      *) die "unknown option: $1" ;;
    esac
  done
  [[ -z "$version" || -z "$bundle" ]] || die 'choose either --bundle or --version'
  if "$automatic"; then
    [[ -n "$config" && -n "$key" ]] || die 'supply configuration and key together'
    [[ "$existing_app" == false && -z "$app_name" ]] || die 'interactive App options require a terminal setup'
  else
    [[ -z "$config$key" ]] || die 'input files require --non-interactive'
    [[ "$existing_app" == false || -z "$app_name" ]] || die 'choose either --existing-app or --app-name'
  fi
  launchctl print "$domain" >/dev/null 2>&1 || die 'log in to the macOS desktop with this account first'
  if launchctl print "$domain/$label.dev" >/dev/null 2>&1; then
    die 'stop the development LaunchAgent before installing Ephemera'
  fi
  if [[ -f "$journal" ]]; then
    work=$(cat "$journal")
    [[ "$work" == "$root"/.install.?????? && -d "$work" ]] || die 'invalid installation recovery record'
    rollback_install || die "could not restore the previous installation from $work"
    rm -f "$journal"
    rm -rf "$work"
    printf 'Recovered the previous installation.\n'
  fi
  work=$(mktemp -d "$root/.install.XXXXXX")
  trap finish_install EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  if [[ -z "$bundle" ]]; then
    if [[ -z "$version" ]]; then
      version=$("${fetch[@]}" -o /dev/null -w '%{url_effective}' "$releases/latest")
      [[ "$version" == "$releases/tag/"* ]] || die 'unexpected latest release URL'
      version=${version#"$releases/tag/"}
    fi
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'invalid release tag'
    archive=ephemera_darwin_arm64.tar.gz
    "${fetch[@]}" -o "$work/checksums.txt" "$releases/download/$version/checksums.txt"
    digest=$(awk -v asset="$archive" '$2 == asset || $2 == "*" asset {print $1}' "$work/checksums.txt")
    [[ "$digest" =~ ^[[:xdigit:]]{64}$ ]] || die "$version has no verified macOS arm64 bundle"
    "${fetch[@]}" -o "$work/$archive" "$releases/download/$version/$archive"
    (cd "$work" && printf '%s  %s\n' "$digest" "$archive" | /usr/bin/shasum -a 256 -c -)
    mkdir "$work/bundle"
    for name in ephemera ephemerad activate-macos.sh server-config.sh; do
      tar -xOzf "$work/$archive" "$name" > "$work/bundle/$name"
    done
    bundle="$work/bundle"
    # Online installation can fetch the pinned Tart distribution.
    online=true
  else
    online=false
  fi
  mkdir "$work/candidate"
  for name in ephemera ephemerad activate-macos.sh server-config.sh; do
    [[ -s "$bundle/$name" ]] || die "missing $name"
    install -m 0700 "$bundle/$name" "$work/candidate/$name"
  done
  identity=$("$work/candidate/ephemera" version)
  [[ "$identity" == "$("$work/candidate/ephemerad" --version)" ]] || die 'bundle versions or commits differ'
  [[ -z "$version" || "${identity%% *}" == "$version" ]] || die 'bundle version does not match release tag'

  if "$online"; then bundle=''; fi
  fetch_tart
  export PATH="$root/tools/tart.app/Contents/MacOS:$PATH"
  target="$root/releases/$(/usr/bin/shasum -a 256 "$work/candidate/"* | awk '{print $1}' | /usr/bin/shasum -a 256 | cut -c1-32)"
  mkdir -p "$root/releases" "$HOME/.local/bin"
  if [[ ! -d "$target" ]]; then mv "$work/candidate" "$target"; fi
  previous=$(readlink "$root/current" || true)
  for name in ephemera ephe; do
    if [[ -e "$HOME/.local/bin/$name" || -L "$HOME/.local/bin/$name" ]]; then
      [[ "$(readlink "$HOME/.local/bin/$name")" == "$root/current/ephemera" ]] || die "$name already exists in ~/.local/bin"
    fi
  done
  if ! "$automatic" && [[ "$target" == "$previous" && -f "$root/config.yaml" ]] &&
     launchctl print "$domain/$label" >/dev/null 2>&1; then
    snapshot=$("$root/current/ephemera" status --json 2>/dev/null || true)
    if [[ "$snapshot" == *'"github":"connected"'* ]]; then
      ensure_cli_links
      printf 'Ephemera is already running.\n'
      return
    fi
  fi
  if [[ -f "$plist" ]]; then cp "$plist" "$work/previous.plist"; fi
  if launchctl print "$domain/$label" >/dev/null 2>&1; then touch "$work/was-running"; fi
  printf '%s\n' "$previous" > "$work/previous"
  printf '%s\n' "$work" > "$work/journal"
  mv "$work/journal" "$journal"
  set_current "$target"
  local options=()
  if "$automatic"; then options=(--non-interactive --config "$config" --github-app-key "$key"); fi
  if "$existing_app"; then options=(--existing-app); fi
  if [[ -n "$app_name" ]]; then options=(--app-name "$app_name"); fi
  if ! "$target/ephemera" setup ${options[@]+"${options[@]}"} || [[ ! -f "$root/config.yaml" ]]; then
    die 'setup did not finish; restoring the previous installation'
  fi
  rm -f "$journal"
  ensure_cli_links
  case ":$PATH:" in
    *":$HOME/.local/bin:"*) ;;
    *) printf 'Add %s to PATH to use ephe.\n' "$HOME/.local/bin" ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then install_mac "$@"; exit; fi
  [[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 && "$EUID" -ne 0 ]] || die 'run as your logged-in user on Apple Silicon macOS'
  os_version=$(sw_vers -productVersion)
  [[ "${os_version%%.*}" -ge 26 ]] || die 'macOS 26 or newer is required'
  umask 077
  mkdir -p "$root"
  [[ ! -L "$root" && "$(stat -f '%u:%Lp' "$root")" == "$(id -u):700" ]] || die 'installation needs a private user-owned data directory (0700)'
  # shellcheck disable=SC2016

  /usr/bin/lockf -k -t 0 "$root/.install.lock" /bin/bash -c 'source "$1"; shift; install_mac "$@"' bash "$0" "$@"
fi
