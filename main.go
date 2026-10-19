package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	configDir  = "/data/config"
	configFile = "/data/config/config.json"
	frpcBinary = "/usr/local/bin/frpc"
	logFile    = "/data/logs/frpc.log"
)

type Config struct {
	Enabled     bool   `json:"enabled"`
	ServerAddr  string `json:"server_addr"`
	ServerPort  int    `json:"server_port"`
	User        string `json:"user"`
	AuthMethod  string `json:"auth_method"`
	AuthOIDCIssuer   string `json:"auth_oidc_issuer"`
	AuthOIDCAudience string `json:"auth_oidc_audience"`
	AuthOIDCScope    string `json:"auth_oidc_scope"`
	AuthOIDCTokenEndpoint string `json:"auth_oidc_token_endpoint"`
}

var (
	frpcCmd *exec.Cmd
	frpcMu  sync.Mutex
)

func main() {
	os.MkdirAll(configDir, 0755)
	os.MkdirAll(filepath.Dir(logFile), 0755)

	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/start", handleStart)
	http.HandleFunc("/api/stop", handleStop)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/log", handleLog)
	http.Handle("/", http.FileServer(http.Dir("/web")))

	log.Println("AFRP Web UI starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := loadConfig()
		json.NewEncoder(w).Encode(cfg)
	case "POST":
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		saveConfig(cfg)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	frpcMu.Lock()
	defer frpcMu.Unlock()

	if frpcCmd != nil && frpcCmd.Process != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "already running"})
		return
	}

	cfg := loadConfig()
	if !cfg.Enabled {
		json.NewEncoder(w).Encode(map[string]string{"error": "not enabled"})
		return
	}

	writeFrpcConfig(cfg)

	logF, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	frpcCmd = exec.Command(frpcBinary, "-c", "/data/config/frpc.ini")
	frpcCmd.Stdout = logF
	frpcCmd.Stderr = logF

	if err := frpcCmd.Start(); err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	go func() {
		frpcCmd.Wait()
		frpcMu.Lock()
		frpcCmd = nil
		frpcMu.Unlock()
	}()

	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	frpcMu.Lock()
	defer frpcMu.Unlock()

	if frpcCmd != nil && frpcCmd.Process != nil {
		frpcCmd.Process.Kill()
		frpcCmd = nil
	}

	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	frpcMu.Lock()
	running := frpcCmd != nil && frpcCmd.Process != nil
	frpcMu.Unlock()

	json.NewEncoder(w).Encode(map[string]bool{"running": running})
}

func handleLog(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(logFile)
	if err != nil {
		http.Error(w, "no log", 404)
		return
	}
	defer f.Close()

	// 返回最后 100 行
	lines := readLastLines(f, 100)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, strings.Join(lines, "\n"))
}

func loadConfig() Config {
	var cfg Config
	data, err := os.ReadFile(configFile)
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configFile, data, 0644)
}

func writeFrpcConfig(cfg Config) {
	ini := fmt.Sprintf(`[common]
server_addr = %s
server_port = %d
user = %s
auth_method = %s
auth_oidc_issuer = %s
auth_oidc_audience = %s
auth_oidc_scope = %s
auth_oidc_token_endpoint = %s
`, cfg.ServerAddr, cfg.ServerPort, cfg.User, cfg.AuthMethod,
		cfg.AuthOIDCIssuer, cfg.AuthOIDCAudience, cfg.AuthOIDCScope, cfg.AuthOIDCTokenEndpoint)

	os.WriteFile("/data/config/frpc.ini", []byte(ini), 0644)
}

func readLastLines(f *os.File, n int) []string {
	stat, _ := f.Stat()
	size := stat.Size()

	buf := make([]byte, min(size, 8192))
	f.ReadAt(buf, max(0, size-8192))

	lines := strings.Split(string(buf), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
