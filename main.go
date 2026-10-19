package main

import (
	"bytes"
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
	"time"
)

const (
	configDir  = "/data/config"
	configFile = "/data/config/config.json"
	frpcBinary = "/usr/local/bin/frpc"
	logFile    = "/data/logs/frpc.log"
	defaultSite = "https://www.afrp.net"
)

type Config struct {
	Enabled     bool   `json:"enabled"`
	ServerAddr  string `json:"server_addr"`
	ServerPort  int    `json:"server_port"`
	User        string `json:"user"`
	AuthMethod  string `json:"auth_method"`
	AuthOIDCIssuer      string `json:"auth_oidc_issuer"`
	AuthOIDCAudience    string `json:"auth_oidc_audience"`
	AuthOIDCScope       string `json:"auth_oidc_scope"`
	AuthOIDCTokenEndpoint string `json:"auth_oidc_token_endpoint"`
}

type LoginRequest struct {
	Site     string `json:"site"`
	Identity string `json:"identity"`
	Password string `json:"password"`
}

type LoginResponse struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Config   OIDCConfig `json:"config,omitempty"`
	Servers  []Server `json:"servers,omitempty"`
	Secret   string   `json:"secret,omitempty"`
}

type OIDCConfig struct {
	TokenEndpoint string `json:"token_endpoint"`
	Audience      string `json:"oidc_audience"`
	Scope         string `json:"oidc_scope"`
}

type Server struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Port     int    `json:"port"`
	Location string `json:"location,omitempty"`
}

var (
	frpcCmd *exec.Cmd
	frpcMu  sync.Mutex
)

func main() {
	os.MkdirAll(configDir, 0755)
	os.MkdirAll(filepath.Dir(logFile), 0755)

	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/start", handleStart)
	http.HandleFunc("/api/stop", handleStop)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/log", handleLog)
	http.Handle("/", http.FileServer(http.Dir("/web")))

	log.Println("AFRP Web UI starting on :8000")
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "invalid request"})
		return
	}

	if req.Site == "" {
		req.Site = defaultSite
	}

	// 调用官网 API 获取配置和服务器列表
	resp, err := callImportAPI(req.Site, req.Identity, req.Password, false)
	if err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: err.Error()})
		return
	}

	var result LoginResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "parse response failed"})
		return
	}

	if !result.OK {
		json.NewEncoder(w).Encode(result)
		return
	}

	// 保存配置
	cfg := loadConfig()
	cfg.Enabled = true
	cfg.User = req.Identity
	cfg.AuthMethod = "oidc"
	cfg.AuthOIDCIssuer = req.Site
	if result.Config.TokenEndpoint != "" {
		cfg.AuthOIDCTokenEndpoint = result.Config.TokenEndpoint
	}
	if result.Config.Audience != "" {
		cfg.AuthOIDCAudience = result.Config.Audience
	}
	if result.Config.Scope != "" {
		cfg.AuthOIDCScope = result.Config.Scope
	}
	if len(result.Servers) > 0 {
		cfg.ServerAddr = result.Servers[0].Addr
		cfg.ServerPort = result.Servers[0].Port
	}
	saveConfig(cfg)

	json.NewEncoder(w).Encode(result)
}

func callImportAPI(site, identity, password string, rotate bool) ([]byte, error) {
	// 构建登录请求
	loginReq := map[string]interface{}{
		"identity": identity,
		"password": password,
		"device":   "AFRP Web UI",
	}
	
	loginData, _ := json.Marshal(loginReq)
	
	// 发送 HTTP 请求到官网 API
	apiURL := site + "/api/v1/auth/login"
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(loginData))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %v", err)
	}
	
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}
	
	return body, nil
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
