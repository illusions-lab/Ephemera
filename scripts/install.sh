#!/usr/bin/env bash
set -euo pipefail

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' 'Usage: sudo bash install.sh [--version TAG | --bundle DIR] [--non-interactive --config FILE --github-app-key FILE]' '       ephe update [--check | --apply]'; }
recovery_mode=
apply_yes=false
case "${1:-}" in
  --recover|--recover-boot|--guard-start|--apply-config) recovery_mode=$1; shift;;
esac
version='' bundle='' config='' key='' mode=install
non_interactive=false
while (($#)); do
  case "$1" in
    --version|--bundle|--config|--github-app-key)
      [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
      case "$1" in --version) version=$2;; --bundle) bundle=$2;; --config) config=$2;; --github-app-key) key=$2;; esac; shift 2;;
    --yes) [[ "$recovery_mode" == --apply-config ]] || die 'unexpected --yes'; apply_yes=true; shift;;
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
active_config=/etc/ephemera/active
activations=/etc/ephemera/activations
releases=https://github.com/illusions-lab/Ephemera/releases
fetch=(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2)
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) die 'only Linux amd64 and arm64 are supported' ;;
esac
latest_version() {
  local url
  url=$("${fetch[@]}" --output /dev/null --write-out '%{url_effective}' "$releases/latest") || die 'could not resolve latest release'
  [[ "$url" == "$releases/tag/"* ]] || die 'could not resolve latest release tag'
  printf '%s\n' "${url#"$releases/tag/"}"
}
show_current_version() {
  printf 'Current: '
  if [[ -x "$current/ephemera" ]]; then
    "$current/ephemera" version || printf 'unavailable\n'
  elif [[ -x /usr/local/bin/ephemera ]]; then
    /usr/local/bin/ephemera version || printf 'unavailable\n'
  elif [[ "$mode" == check ]] && command -v ephemera >/dev/null; then
    ephemera version || printf 'unavailable\n'
  else
    printf 'none\n'
  fi
}
if [[ "$mode" == check ]]; then
  show_current_version
  printf 'Staged: '; if [[ -x "$staged/ephemera" ]]; then "$staged/ephemera" version; else echo none; fi
  latest=$(latest_version)
  printf 'Latest: %s\n' "$latest"
  exit
fi
[[ "$(uname -s)" == Linux && "$EUID" -eq 0 ]] || die 'installation requires Linux and root'
if [[ ! -d /run/systemd/system ]] || ! command -v systemctl >/dev/null; then die 'installation requires a running systemd instance'; fi
if [[ -z "$recovery_mode" ]]; then show_current_version; fi
mkdir -p /run/lock
exec 9>/run/lock/ephemera-install.lock
if ! flock -n 9; then
  [[ "$recovery_mode" == --guard-start ]] && exit 0
  die 'another Ephemera install is in progress'
fi
mkdir -p "$root/releases"
txn=$root/transaction
atomic_link() { ln -sfn "$1" "$2.new"; mv -Tf "$2.new" "$2"; }
restore_files() {
  if [[ -f "$txn/restore-config" ]]; then
    if [[ -f "$txn/previous-config" ]]; then
      atomic_link "$(cat "$txn/previous-config")" "$active_config" || return
    else
      rm -f "$active_config" || return
    fi
    sync -f /etc/ephemera || return
  fi
  if [[ -f "$txn/disable" ]]; then
    rm -f /etc/systemd/system/multi-user.target.wants/ephemera.service || return
  fi
  if [[ -f "$txn/previous" ]]; then
    atomic_link "$(cat "$txn/previous")" "$current" || return
  else
    rm -f "$current" || return
  fi
  if [[ -f "$txn/unit" ]]; then
    install -m 0644 "$txn/unit" "$unit.recovery" && mv -Tf "$unit.recovery" "$unit" || return
  else
    rm -f "$unit" || return
  fi
  sync -f "$root" && sync -f "$(dirname "$unit")"
}
finish_transaction() {
  # Rename is the commit point and leaves a recoverable journal until then.
  sync -f /etc/systemd/system && mv -Tf "$txn" "$temp/finished-transaction" && sync -f "$root"
}
rollback() {
  [[ -d "$txn" ]] || return 0
  printf 'Restoring previous installation...\n' >&2
  if [[ -f "$txn/switching" ]]; then
    systemctl stop ephemera.service 9>&- || return
    if [[ -f "$txn/disable" ]]; then
      systemctl disable ephemera.service || return
    fi
    restore_files || return
    systemctl daemon-reload || return
    if [[ -f "$txn/unit" ]]; then systemctl reset-failed ephemera.service || return; fi
  fi
  if [[ -f "$txn/active" ]]; then
    systemctl start ephemera.service 9>&- || return
  fi
  finish_transaction
}
if [[ "$recovery_mode" == --guard-start ]]; then
  if [[ -d "$txn" ]]; then
    flock -u 9
    systemctl --no-block start ephemera-recover.service 9>&-
    exit 1
  fi
  exit 0
fi
temp=$(mktemp -d "$root/.install.XXXXXXXX")
trap 'rm -rf -- "$temp"' EXIT
if [[ "$recovery_mode" == --recover-boot ]]; then
  if [[ -f "$txn/boot-id" && "$(cat "$txn/boot-id")" != "$(cat /proc/sys/kernel/random/boot_id)" ]]; then
    # A start job can already be ordered behind this recovery unit. Cancel
    # it if the candidate installed a unit or enabled a previously disabled one.
    if [[ ! -f "$txn/unit" || -f "$txn/disable" ]]; then
      systemctl --no-block stop ephemera.service 9>&-
    fi
    restore_files || die 'boot recovery could not restore the previous installation'
    systemctl daemon-reload
    if [[ -f "$txn/active" ]]; then
      systemctl --no-block start ephemera.service 9>&-
    fi
    finish_transaction
  fi
  exit 0
fi
rollback || die 'rollback failed; transaction and releases retained for repair'
if [[ "$recovery_mode" == --recover ]]; then exit 0; fi

finish_install() {
  local result=$?
  trap - EXIT
  trap '' INT TERM
  if (( result != 0 )); then
    rollback || printf 'install: rollback failed; transaction and releases retained for repair\n' >&2
  fi
  rm -rf -- "$temp"
  exit "$result"
}
trap finish_install EXIT
config_child=
interrupt_install() {
  if [[ -n "$config_child" ]]; then
    kill -TERM "$config_child" 2>/dev/null || true
    wait "$config_child" 2>/dev/null || true
  fi
  exit "$1"
}
trap 'interrupt_install 130' INT
trap 'interrupt_install 143' TERM

run_config_step() {
  local result=0
  "$@" <&0 9>&- &
  config_child=$!
  wait "$config_child" || result=$?
  config_child=
  return "$result"
}
begin_transaction() {
  mkdir "$temp/transaction"
  [[ ! -L "$current" ]] || readlink "$current" > "$temp/transaction/previous"
  [[ ! -f "$unit" ]] || cp "$unit" "$temp/transaction/unit"
  if [[ "$1" == config ]]; then
    touch "$temp/transaction/restore-config"
    [[ ! -L "$active_config" ]] || readlink "$active_config" > "$temp/transaction/previous-config"
  fi
  cat /proc/sys/kernel/random/boot_id > "$temp/transaction/boot-id"
  local service_state
  service_state=$(systemctl show --property=ActiveState --value ephemera.service) || die 'could not determine current service state'
  case "$service_state" in
    active|activating|reloading|deactivating) touch "$temp/transaction/active" ;;
    inactive|failed) ;;
    *) die "cannot verify that the service is stopped ($service_state)" ;;
  esac
  mv "$temp/transaction" "$txn"
  sync -f "$root"
}

publish_config() {
  # Move a complete, durable pair into the snapshot store before publishing the link.
  local generation
  generation=$(mktemp -d "$activations/config.XXXXXXXX")
  chmod 0755 "$generation"
  install -m 0644 "$1/config.yaml" "$generation/config.yaml"
  install -m 0600 "$1/github-app.pem" "$generation/github-app.pem"
  sync -f "$generation"
  atomic_link "$generation" "$active_config"
  sync -f /etc/ephemera
}

if [[ "$recovery_mode" == --apply-config ]]; then
  [[ -x "$current/ephemera" && -f "$unit" ]] || die 'install Ephemera before applying its configuration'
  install -d -m 0755 "$activations"
  mkdir -m 0700 "$temp/config"
  prepare=()
  if "$apply_yes"; then prepare+=(--yes); fi
  run_config_step "$current/ephemera" config --apply-step prepare "$temp/config" "${prepare[@]}"
  [[ -f "$temp/config/digest" ]] || exit 0
  begin_transaction config
  if [[ -f "$txn/active" ]]; then
    run_config_step "$current/ephemera" config --apply-step stop "$temp/config"
  else
    "$current/ephemera" config --apply-step check-recovery "$temp/config" 9>&-
  fi
  touch "$txn/switching"
  sync -f "$root"
  publish_config "$temp/config"
  if ! systemctl is-enabled --quiet ephemera.service; then
    touch "$txn/disable"
    sync -f "$root"
    systemctl enable ephemera.service
  fi
  run_config_step systemctl start ephemera.service
  "$current/ephemera" config --apply-step verify "$temp/config" 9>&-
  finish_transaction
  printf 'Configuration applied.\n'
  exit 0
fi

first=true
resume=false
replace_residue=false
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
  installed_key=$credential
  if [[ -f "$active_config/config.yaml" ]]; then installed_key=$active_config/github-app.pem; fi
  [[ -f "$installed_key" && ! -L "$installed_key" && "$(stat -c '%u:%a' "$installed_key")" == 0:600 ]] || die 'invalid GitHub App private key permissions'
fi
if "$first" && "$non_interactive" && [[ -n "$key" && ( -e "$credential" || -L "$credential" ) ]]; then
  replace_residue=true
fi
if [[ "$mode" != apply ]]; then
  online_bundle=false
  if [[ -z "$bundle" ]]; then
    online_bundle=true
    [[ -n "$version" ]] || version=$(latest_version)
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'invalid release tag'
    printf 'Target: %s\n' "$version"
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
  if ! "$online_bundle"; then printf 'Target: %s\n' "$identity"; fi
  [[ "$identity" == "$("$temp/ephemerad" --version)" ]] || die 'binary versions or commits differ'
  [[ -z "$version" || "${identity%% *}" == "$version" ]] || die 'bundle version does not match release tag'
  if "$non_interactive" && { "$first" || "$resume"; }; then
    [[ -r "$config" && -r "$key" ]] || die 'first install requires readable config and key'
    setup_args=(--non-interactive --config "$config" --github-app-key "$key")
    if "$resume"; then setup_args+=(--resume); fi
    if "$replace_residue"; then setup_args+=(--recover-residual-key); fi
    "$temp/ephemera" setup "${setup_args[@]}"
  fi
  selected_config=$destination
  if [[ -f "$active_config/config.yaml" ]]; then selected_config=$active_config/config.yaml; fi
  [[ ! -f "$selected_config" ]] || "$temp/ephemera" config validate --config "$selected_config"
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
if [[ "$mode" == apply ]]; then printf 'Target: %s\n' "$("$staged/ephemera" version)"; fi
selected_config=$destination
if [[ -f "$active_config/config.yaml" ]]; then selected_config=$active_config/config.yaml; fi
[[ ! -f "$selected_config" ]] || "$staged/ephemera" config validate --config "$selected_config"
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
# Install the independent recovery entry before recording or changing the service.
recovery_source=$0
if [[ ! -f "$recovery_source" ]]; then recovery_source=$staged/install.sh; fi
install -m 0755 "$recovery_source" "$root/recover-install.new"
mv -Tf "$root/recover-install.new" "$root/recover-install"
cat > "$temp/recovery-boot-unit" <<'UNIT'
[Unit]
Description=Restore Ephemera installation before daemon startup
ConditionPathExists=/opt/ephemera/transaction
Before=ephemera.service
RequiresMountsFor=/opt/ephemera
[Service]
Type=oneshot
ExecStart=/opt/ephemera/recover-install --recover-boot
[Install]
WantedBy=multi-user.target
UNIT
install -m 0644 "$temp/recovery-boot-unit" /etc/systemd/system/ephemera-recover-boot.service
install -d -m 0755 /etc/systemd/system/multi-user.target.wants
ln -sfn ../ephemera-recover-boot.service /etc/systemd/system/multi-user.target.wants/ephemera-recover-boot.service
cat > "$temp/recovery-unit" <<'UNIT'
[Unit]
Description=Recover an interrupted Ephemera installation
[Service]
Type=oneshot
ExecStart=/opt/ephemera/recover-install --recover
UNIT
install -m 0644 "$temp/recovery-unit" /etc/systemd/system/ephemera-recover.service
sync -f "$root"
sync -f /etc/systemd/system
transaction_type=release
if [[ ! -e "$active_config" && -f "$destination" ]]; then transaction_type=config; fi
begin_transaction "$transaction_type"
if [[ -f "$txn/active" ]]; then
  "$staged/ephemera" service stop 9>&- || die "upgrade paused; staged version retained at $staged"
fi
touch "$txn/switching"
sync -f "$root"
if [[ "$transaction_type" == config ]]; then
  install -d -m 0755 "$activations"
  mkdir -m 0700 "$temp/config"
  install -m 0644 "$destination" "$temp/config/config.yaml"
  install -m 0600 "$credential" "$temp/config/github-app.pem"
  "$staged/ephemera" config validate --config "$temp/config/config.yaml"
  publish_config "$temp/config"
fi
atomic_link "$(readlink "$staged")" "$current"
for binary in ephemera ephemerad; do atomic_link "$current/$binary" "/usr/local/bin/$binary"; done
if [[ -e /usr/local/bin/ephe || -L /usr/local/bin/ephe ]]; then
  [[ -L /usr/local/bin/ephe && "$(readlink /usr/local/bin/ephe)" == ephemera ]] || { die 'ephe alias already exists'; }
else ln -s ephemera /usr/local/bin/ephe; fi
install -m 0644 "$current/ephemera.service" "$unit.new"
mv -Tf "$unit.new" "$unit"
sync -f "$(dirname "$unit")"
if ! systemctl daemon-reload; then die 'daemon-reload failed'; fi
if "$first" && ! "$non_interactive" && [[ "$mode" == install ]]; then
  finish_transaction; rm -f "$staged"
  printf 'Ephemera installed.\n'
  if ( : <> /dev/tty ) 2>/dev/null; then
    flock -u 9
    /usr/local/bin/ephe setup </dev/tty >/dev/tty 2>&1
  else
    printf 'Run sudo ephe setup from a terminal to configure and start it.\n'
  fi
  exit 0
fi
if "$non_interactive"; then
  if ! systemctl is-enabled --quiet ephemera.service; then touch "$txn/disable"; sync -f "$root"; fi
  systemctl enable ephemera.service || { die 'failed to enable service'; }
fi
if ! systemctl start ephemera.service 9>&- || ! systemctl is-active --quiet ephemera.service; then
  die 'service failed to become ready; previous installation restored'
fi
finish_transaction; rm -f "$staged"
printf 'Ephemera is active: %s\n' "$("$current/ephemera" version)"
