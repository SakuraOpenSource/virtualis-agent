# Virtualis Agent

Virtualis 被控节点是纯 Go 后端，无前端。它负责在本机探测驱动、创建实例、采集资源/网络状态，并提供受 token 保护的 RPC 给主控。

支持 LXC / Incus / QEMU / Mock。QEMU 实例会启用 VNC，并由主控代理为浏览器 noVNC 使用。

如果被控位于 NAT 或多网卡环境，请显式指定主控可访问的地址：

```bash
sudo ./virtualis-agent --master http://MASTER_IP:8080 --token <token> --name node-01 --advertise http://AGENT_IP:8081
```

## 一键安装

安装包由主控的 `agent-packages` 提供。也可以执行主控生成的安装命令：

```bash
curl -fsSL http://MASTER:8080/api/agent/install.sh | bash -s -- --master http://MASTER:8080 --token TOKEN --name node-01 --mode 1
```

安装模式：`1` 仅安装 Agent，`2` Incus，`3` LXC，`4` QEMU，`5` Mock。
