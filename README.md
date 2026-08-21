# Virtualis Agent

Virtualis 被控节点 – 纯 Go 后端，无前端。通过主控生成的一键指令接入。

```bash
sudo ./virtualis-agent --master http://MASTER_IP:8080 --token <token> --name node-01
```

支持 LXC / Incus / QEMU / Mock，与主控复用同一套驱动抽象。

## 一键安装（内置可选后端）

脚本已整合至 Agent 内：`virtualis-agent/install.sh`，可选择：

1. 仅安装 Agent
2. 安装 Incus+Agent
3. 安装 LXC+Agent
4. 安装 QEMU+Agent
5. 安装 Mock+Agent

```bash
sudo bash virtualis-agent/install.sh --master http://MASTER:8080 --token <token> --name node-01 --mode 2
# 或交互式
sudo bash virtualis-agent/install.sh
```
