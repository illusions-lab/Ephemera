#!/bin/bash
set -euo pipefail

power_read() { /usr/bin/pmset -g custom; }
power_capabilities() { /usr/bin/pmset -g cap; }
power_write() { /usr/bin/sudo /usr/bin/pmset -c "$@"; }

ac_value() {
  power_read | awk -v key="$1" '
    /^AC Power:/ { ac=1; next }
    /^[^[:space:]]/ { ac=0 }
    ac && $1 == key && $2 ~ /^[0-9]+$/ { print $2; exit }
  '
}

desired_power() {
  printf 'sleep 0\n'
  if power_capabilities | grep -Eq '^[[:space:]]*autorestart([[:space:]]|$)' &&
     [[ -n "$(ac_value autorestart)" ]]; then
    printf 'autorestart 1\n'
  fi
}

set_power() {
  power_write "$1" "$2" && [[ "$(ac_value "$1")" == "$2" ]]
}

server_settings() (
  local mode="${1:-}" root="$HOME/Library/Application Support/Ephemera"
  local key wanted old backup tmp applied failure
  case "$mode" in
    --preview|--apply|--restore) ;;
    *) printf 'usage: server-config.sh --preview|--apply|--restore\n' >&2; return 2 ;;
  esac
  if [[ "$mode" == --preview ]]; then
    while read -r key wanted; do printf 'AC %s: %s → %s\n' "$key" "$(ac_value "$key")" "$wanted"; done < <(desired_power)
    return
  fi
  umask 077
  backup="$root/power-backup"
  if [[ "$mode" == --restore ]]; then
    failure=0
    if [[ -f "$backup" ]]; then
      while read -r key old wanted; do
        case "$key:$wanted" in sleep:0|autorestart:1) ;; *) return 1 ;; esac
        [[ "$old" =~ ^[0-9]+$ ]] || return 1
        if [[ "$(ac_value "$key")" == "$wanted" ]]; then
          set_power "$key" "$old" || failure=1
        else
          printf 'Keeping current AC %s. It differs from the applied value.\n' "$key"
        fi
      done < "$backup"
    fi
    [[ "$failure" == 0 ]] || return 1
    if [[ -f "$root/server-profile" ]]; then
      tmp=$(mktemp "$root/.power-profile.XXXXXX")
      trap 'rm -f "$tmp"' EXIT
      printf 'personal\n' > "$tmp"
      mv "$tmp" "$root/server-profile"
    fi
    rm -f "$backup"
    return
  fi

  if [[ -f "$backup" ]]; then
    printf 'Server power settings already recorded. Restore them before applying a new profile.\n'
    return
  fi
  tmp=$(mktemp "$root/.power.XXXXXX")
  applied=$(mktemp "$root/.power-applied.XXXXXX")
  trap 'rm -f "$tmp" "$applied"' EXIT
  while read -r key wanted; do
    old=$(ac_value "$key")
    [[ "$old" =~ ^[0-9]+$ ]] || { printf 'Unable to read AC %s\n' "$key" >&2; return 1; }
    [[ "$old" == "$wanted" ]] || printf '%s %s %s\n' "$key" "$old" "$wanted" >> "$tmp"
  done < <(desired_power)
  mv "$tmp" "$backup"
  failure=0
  while read -r key old wanted; do
    printf '%s %s\n' "$key" "$old" >> "$applied"
    if ! set_power "$key" "$wanted"; then
      failure=1
      while read -r key old; do
        if [[ "$(ac_value "$key")" != "$old" ]]; then
          set_power "$key" "$old" || failure=2
        fi
      done < "$applied"
      break
    fi
  done < "$backup"
  if [[ "$failure" == 1 ]]; then rm -f "$backup"; fi
  [[ "$failure" == 0 ]]
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  if [[ "${1:-}" == --preview ]]; then
    server_settings "$@"
  else
    # Install the original-value record under a lock before writing a setting.
    # shellcheck disable=SC2016

    /usr/bin/lockf -k -t 0 "$HOME/Library/Application Support/Ephemera/.power.lock" \
      /bin/bash -c 'source "$1"; shift; server_settings "$@"' bash "$0" "$@"
  fi
fi
