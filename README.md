# Virtualis Agent

Virtualis 被控节点 – 纯 Go 后端，无前端。通过主控生成的一键指令接入。

```bash
sudo ./virtualis-agent --master http://MASTER_IP:8080 --token <token> --name node-01
```

支持 LXC / Incus / QEMU / Mock，与主控复用同一套驱动抽象。
