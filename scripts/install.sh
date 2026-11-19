#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'HELP'
Usage: sudo bash install.sh [--version TAG | --bundle DIR]
       [--non-interactive --config FILE --github-app-key FILE]

Install Ephemera on a Debian systemd host. By default, download the latest
GitHub release and run the setup wizard on first install. Pass --bundle for
an unpacked release containing ephemera and ephemera.service.
Existing configuration is reused on repeat installs.
HELP
}

die() {
  printf 'install: %s\n' "$*" >&2
  exit 1
}

version=
bundle=
config=
key=
non_interactive=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version|--bundle|--config|--github-app-key)
      [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
      case "$1" in
        --version) version=$2 ;;
        --bundle) bundle=$2 ;;
        --config) config=$2 ;;
        --github-app-key) key=$2 ;;
      esac
      shift 2
      ;;
    --non-interactive) non_interactive=true; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[[ -z "$version" || -z "$bundle" ]] || die 'choose either --version or --bundle'
if [[ -n "$config" || -n "$key" ]]; then
  "$non_interactive" && [[ -n "$config" && -n "$key" ]] ||
    die 'provide --non-interactive, --config and --github-app-key together'
fi
[[ "$(uname -s)" == Linux ]] || die 'installation requires Linux'
[[ "$EUID" -eq 0 ]] || die 'run this script with sudo'
if [[ ! -d /run/systemd/system ]] || ! command -v systemctl >/dev/null; then
  die 'installation requires a running systemd instance'
fi
exec 9>/run/lock/ephemera-install.lock
flock -n 9 || die 'another Ephemera install is in progress'

destination=/etc/ephemera/config.yaml
credential=/etc/ephemera/credentials/github-app.pem
first_install=false
if [[ -e "$destination" || -L "$destination" ]]; then
  [[ -z "$config" ]] || die "configuration already exists at $destination; omit the input files to upgrade"
  [[ -f "$credential" && ! -L "$credential" && "$(stat -c '%u:%a' "$credential")" == 0:600 ]] ||
    die "expected a root-owned private key with mode 0600 at $credential"
else
  first_install=true
  if "$non_interactive"; then
    [[ -r "$config" && -r "$key" ]] || die 'first install requires readable --config and --github-app-key files'
  else
    [[ -t 0 && -t 1 ]] || die 'use a terminal or provide non-interactive input files'
  fi
fi

temp=$(mktemp -d)
trap 'rm -r -- "$temp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -z "$bundle" ]]; then
  for command in curl sha256sum tar; do
    command -v "$command" >/dev/null || die "required command missing: $command"
  done
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) die 'unsupported CPU architecture' ;;
  esac
  releases=https://github.com/illusions-lab/Ephemera/releases
  fetch=(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2)
  if [[ -z "$version" ]]; then
    latest=$("${fetch[@]}" --output /dev/null --write-out '%{url_effective}' "$releases/latest")
    [[ "$latest" == "$releases/tag/"* ]] || die 'could not resolve the latest release tag'
    version=${latest#"$releases/tag/"}
  fi
  [[ "$version" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || die 'invalid release tag'
  archive="ephemera_linux_$arch.tar.gz"
  "${fetch[@]}" --output "$temp/checksums.txt" "$releases/download/$version/checksums.txt"
  "${fetch[@]}" --output "$temp/$archive" "$releases/download/$version/$archive"
  expected=$(awk -v asset="$archive" '$2 == asset || $2 == "*" asset { print $1 }' "$temp/checksums.txt")
  [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || die "expected one SHA-256 digest for $archive"
  (cd "$temp" && printf '%s  %s\n' "$expected" "$archive" | sha256sum -c -)
  mkdir "$temp/bundle"
  tar -xOzf "$temp/$archive" ephemera > "$temp/bundle/ephemera"
  tar -xOzf "$temp/$archive" ephemera.service > "$temp/bundle/ephemera.service"
  bundle="$temp/bundle"
fi

for file in ephemera ephemera.service; do
  [[ -f "$bundle/$file" && -s "$bundle/$file" ]] || die "missing or empty bundle file: $file"
done
install -m 0755 -- "$bundle/ephemera" "$temp/ephemera"
if "$first_install"; then
  args=(setup)
  if "$non_interactive"; then
    args+=(--non-interactive --config "$config" --github-app-key "$key")
  fi
  "$temp/ephemera" "${args[@]}"
fi
"$temp/ephemera" config validate --config "$destination"

install -D -m 0755 -- "$temp/ephemera" /usr/local/bin/ephemera
install -D -m 0644 -- "$bundle/ephemera.service" /etc/systemd/system/ephemera.service
systemctl daemon-reload
systemctl enable ephemera.service
if ! systemctl restart ephemera.service || ! systemctl is-active --quiet ephemera.service; then
  systemctl status ephemera.service --no-pager >&2 || true
  die 'service failed to start; see journalctl -u ephemera'
fi
printf 'Ephemera is active. Configuration: %s\n' "$destination"
