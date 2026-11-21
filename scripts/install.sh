#!/usr/bin/env bash
set -euo pipefail

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' 'Usage: sudo bash install.sh [--version TAG | --bundle DIR] [--non-interactive --config FILE --github-app-key FILE]' '       ephe update [--check | --apply]'; }
version='' bundle='' config='' key='' mode=install
non_interactive=false
while (($#)); do
  case "$1" in
    --version|--bundle|--config|--github-app-key)
      [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
      case "$1" in --version) version=$2;; --bundle) bundle=$2;; --config) config=$2;; --github-app-key) key=$2;; esac; shift 2;;
    --non-interactive) non_interactive=true; shift;;
    --update) mode=stage; shift;;
    --check) [[ "$mode" == stage ]] || die 'unexpected --check'; mode=check; shift;;
    --apply) [[ "$mode" == stage ]] || die 'unexpected --apply'; mode=apply; shift;;
    --help|-h) usage; exit 0;; *) die "unknown option: $1";;
  esac
done
[[ -z "$version" || -z "$bundle" ]] || die 'choose either --version or --bundle'
[[ "$mode" == install || ( -z "$version" && -z "$bundle" && -z "$config" && -z "$key" && "$non_interactive" == false ) ]] || die 'invalid update options'
if [[ -n "$config$key" ]] && { ! "$non_interactive" || [[ -z "$config" || -z "$key" ]]; }; then
  die 'provide --non-interactive, --config and --github-app-key together'
fi
root=/opt/ephemera
current=$root/current
staged=$root/staged
unit=/etc/systemd/system/ephemera.service
destination=/etc/ephemera/config.yaml
credential=/etc/ephemera/credentials/github-app.pem
releases=https://github.com/illusions-lab/Ephemera/releases
fetch=(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2)
online=true
[[ -z "$bundle" ]] || online=false
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) die 'only Linux amd64 and arm64 are supported' ;;
esac
install_cloudflared() {
  local version=2026.11.1
  local digest=97bb3b02d23a5980fc9c15a1a270e692da1923f103f920d5b16d4931d5ab55e7
  local asset=cloudflared-linux-$arch
  if [[ "$arch" == arm64 ]]; then digest=b155cec3e48e489cfb8e5c602769fb8c771083018d8881d68b4d18a791567c5a; fi
  if [[ -x "$root/tools/cloudflared" ]] || command -v cloudflared >/dev/null; then return 0; fi
  printf 'Preparing cloudflared for browser setup...\n'
  "${fetch[@]}" --retry 0 --connect-timeout 8 --max-time 60 -o "$temp/$asset" \
    "https://github.com/cloudflare/cloudflared/releases/download/$version/$asset" || return 1
  (cd "$temp" && printf '%s  %s\n' "$digest" "$asset" | sha256sum -c -) || return 1
  install -d -m 0755 "$root/tools" || return 1
  chmod 0755 "$temp/$asset" && mv -Tf "$temp/$asset" "$root/tools/cloudflared"
}
latest_version() {
  local url
  url=$("${fetch[@]}" --output /dev/null --write-out '%{url_effective}' "$releases/latest") || die 'could not resolve latest release'
  [[ "$url" == "$releases/tag/"* ]] || die 'could not resolve latest release tag'
  printf '%s\n' "${url#"$releases/tag/"}"
}
if [[ "$mode" == check ]]; then
  printf 'Current: '; if [[ -x "$current/ephemera" ]]; then "$current/ephemera" version; elif command -v ephemera >/dev/null; then ephemera version || true; else echo none; fi
  printf 'Staged: '; if [[ -x "$staged/ephemera" ]]; then "$staged/ephemera" version; else echo none; fi
  latest=$(latest_version)
  printf 'Latest: %s\n' "$latest"
  exit
fi
[[ "$(uname -s)" == Linux && "$EUID" -eq 0 ]] || die 'installation requires Linux and root'
if [[ ! -d /run/systemd/system ]] || ! command -v systemctl >/dev/null; then die 'installation requires a running systemd instance'; fi
exec 9>/run/lock/ephemera-install.lock
flock -n 9 || die 'another Ephemera install is in progress'
mkdir -p "$root/releases"
txn=$root/transaction
atomic_link() { ln -sfn "$1" "$2.new"; mv -Tf "$2.new" "$2"; }
rollback() {
  [[ -d "$txn" ]] || return 0
  printf 'Restoring previous installation...\n' >&2
  local failed=false
  # Cancel any start job left behind by an interrupted installer.
  if [[ -f "$unit" ]]; then systemctl stop ephemera.service 9>&- || failed=true; fi
  if [[ -f "$txn/disable" ]]; then
    systemctl disable ephemera.service || die 'failed to undo service enablement; transaction and candidate unit retained for recovery'
    rm -f "$txn/disable"
  fi
  if [[ -f "$txn/previous" ]]; then atomic_link "$(cat "$txn/previous")" "$current" || failed=true; else rm -f "$current" || failed=true; fi
  if [[ -f "$txn/unit" ]]; then install -m 0644 "$txn/unit" "$unit" || failed=true; else rm -f "$unit" || failed=true; fi
  systemctl daemon-reload || failed=true
  if [[ -f "$txn/unit" ]]; then systemctl reset-failed ephemera.service || failed=true; fi
  if [[ -f "$txn/active" ]]; then
    systemctl start ephemera.service 9>&- || failed=true
  fi
  if "$failed"; then die 'rollback failed; transaction and releases retained for repair'; fi
  rm -rf "$txn"
}
rollback
first=true
resume=false
[[ -e "$destination" || -L "$destination" ]] && first=false
if ! "$first"; then
  installed=false
  if [[ -e "$current" || -L "$current" || -e "$unit" || -L "$unit" ]]; then installed=true; fi
  for binary in ephemera ephemerad; do
    path=/usr/local/bin/$binary
    if [[ -e "$path" || -L "$path" ]]; then
      if [[ ! -L "$path" || "$(readlink "$path")" != "$current/$binary" ]]; then installed=true; fi
    fi
  done
  if "$installed"; then
    [[ -z "$config" ]] || die 'installation already exists; omit input files'
  elif "$non_interactive"; then
    [[ -n "$config" ]] || die 'first install retry requires the original config and key'
    resume=true
  fi
  [[ -f "$credential" && ! -L "$credential" && "$(stat -c '%u:%a' "$credential")" == 0:600 ]] || die 'invalid GitHub App private key permissions'
fi
temp=$(mktemp -d "$root/.install.XXXXXXXX")
trap 'rm -rf -- "$temp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ "$mode" != apply ]]; then
  if [[ -z "$bundle" ]]; then
    [[ -n "$version" ]] || version=$(latest_version)
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'invalid release tag'
    if [[ "$mode" == stage && -x "$current/ephemera" ]]; then
      installed=$("$current/ephemera" version | cut -d' ' -f1)
      if [[ "$installed" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [[ "$(printf '%s\n%s\n' "$installed" "$version" | sort -V | tail -n1)" == "$installed" ]]; then
        printf 'Already up to date: %s\n' "$installed"; exit 0
      fi
    fi
    if [[ "$mode" == stage && -x "$staged/ephemera" && "$("$staged/ephemera" version | cut -d' ' -f1)" == "$version" ]]; then
      printf 'Already staged: %s\n' "$version"; exit 0
    fi
    archive=ephemera_linux_$arch.tar.gz
    "${fetch[@]}" -o "$temp/checksums.txt" "$releases/download/$version/checksums.txt"
    expected=$(awk -v asset="$archive" '$2 == asset || $2 == "*" asset { print $1 }' "$temp/checksums.txt")
    [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || die "release $version has no valid checksum for $archive"
    "${fetch[@]}" -o "$temp/$archive" "$releases/download/$version/$archive"
    (cd "$temp" && printf '%s  %s\n' "$expected" "$archive" | sha256sum -c -)
    mkdir "$temp/bundle"
    for file in ephemera ephemerad ephemera.service install.sh; do tar -xOzf "$temp/$archive" "$file" > "$temp/bundle/$file" || die "missing $file"; done
    bundle=$temp/bundle
  fi
  for file in ephemera ephemerad ephemera.service; do [[ -s "$bundle/$file" ]] || die "missing bundle file: $file"; done
  install -m 0755 "$bundle/ephemera" "$temp/ephemera"
  install -m 0755 "$bundle/ephemerad" "$temp/ephemerad"
  identity=$("$temp/ephemera" version)
  [[ "$identity" == "$("$temp/ephemerad" --version)" ]] || die 'binary versions or commits differ'
  [[ -z "$version" || "${identity%% *}" == "$version" ]] || die 'bundle version does not match release tag'
  if "$non_interactive" && { "$first" || "$resume"; }; then
    [[ -r "$config" && -r "$key" ]] || die 'first install requires readable config and key'
    setup_args=(--non-interactive --config "$config" --github-app-key "$key")
    if "$resume"; then setup_args+=(--resume); fi
    "$temp/ephemera" setup "${setup_args[@]}"
  fi
  [[ ! -f "$destination" ]] || "$temp/ephemera" config validate --config "$destination"
  # The content hash keeps development bundles and repeated versions distinct.
  id=$(cat "$temp/ephemera" "$temp/ephemerad" "$bundle/ephemera.service" | sha256sum | cut -c1-16)
  target=$root/releases/$id
  if [[ ! -d "$target" ]]; then
    mkdir "$temp/release"
    cp "$temp/ephemera" "$temp/ephemerad" "$temp/release/"
    install -m 0644 "$bundle/ephemera.service" "$temp/release/ephemera.service"
    install -m 0755 "${bundle}/install.sh" "$temp/release/install.sh" 2>/dev/null || install -m 0755 "$0" "$temp/release/install.sh"
    mv "$temp/release" "$target"
  fi
  atomic_link "$target" "$staged"
  if [[ "$mode" == stage ]] || { [[ "$mode" == install ]] && ! "$first" && ! "$non_interactive"; }; then
    printf 'Staged %s. Run sudo bash %s --update --apply to activate.\n' "$identity" "$target/install.sh"; exit 0
  fi
fi
[[ -x "$staged/ephemera" ]] || die 'no staged release'
[[ ! -f "$destination" ]] || "$staged/ephemera" config validate --config "$destination"
# Preserve an existing flat installation as a rollback target before replacing it.
if [[ ! -L "$current" && -f /usr/local/bin/ephemera && ! -L /usr/local/bin/ephemera ]]; then
  legacy=$root/releases/legacy
  mkdir -p "$legacy"
  cp /usr/local/bin/ephemera "$legacy/ephemera"
  [[ ! -f /usr/local/bin/ephemerad ]] || cp /usr/local/bin/ephemerad "$legacy/ephemerad"
  [[ ! -f "$unit" ]] || cp "$unit" "$legacy/ephemera.service"
  atomic_link "$legacy" "$current"
fi
getent group kvm >/dev/null || groupadd --system kvm
if ! id ephemera >/dev/null 2>&1; then
  useradd --system --user-group --home-dir /var/lib/ephemera --shell /usr/sbin/nologin ephemera
fi
mkdir "$temp/transaction"
[[ ! -L "$current" ]] || readlink "$current" > "$temp/transaction/previous"
[[ ! -f "$unit" ]] || cp "$unit" "$temp/transaction/unit"
if systemctl is-active --quiet ephemera.service; then touch "$temp/transaction/active"; fi
mv "$temp/transaction" "$txn"
atomic_link "$(readlink "$staged")" "$current"
for binary in ephemera ephemerad; do atomic_link "$current/$binary" "/usr/local/bin/$binary"; done
if [[ -e /usr/local/bin/ephe || -L /usr/local/bin/ephe ]]; then
  [[ -L /usr/local/bin/ephe && "$(readlink /usr/local/bin/ephe)" == ephemera ]] || { rollback; die 'ephe alias already exists'; }
else ln -s ephemera /usr/local/bin/ephe; fi
install -m 0644 "$current/ephemera.service" "$unit"
if ! systemctl daemon-reload; then rollback; die 'daemon-reload failed'; fi
if "$first" && ! "$non_interactive" && [[ "$mode" == install ]]; then
  rm -rf "$txn"; rm -f "$staged"
  printf 'Ephemera installed.\n'
  if ( : <> /dev/tty ) 2>/dev/null; then
    if "$online" && ! install_cloudflared; then
      printf 'Cloudflare dependency unavailable. Local browser and SSH setup are available.\n' >&2
    fi
    flock -u 9
    /usr/local/bin/ephe setup </dev/tty >/dev/tty 2>&1
  else
    printf 'Run sudo ephe setup from a terminal to configure and start it.\n'
  fi
  exit 0
fi
if "$non_interactive"; then
  if [[ ! -f "$txn/previous" && ! -f "$txn/unit" ]]; then touch "$txn/disable"; fi
  systemctl enable ephemera.service || { rollback; die 'failed to enable service'; }
fi
if ! systemctl restart ephemera.service 9>&- || ! systemctl is-active --quiet ephemera.service; then
  rollback; die 'service failed to become ready; previous installation restored'
fi
rm -rf "$txn"; rm -f "$staged"
printf 'Ephemera is active: %s\n' "$("$current/ephemera" version)"
