#!/usr/bin/env bash
set -e
# Virtualis Agent 一键安装（内置可选后端）
# 可选择：
# 1. 仅安装 Agent
# 2. 安装 Incus+Agent
# 3. 安装 LXC+Agent
# 4. 安装 QEMU+Agent
# 5. 安装 Mock+Agent
# 用法: sudo bash install.sh --master http://MASTER:8080 --token <token> [--name node-01] [--mode 1-5]
# 或交互式: sudo bash install.sh

MASTER=""; TOKEN=""; NAME=""; MODE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --master) MASTER="$2"; shift 2;;
    --token) TOKEN="$2"; shift 2;;
    --name) NAME="$2"; shift 2;;
    --mode) MODE="$2"; shift 2;;
    *) shift;;
  esac
done

if [[ -z "$MASTER" || -z "$TOKEN" ]]; then
  echo "=== Virtualis Agent 一键安装 ==="
  read -p "主控地址 (如 http://114.66.41.15:8080): " MASTER
  read -p "接入 Token: " TOKEN
  read -p "被控名称 [node-$(hostname)]: " NAME
  NAME=${NAME:-node-$(hostname)}
fi

if [[ -z "$MODE" ]]; then
  echo ""
  echo "可选择："
  echo "  1) 仅安装 Agent"
  echo "  2) 安装 Incus+Agent"
  echo "  3) 安装 LXC+Agent"
  echo "  4) 安装 QEMU+Agent"
  echo "  5) 安装 Mock+Agent"
  read -p "选择 [1]: " MODE
  MODE=${MODE:-1}
fi

echo "主控: $MASTER"
echo "名称: $NAME"
echo "模式: $MODE"

install_backend() {
  case "$1" in
    1) echo "仅安装 Agent，跳过后端安装" ;;
    2) echo "安装 Incus..."; 
       if command -v apt-get >/dev/null 2>&1; then apt-get update && apt-get install -y incus 2>/dev/null || true
       elif command -v dnf >/dev/null 2>&1; then dnf install -y incus 2>/dev/null || true
       elif command -v yum >/dev/null 2>&1; then yum install -y incus 2>/dev/null || true
       else echo "未识别包管理器，请手动安装 incus"; fi
       ;;
    3) echo "安装 LXC...";
       if command -v apt-get >/dev/null 2>&1; then apt-get update && apt-get install -y lxc lxc-templates 2>/dev/null || true
       elif command -v dnf >/dev/null 2>&1; then dnf install -y lxc lxc-templates 2>/dev/null || true
       elif command -v yum >/dev/null 2>&1; then yum install -y lxc lxc-templates 2>/dev/null || true
       else echo "未识别包管理器，请手动安装 lxc"; fi
       ;;
    4) echo "安装 QEMU...";
       if command -v apt-get >/dev/null 2>&1; then apt-get update && apt-get install -y qemu-kvm qemu-utils libvirt-clients 2>/dev/null || true
       elif command -v dnf >/dev/null 2>&1; then dnf install -y qemu-kvm qemu-img 2>/dev/null || true
       elif command -v yum >/dev/null 2>&1; then yum install -y qemu-kvm qemu-img 2>/dev/null || true
       else echo "未识别包管理器，请手动安装 qemu-kvm"; fi
       ;;
    5) echo "安装 Mock+Agent (Mock 无需额外依赖)" ;;
    *) echo "未知模式 $1，按 1 仅安装 Agent 处理" ;;
  esac
}

install_backend "$MODE"

# 安装并启动 Agent（复用主控部署逻辑或直接下载）
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VIRT_DEPLOY=""
if [[ -f "$SCRIPT_DIR/../virtualis/deploy/install-linux.sh" ]]; then
  VIRT_DEPLOY="$SCRIPT_DIR/../virtualis/deploy/install-linux.sh"
elif [[ -f "/tmp/virtualis/deploy/install-linux.sh" ]]; then
  VIRT_DEPLOY="/tmp/virtualis/deploy/install-linux.sh"
fi

if [[ -n "$VIRT_DEPLOY" && -f "$VIRT_DEPLOY" ]]; then
  echo "调用 $VIRT_DEPLOY --agent ..."
  bash "$VIRT_DEPLOY" --agent --master "$MASTER" --token "$TOKEN" --name "$NAME"
else
  echo "下载 virtualis-agent..."
  curl -fsSL "$MASTER/api/agent/install.sh" | bash -s -- --master "$MASTER" --token "$TOKEN" --name "$NAME"
fi

echo ""
echo "Agent 安装完成（模式 $MODE）"
