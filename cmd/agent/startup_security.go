package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
)

func validateMasterURL(master string, allowInsecure bool) error {
	u, err := url.Parse(master)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("master must be an http(s) URL without credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := (ip != nil && ip.IsLoopback()) || strings.EqualFold(u.Hostname(), "localhost")
	if u.Scheme == "http" && !loopback && !allowInsecure {
		return fmt.Errorf("non-loopback plaintext master refused; use HTTPS or explicitly accept credential exposure with --allow-insecure")
	}
	return nil
}

func loadAgentToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("token file unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", fmt.Errorf("token file must be a small regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("token file permissions must be 0600 or stricter")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot open token file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", fmt.Errorf("cannot read token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || len(raw) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return "", fmt.Errorf("token file contains an invalid token")
	}
	return token, nil
}
