#!/usr/bin/env bash
set -Eeuo pipefail

# Agent installation is owned by the master. This local helper only collects
# parameters and executes the exact installer served by that master.
MASTER=""
TOKEN=""
NAME=""
MODE=""
ADVERTISE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --master) MASTER="${2:-}"; shift 2;;
    --token) TOKEN="${2:-}"; shift 2;;
    --name) NAME="${2:-}"; shift 2;;
    --mode) MODE="${2:-}"; shift 2;;
    --advertise) ADVERTISE="${2:-}"; shift 2;;
    *) shift;;
  esac
done

if [[ -z "$MASTER" ]]; then
  read -r -p "主控地址 (如 http://MASTER:8080): " MASTER < /dev/tty
fi
if [[ -z "$TOKEN" ]]; then
  read -r -p "接入 Token: " TOKEN < /dev/tty
fi
if [[ -z "$NAME" ]]; then
  read -r -p "被控名称 [$(hostname)]: " NAME < /dev/tty || true
  NAME="${NAME:-$(hostname)}"
fi
if [[ -z "$MODE" ]]; then
  read -r -p "安装模式 [1=仅 Agent, 2=Incus, 3=LXC, 4=QEMU, 5=Mock] [1]: " MODE < /dev/tty || true
  MODE="${MODE:-1}"
fi

ARGS=(--master "$MASTER" --token "$TOKEN" --name "$NAME" --mode "$MODE")
if [[ -n "$ADVERTISE" ]]; then ARGS+=(--advertise "$ADVERTISE"); fi
if command -v curl >/dev/null 2>&1; then
  curl --fail --silent --show-error --location "$MASTER/api/agent/install.sh" | bash -s -- "${ARGS[@]}"
elif command -v wget >/dev/null 2>&1; then
  wget -qO- "$MASTER/api/agent/install.sh" | bash -s -- "${ARGS[@]}"
else
  echo "需要 curl 或 wget"
  exit 1
fi
