package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHelperChecksSameOriginDigestBeforeExecuting(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash unavailable; installer shell integration requires Bash")
	}
	for _, state := range []string{"valid", "mismatch", "missing", "duplicate"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "executed")
			tokenFile := filepath.Join(dir, "token")
			if err := os.WriteFile(tokenFile, []byte("fixture-token"), 0600); err != nil {
				t.Fatal(err)
			}
			script := []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$HELPER_EXECUTED\"\n")
			hash := sha256.Sum256(script)
			digest := hex.EncodeToString(hash[:]) + "  install.sh\n"
			if state == "mismatch" {
				digest = strings.Repeat("0", 64) + "  install.sh\n"
			}
			if state == "duplicate" {
				digest += digest
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/agent/install.sh" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Query().Get("checksum") == "1" {
					if state == "missing" {
						http.NotFound(w, r)
						return
					}
					w.Write([]byte(digest))
					return
				}
				w.Write(script)
			}))
			defer server.Close()
			cmd := exec.Command(bash, "../../install.sh", "--master", server.URL, "--token-file", filepath.ToSlash(tokenFile), "--name", "fixture", "--mode", "1", "--allow-insecure")
			cmd.Env = append(os.Environ(), "HELPER_EXECUTED="+filepath.ToSlash(marker), "TMPDIR="+filepath.ToSlash(t.TempDir()))
			out, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(marker)
			if state == "valid" {
				if err != nil || readErr != nil {
					t.Fatalf("valid checksum rejected: %v %v %s", err, readErr, out)
				}
				if !strings.Contains(string(data), "--token-file") || strings.Contains(string(data), "fixture-token") {
					t.Fatalf("token was passed on CLI: %s", data)
				}
			} else if err == nil || !os.IsNotExist(readErr) {
				t.Fatalf("unverified installer executed: state=%s err=%v marker=%s output=%s", state, err, data, out)
			}
		})
	}
}
