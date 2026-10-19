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
	ClientSecret string `json:"client_secret,omitempty"`
	LoginToken   string `json:"login_token,omitempty"` // 临时 token，用于调用官网 API
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
	http.HandleFunc("/api/secret", handleSecret)
	http.HandleFunc("/api/start", handleStart)
	http.HandleFunc("/api/stop", handleStop)
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/log", handleLog)
	
	// 使用相对路径适配 Windows/Linux
	webDir := "./web"
	if _, err := os.Stat(webDir); os.IsNotExist(err) {
		// 如果相对路径不存在，尝试 Docker 容器内的路径
		webDir = "/web"
	}
	
	http.Handle("/", http.FileServer(http.Dir(webDir)))

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

	// 步骤1: 调用官网 API 登录获取 token
	loginResp, err := callLoginAPI(req.Site, req.Identity, req.Password)
	if err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: err.Error()})
		return
	}

	var loginResult struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		Token string `json:"token,omitempty"`
	}
	if err := json.Unmarshal(loginResp, &loginResult); err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "parse login response failed"})
		return
	}

	if !loginResult.OK || loginResult.Token == "" {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: loginResult.Error})
		return
	}

	// 步骤2: 使用 token 获取服务器列表
	serversResp, err := callServersAPI(req.Site, loginResult.Token)
	if err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "get servers failed: " + err.Error()})
		return
	}

	var serversResult struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(serversResp, &serversResult); err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "parse servers response failed"})
		return
	}

	// 构建响应
	result := LoginResponse{
		OK:      true,
		Servers: serversResult.Servers,
	}

	// 保存配置（只保存 token，不保存为 client_secret）
	cfg := loadConfig()
	cfg.Enabled = true
	cfg.User = req.Identity
	cfg.AuthMethod = "oidc"
	cfg.AuthOIDCIssuer = req.Site
	cfg.LoginToken = loginResult.Token // 保存登录 token 用于后续 API 调用
	if len(result.Servers) > 0 {
		cfg.ServerAddr = result.Servers[0].Addr
		cfg.ServerPort = result.Servers[0].Port
	}
	saveConfig(cfg)

	json.NewEncoder(w).Encode(result)
}

func callLoginAPI(site, identity, password string) ([]byte, error) {
	loginReq := map[string]interface{}{
		"identity": identity,
		"password": password,
		"device":   "AFRP Web UI",
	}
	
	loginData, _ := json.Marshal(loginReq)
	
	apiURL := site + "/api/login.php"
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

func callServersAPI(site, token string) ([]byte, error) {
	apiURL := site + "/api/servers.php"
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	
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

func handleSecret(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := loadConfig()
		json.NewEncoder(w).Encode(map[string]string{"secret": cfg.ClientSecret})
	case "POST":
		var req struct {
			Secret string `json:"secret"`
			Action string `json:"action"` // "save" or "rotate"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		
		cfg := loadConfig()
		if req.Action == "rotate" {
			// 使用登录 token 调用官网 API 生成新密钥
			newSecret, err := callRotateSecretAPI(cfg.AuthOIDCIssuer, cfg.LoginToken)
			if err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			req.Secret = newSecret
		}
		
		cfg.ClientSecret = req.Secret
		saveConfig(cfg)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "secret": req.Secret})
	}
}

func callRotateSecretAPI(site, loginToken string) (string, error) {
	apiURL := site + "/api/secret.php"
	
	reqBody := map[string]interface{}{}
	bodyData, _ := json.Marshal(reqBody)
	
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(bodyData))
	if err != nil {
		return "", fmt.Errorf("create request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if loginToken != "" {
		req.Header.Set("Authorization", "Bearer "+loginToken)
	}
	
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response failed: %v", err)
	}
	
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}
	
	var result struct {
		OK   bool   `json:"ok"`
		Data struct {
			ClientSecret string `json:"client_secret"`
		} `json:"data"`
		Error string `json:"error,omitempty"`
	}
	
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response failed: %v", err)
	}
	
	if !result.OK {
		return "", fmt.Errorf("rotate failed: %s", result.Error)
	}
	
	return result.Data.ClientSecret, nil
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
auth_oidc_client_secret = %s
`, cfg.ServerAddr, cfg.ServerPort, cfg.User, cfg.AuthMethod,
		cfg.AuthOIDCIssuer, cfg.AuthOIDCAudience, cfg.AuthOIDCScope, cfg.AuthOIDCTokenEndpoint,
		cfg.ClientSecret)

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
