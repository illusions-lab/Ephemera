#!/bin/bash
set -euo pipefail

die() { printf 'install-macos: %s\n' "$*" >&2; exit 1; }
root="$HOME/Library/Application Support/Ephemera"
releases=https://github.com/illusions-lab/Ephemera/releases
fetch=(/usr/bin/curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2)
bundle_files=(ephemera ephemerad server-config.sh tart-manifest.tsv)

stage_tart() {
  local manifest=$1 online=$2 tart_version tart_url tart_digest extra entry tools relative
  IFS=$'\t' read -r tart_version tart_url tart_digest extra < "$manifest"
  [[ "$tart_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ &&
     "$tart_url" == https://* && "$tart_digest" =~ ^[a-f0-9]{64}$ && -z "$extra" ]] ||
    die 'invalid Tart manifest'
  entry="$tart_version-$tart_digest"
  tools="$root/tools/tart"
  relative=tart.app/Contents/MacOS/tart
  if ! "$online" && [[ ! -d "$tools/$entry" ]]; then
    tools="$root/tools/tart-prebuilt"
  fi
  if [[ ! -d "$tools/$entry" ]]; then
    mkdir -p "$work/dependency/$entry" "$tools"
    if "$online"; then
      "${fetch[@]}" -o "$work/tart.tar.gz" "$tart_url"
      (cd "$work" && printf '%s  %s\n' "$tart_digest" tart.tar.gz | /usr/bin/shasum -a 256 -c -)
      tar -xzf "$work/tart.tar.gz" -C "$work/dependency/$entry"
    else
      [[ -x "$root/tools/$relative" && "$("$root/tools/$relative" --version)" == "$tart_version" ]] ||
        die "offline bundle requires Tart $tart_version at $root/tools/tart.app"
      /usr/bin/ditto "$root/tools/tart.app" "$work/dependency/$entry/tart.app"
    fi
    [[ "$("$work/dependency/$entry/$relative" --version)" == "$tart_version" ]] ||
      die "staged Tart does not match version $tart_version"
    # Move into the parent so simultaneous installers cannot nest one copy in another.
    /bin/mv -n "$work/dependency/$entry" "$tools/"
  fi
  tart="$tools/$entry/$relative"
  [[ -x "$tart" && "$("$tart" --version)" == "$tart_version" ]] ||
    die "installed Tart at $tart does not match $tart_version"
}

install_cli_links() {
  local name path target="$root/current/ephemera"
  for name in ephemera ephe; do
    path="$HOME/.local/bin/$name"
    [[ "$(readlink "$path" || true)" == "$target" ]] && continue
    if [[ ! -e "$path" && ! -L "$path" ]]; then
      /bin/ln -sh "$target" "$path" || true
    fi
    [[ "$(readlink "$path" || true)" == "$target" ]] || die "Ephemera is active but could not create $path"
  done
}

install_mac() {
  local version='' bundle='' config='' key='' app_name='' online=false
  local automatic=false existing_app=false target identity digest archive name tart entry path
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
  printf 'Current: '
  if [[ -x "$root/current/ephemera" ]]; then
    "$root/current/ephemera" version || printf 'unavailable\n'
  else
    printf 'none\n'
  fi
  work=$(mktemp -d "$root/.install.XXXXXX")
  trap 'rm -rf "$work"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  if [[ -z "$bundle" ]]; then
    if [[ -z "$version" ]]; then
      version=$("${fetch[@]}" -o /dev/null -w '%{url_effective}' "$releases/latest")
      [[ "$version" == "$releases/tag/"* ]] || die 'unexpected latest release URL'
      version=${version#"$releases/tag/"}
    fi
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'invalid release tag'
    printf 'Target: %s\n' "$version"
    archive=ephemera_darwin_arm64.tar.gz
    "${fetch[@]}" -o "$work/checksums.txt" "$releases/download/$version/checksums.txt"
    digest=$(awk -v asset="$archive" '$2 == asset || $2 == "*" asset {print $1}' "$work/checksums.txt")
    [[ "$digest" =~ ^[[:xdigit:]]{64}$ ]] || die "$version has no verified macOS arm64 bundle"
    "${fetch[@]}" -o "$work/$archive" "$releases/download/$version/$archive"
    (cd "$work" && printf '%s  %s\n' "$digest" "$archive" | /usr/bin/shasum -a 256 -c -)
    bundle="$work/bundle"
    mkdir "$bundle"
    for name in "${bundle_files[@]}"; do
      tar -xOzf "$work/$archive" "$name" > "$bundle/$name"
    done
    online=true
  fi

  mkdir "$work/candidate"
  for name in "${bundle_files[@]}"; do
    [[ -s "$bundle/$name" ]] || die "missing $name"
    install -m 0700 "$bundle/$name" "$work/candidate/$name"
  done
  identity=$("$work/candidate/ephemera" version)
  if ! "$online"; then printf 'Target: %s\n' "$identity"; fi
  [[ -n "$identity" && "$identity" == "$("$work/candidate/ephemerad" --version)" ]] ||
    die 'bundle versions or commits differ'
  [[ -z "$version" || "${identity%% *}" == "$version" ]] || die 'bundle version does not match release tag'
  "$work/candidate/ephemera" tart-manifest > "$work/cli-manifest"
  "$work/candidate/ephemerad" --tart-manifest > "$work/daemon-manifest"
  if ! cmp "$work/candidate/tart-manifest.tsv" "$work/cli-manifest" ||
     ! cmp "$work/candidate/tart-manifest.tsv" "$work/daemon-manifest"; then
    die 'Tart manifest does not match the candidate binaries'
  fi
  stage_tart "$work/candidate/tart-manifest.tsv" "$online"
  entry=$({
    for name in "${bundle_files[@]}"; do
      /usr/bin/shasum -a 256 "$work/candidate/$name" | cut -d ' ' -f 1
    done
    printf '%s\n' "$tart"
  } | /usr/bin/shasum -a 256 | cut -d ' ' -f 1)
  ln -s "$tart" "$work/candidate/tart"
  mv "$work/candidate" "$work/$entry"
  mkdir -p "$root/releases" "$HOME/.local/bin"
  target="$root/releases/$entry"
  /bin/mv -n "$work/$entry" "$root/releases/"
  for name in "${bundle_files[@]}"; do
    cmp "$bundle/$name" "$target/$name" || die "existing release differs at $target/$name"
  done
  [[ "$(readlink "$target/tart")" == "$tart" ]] || die "existing release has a different Tart at $target/tart"
  for name in ephemera ephe; do
    path="$HOME/.local/bin/$name"
    if [[ -e "$path" || -L "$path" ]]; then
      [[ "$(readlink "$path" || true)" == "$root/current/ephemera" ]] || die "$name already exists in ~/.local/bin"
    fi
  done

  local options=()
  if "$automatic"; then options=(--non-interactive --config "$config" --github-app-key "$key"); fi
  if "$existing_app"; then options=(--existing-app); fi
  if [[ -n "$app_name" ]]; then options=(--app-name "$app_name"); fi
  "$target/ephemera" setup ${options[@]+"${options[@]}"}
  [[ "$(cd "$root/current" 2>/dev/null && pwd -P)" == "$(cd "$target" && pwd -P)" ]] ||
    die 'setup ended without activating the staged release'
  install_cli_links
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
  install_mac "$@"
fi
