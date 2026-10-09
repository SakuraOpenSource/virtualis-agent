#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# The master owns installation; this helper verifies its script before executing it.
MASTER=''
TOKEN_FILE=''
NAME=''
MODE=''
ADVERTISE=''
ALLOW_INSECURE=0
INSTALLER_SHA256="${VIRTUALIS_INSTALL_SHA256:-}"
FORWARDED=()
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --master|--master-url) MASTER="${2:?master URL required}"; shift 2;;
    --token-file) TOKEN_FILE="${2:?token file required}"; shift 2;;
    --token) fail 'Use --token-file; command-line secrets are not accepted';;
    --name) NAME="${2:?name required}"; shift 2;;
    --mode) MODE="${2:?mode required}"; shift 2;;
    --advertise) ADVERTISE="${2:?advertise URL required}"; shift 2;;
    --allow-insecure) ALLOW_INSECURE=1; shift;;
    --installer-sha256) INSTALLER_SHA256="${2:?installer SHA-256 required}"; shift 2;;
    --expected-sha256|--version|--gh-proxy) FORWARDED+=("$1" "${2:?value required}"); shift 2;;
    --no-start|--update) FORWARDED+=("$1"); shift;;
    -h|--help) printf '%s\n' 'Usage: install.sh --master https://MASTER --token-file /secure/token [--mode 1|2|4] [--allow-insecure] [--installer-sha256 independent-digest]'; exit 0;;
    *) fail "Unknown option: $1";;
  esac
done
if [[ -z "$MASTER" ]]; then read -r -p 'Master URL (https://MASTER): ' MASTER < /dev/tty; fi
MASTER="${MASTER%/}"
[[ "$MASTER" =~ ^https?://[^/@?\#[:space:]]+(:[0-9]+)?(/[^?\#[:space:]]*)?$ ]] || fail 'Invalid master URL'
HOST="${MASTER#*://}"; HOST="${HOST%%/*}"
if [[ "$MASTER" == http://* && "$ALLOW_INSECURE" != 1 ]]; then
  case "$HOST" in
    localhost|localhost:*|127.*|'[::1]'|'[::1]':*) ;;
    *) fail 'Non-loopback HTTP requires explicit --allow-insecure; prefer HTTPS';;
  esac
fi
if [[ -z "$NAME" ]]; then NAME="$(hostname -s)"; fi
if [[ -z "$MODE" ]]; then MODE=1; fi
case "$MODE" in
  1|2|4) ;;
  3) fail 'Mode 3 is an unsupported LXD-compatible client, not traditional LXC';;
  *) fail 'Use 1=Agent, 2=Incus or 4=QEMU';;
esac
command -v curl >/dev/null || fail 'curl is required'
command -v sha256sum >/dev/null || fail 'sha256sum is required'
WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/virtualis-helper.XXXXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
if [[ -z "$TOKEN_FILE" ]]; then
  read -r -s -p 'Join token: ' TOKEN < /dev/tty
  printf '\n' >&2
  TOKEN_FILE="$WORK/token"
  printf '%s\n' "$TOKEN" > "$TOKEN_FILE"
  unset TOKEN
fi
[[ -f "$TOKEN_FILE" && ! -L "$TOKEN_FILE" && -s "$TOKEN_FILE" ]] || fail 'Token file must be a nonempty regular file'
SCRIPT="$WORK/install.sh"
MANIFEST="$WORK/checksums"
# Do not follow redirects: both downloads must remain bound to the selected origin.
# Native Windows curl cannot consume MSYS paths when automatic path conversion is disabled.
output_path() {
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) cygpath -m "$1";;
    *) printf '%s\n' "$1";;
  esac
}
curl --fail --silent --show-error --proto '=http,https' --output "$(output_path "$SCRIPT")" "$MASTER/api/agent/install.sh" || fail 'Installer download failed'
curl --fail --silent --show-error --proto '=http,https' --output "$(output_path "$MANIFEST")" "$MASTER/api/agent/install.sh?checksum=1" || fail 'Same-origin installer checksum unavailable'
DIGEST=''; COUNT=0
while read -r HASH ASSET EXTRA || [[ -n "$HASH" ]]; do
  [[ "$HASH" =~ ^[0-9a-fA-F]{64}$ && "${ASSET#\*}" == install.sh && -z "$EXTRA" ]] || fail 'Malformed installer checksum'
  DIGEST="${HASH,,}"; COUNT=$((COUNT + 1))
done < "$MANIFEST"
[[ "$COUNT" == 1 ]] || fail 'Installer checksum must contain exactly one entry'
if [[ -n "$INSTALLER_SHA256" ]]; then
  [[ "$INSTALLER_SHA256" =~ ^[0-9a-fA-F]{64}$ && "${INSTALLER_SHA256,,}" == "$DIGEST" ]] || fail 'Independent installer digest does not match the master'
fi
printf '%s  %s\n' "$DIGEST" "$SCRIPT" | sha256sum --check --status - || fail 'Installer SHA-256 mismatch; refusing execution'
printf '%s\n' 'NOTICE: Same-origin checksums verify integrity, not an independent publisher signature. Use --installer-sha256 from a trusted channel for authenticity.' >&2
ARGS=(--master-url "$MASTER" --token-file "$TOKEN_FILE" --name "$NAME" --mode "$MODE")
[[ -z "$ADVERTISE" ]] || ARGS+=(--advertise "$ADVERTISE")
[[ "$ALLOW_INSECURE" != 1 ]] || ARGS+=(--allow-insecure)
bash "$SCRIPT" "${ARGS[@]}" "${FORWARDED[@]}"
