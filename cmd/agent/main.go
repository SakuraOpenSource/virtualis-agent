package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	var (
		master  = flag.String("master", "", "主控地址，例如 http://MASTER_IP:8080")
		token   = flag.String("token", "", "主控生成的接入 token")
		name    = flag.String("name", "", "被控名称，如 node-01")
		listen  = flag.String("listen", ":8081", "被控自身监听地址")
		version = flag.String("version", "dev", "版本")
	)
	flag.Parse()

	if *master == "" || *token == "" {
		fmt.Println("用法: virtualis-agent --master http://MASTER:8080 --token <token> --name node-01 [--listen :8081]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "agent"
		}
		*name = h
	}

	fmt.Printf("Virtualis Agent %s\n", *version)
	fmt.Printf("主控: %s\n", *master)
	fmt.Printf("名称: %s\n", *name)
	fmt.Printf("监听: %s\n", *listen)

	// 注册到主控
	registerURL := fmt.Sprintf("%s/api/agent/register", *master)
	payload := map[string]string{
		"driver":  "mock",
		"version": *version,
	}
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", registerURL, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", *token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("注册失败: %v (请检查主控地址与 token)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var body bytes.Buffer
		body.ReadFrom(resp.Body)
		log.Fatalf("主控拒绝注册 (%d): %s", resp.StatusCode, body.String())
	}
	fmt.Println("✓ 已成功注册到主控")

	// 心跳循环
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			hb, _ := http.NewRequest("POST", registerURL, bytes.NewReader(raw))
			hb.Header.Set("Content-Type", "application/json")
			hb.Header.Set("X-Agent-Token", *token)
			resp, err := client.Do(hb)
			if err != nil {
				log.Printf("心跳失败: %v", err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				log.Printf("心跳成功")
			}
		}
	}()

	// 被控自身也提供与主控相同的 VM API（无前端），供主控转发调用
	// 这里启动一个极简 HTTP 服务，复用 master 的驱动逻辑
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"name":"` + *name + `"}`))
	})
	mux.HandleFunc("/api/drivers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[{"name":"mock","available":true},{"name":"qemu","available":false},{"name":"lxc","available":false},{"name":"incus","available":false}]}`))
	})
	fmt.Printf("被控 HTTP 服务已启动 %s (健康检查: http://localhost%s/api/health)\n", *listen, *listen)
	fmt.Println("保持运行中，按 Ctrl+C 退出")
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("监听失败: %v", err)
	}
}
