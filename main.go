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
	ClientSecret string   `json:"client_secret,omitempty"`
	LoginToken   string   `json:"login_token,omitempty"` // 临时 token，用于调用官网 API
	UserInfo     map[string]interface{} `json:"user_info,omitempty"` // 用户信息（头像、用户名、邮箱、角色等）
	Tunnels      []Tunnel `json:"tunnels,omitempty"`
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
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Addr        string `json:"addr"`
	Port        int    `json:"port"`
	Location    string `json:"location,omitempty"`
	Group       string `json:"group,omitempty"`
	Visibility  int    `json:"visibility,omitempty"`
	Subdomain   string `json:"subdomain,omitempty"`
	AuthMethod  string `json:"auth_method,omitempty"`
}

type Tunnel struct {
	Name         string `json:"name"`
	Type         string `json:"type"` // tcp | udp | http | https | stcp | sudp | xtcp
	LocalIP      string `json:"localIp"`
	LocalPort    int    `json:"localPort"`
	RemotePort   *int   `json:"remotePort,omitempty"`
	Subdomain    string `json:"subdomain,omitempty"`
	CustomDomain string `json:"customDomain,omitempty"`
	SecretKey    string `json:"secretKey,omitempty"`
}

var (
	frpcCmd *exec.Cmd
	frpcMu  sync.Mutex
)

func main() {
	os.MkdirAll(configDir, 0755)
	os.MkdirAll(filepath.Dir(logFile), 0755)

	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/user", handleUser)
	http.HandleFunc("/api/plans", handlePlans)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/secret", handleSecret)
	http.HandleFunc("/api/tunnels", handleTunnels)
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

	var loginResponse struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		Data  struct {
			Token     string                 `json:"token"`
			ExpiresAt string                 `json:"expires_at"`
			User      map[string]interface{} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(loginResp, &loginResponse); err != nil {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "parse login response failed"})
		return
	}

	if !loginResponse.OK || loginResponse.Data.Token == "" {
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: loginResponse.Error})
		return
	}

	// 步骤2: 使用 token 获取服务器列表
	serversResp, err := callServersAPI(req.Site, loginResponse.Data.Token)
	if err != nil {
		log.Printf("Failed to call servers API: %v", err)
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "get servers failed: " + err.Error()})
		return
	}
	log.Printf("Servers API response: %s", string(serversResp))

	// 官网 API 返回格式: {"ok":true,"data":{"servers":[...],"group_level":2}}
	var serversResponse struct {
		OK   bool `json:"ok"`
		Data struct {
			Servers    []Server `json:"servers"`
			GroupLevel int      `json:"group_level"`
		} `json:"data"`
	}
	
	if err := json.Unmarshal(serversResp, &serversResponse); err != nil {
		log.Printf("Failed to parse servers response: %v, raw: %s", err, string(serversResp))
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "parse servers response failed"})
		return
	}
	
	if !serversResponse.OK {
		log.Printf("Servers API returned ok=false")
		json.NewEncoder(w).Encode(LoginResponse{OK: false, Error: "servers API returned error"})
		return
	}
	
	log.Printf("Parsed %d servers from API (group_level=%d)", len(serversResponse.Data.Servers), serversResponse.Data.GroupLevel)
	for i, srv := range serversResponse.Data.Servers {
		log.Printf("Server[%d]: ID=%d, Name=%s, Addr=%s, Port=%d", i, srv.ID, srv.Name, srv.Addr, srv.Port)
	}
	
	serversResult := struct {
		Servers []Server
	}{
		Servers: serversResponse.Data.Servers,
	}

	// 步骤3: 获取公共配置（OIDC 参数）
	configResp, err := callConfigAPI(req.Site)
	var oidcConfig OIDCConfig
	if err == nil {
		var configResult struct {
			OK   bool       `json:"ok"`
			Data OIDCConfig `json:"data"`
		}
		if err := json.Unmarshal(configResp, &configResult); err == nil && configResult.OK {
			oidcConfig = configResult.Data
		}
	}

	// 步骤4: 获取用户信息（包含 Client Secret 和会员信息）
	userResp, err := callUserAPI(req.Site, loginResponse.Data.Token)
	var clientSecret string
	var userInfo map[string]interface{}
	if err == nil {
		var userResult struct {
			OK   bool                   `json:"ok"`
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(userResp, &userResult); err == nil && userResult.OK {
			// 提取 client_secret
			if cs, ok := userResult.Data["client_secret"].(string); ok {
				clientSecret = cs
				if len(clientSecret) > 10 {
					log.Printf("Got client secret from API: %s...", clientSecret[:10])
				} else {
					log.Printf("Got client secret from API: %s", clientSecret)
				}
			}
			// 提取 user 对象中的字段
			if userObj, ok := userResult.Data["user"].(map[string]interface{}); ok {
				userInfo = userObj
			} else {
				userInfo = userResult.Data
			}
			// 保存会员信息到 userInfo
			if groups, ok := userResult.Data["groups"]; ok {
				userInfo["groups"] = groups
			}
			if topGroup, ok := userResult.Data["top_group"]; ok {
				userInfo["top_group"] = topGroup
			}
			if expiries, ok := userResult.Data["expiries"]; ok {
				userInfo["expiries"] = expiries
			}
			if memberLabel, ok := userResult.Data["member_label"]; ok {
				userInfo["member_label"] = memberLabel
			}
		}
	}

	// 构建响应
	result := LoginResponse{
		OK:      true,
		Servers: serversResult.Servers,
		Config:  oidcConfig,
		Secret:  clientSecret,
	}
	log.Printf("Building login response: OK=%v, Servers count=%d, Secret=%v", result.OK, len(result.Servers), clientSecret != "")

	// 保存配置（包含 OIDC 参数和 Client Secret）
	cfg := loadConfig()
	cfg.Enabled = true
	cfg.User = req.Identity
	cfg.AuthMethod = "oidc"
	cfg.AuthOIDCIssuer = req.Site
	cfg.LoginToken = loginResponse.Data.Token // 保存登录 token 用于后续 API 调用
	cfg.ClientSecret = clientSecret // 保存从官网获取的 Client Secret
	if userInfo != nil {
		cfg.UserInfo = userInfo // 保存完整用户信息（包括会员信息）
	} else {
		cfg.UserInfo = loginResponse.Data.User // 回退到登录响应中的用户信息
	}
	
	// 保存 OIDC 配置参数
	if oidcConfig.TokenEndpoint != "" {
		cfg.AuthOIDCTokenEndpoint = oidcConfig.TokenEndpoint
	} else {
		// 默认值
		cfg.AuthOIDCTokenEndpoint = req.Site + "/oidc/token.php"
	}
	if oidcConfig.Audience != "" {
		cfg.AuthOIDCAudience = oidcConfig.Audience
	}
	if oidcConfig.Scope != "" {
		cfg.AuthOIDCScope = oidcConfig.Scope
	}
	
	// 只在没有配置服务器时，才默认使用第一个服务器
	if cfg.ServerAddr == "" && len(result.Servers) > 0 {
		cfg.ServerAddr = result.Servers[0].Addr
		cfg.ServerPort = result.Servers[0].Port
	}
	saveConfig(cfg)

	log.Printf("Sending login response to client")
	json.NewEncoder(w).Encode(result)
}

func handleUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	cfg := loadConfig()
	if cfg.UserInfo == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "not logged in"})
		return
	}
	// 添加 OIDC 认证状态和服务器密钥状态
	userInfo := make(map[string]interface{})
	for k, v := range cfg.UserInfo {
		userInfo[k] = v
	}
	userInfo["auth_enabled"] = cfg.AuthMethod == "oidc" && cfg.ClientSecret != ""
	userInfo["secret_set"] = cfg.ClientSecret != ""
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": userInfo})
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

func callConfigAPI(site string) ([]byte, error) {
	apiURL := site + "/api/config.php"
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %v", err)
	}
	
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

func callUserAPI(site, token string) ([]byte, error) {
	apiURL := site + "/api/me.php"
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

func callPlansAPI(site, token string) ([]byte, error) {
	apiURL := site + "/api/plans.php"
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

func handlePlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	cfg := loadConfig()
	if cfg.LoginToken == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "not logged in"})
		return
	}
	plansResp, err := callPlansAPI(cfg.AuthOIDCIssuer, cfg.LoginToken)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	var plansResult struct {
		OK   bool                   `json:"ok"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(plansResp, &plansResult); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "parse failed"})
		return
	}
	if !plansResult.OK {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "plans API returned error"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": plansResult.Data})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := loadConfig()
		json.NewEncoder(w).Encode(cfg)
	case "POST":
		var updates map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		
		// 加载现有配置
		cfg := loadConfig()
		
		// 只更新传入的字段
		if addr, ok := updates["server_addr"].(string); ok {
			cfg.ServerAddr = addr
		}
		if port, ok := updates["server_port"].(float64); ok {
			cfg.ServerPort = int(port)
		}
		if secret, ok := updates["client_secret"].(string); ok {
			cfg.ClientSecret = secret
		}
		if enabled, ok := updates["enabled"].(bool); ok {
			cfg.Enabled = enabled
		}
		
		saveConfig(cfg)
		
		// 如果更新了服务器地址，重新生成 frpc 配置
		if _, hasAddr := updates["server_addr"]; hasAddr {
			writeFrpcConfig(cfg)
			log.Printf("Regenerated frpc config after server change")
		}
		
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

func handleTunnels(w http.ResponseWriter, r *http.Request) {
	cfg := loadConfig()
	
	switch r.Method {
	case "GET":
		// 返回所有隧道
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"tunnels": cfg.Tunnels,
		})
		
	case "POST":
		// 添加或更新隧道
		var req Tunnel
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "invalid request"})
			return
		}
		
		// 验证必填字段
		if req.Name == "" || req.Type == "" || req.LocalIP == "" || req.LocalPort == 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "missing required fields"})
			return
		}
		
		// 查找是否已存在同名隧道
		found := false
		for i, t := range cfg.Tunnels {
			if t.Name == req.Name {
				cfg.Tunnels[i] = req
				found = true
				break
			}
		}
		
		// 如果不存在，添加到列表
		if !found {
			cfg.Tunnels = append(cfg.Tunnels, req)
		}
		
		saveConfig(cfg)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		
	case "DELETE":
		// 删除隧道
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "invalid request"})
			return
		}
		
		// 从列表中移除
		newTunnels := make([]Tunnel, 0, len(cfg.Tunnels))
		for _, t := range cfg.Tunnels {
			if t.Name != req.Name {
				newTunnels = append(newTunnels, t)
			}
		}
		cfg.Tunnels = newTunnels
		
		saveConfig(cfg)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
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

	frpcCmd = exec.Command(frpcBinary, "-c", "/data/config/frpc.toml")
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
	toml := fmt.Sprintf(`# 由 AFRP Web UI 自动生成，请勿手动修改（修改会在下次连接时覆盖）
serverAddr = "%s"
serverPort = %d
user = "%s"
loginFailExit = false

auth.method = "oidc"
auth.oidc.clientID = "%s"
auth.oidc.clientSecret = "%s"
auth.oidc.tokenEndpointURL = "%s"
auth.oidc.audience = "%s"
auth.oidc.scope = "%s"
`, cfg.ServerAddr, cfg.ServerPort, cfg.User, cfg.User, cfg.ClientSecret,
		cfg.AuthOIDCTokenEndpoint, cfg.AuthOIDCAudience, cfg.AuthOIDCScope)

	// 添加隧道配置
	for _, t := range cfg.Tunnels {
		toml += fmt.Sprintf(`
[[proxies]]
name = "%s"
type = "%s"
localIP = "%s"
localPort = %d
`, escapeToml(t.Name), escapeToml(t.Type), escapeToml(t.LocalIP), t.LocalPort)
		
		if t.Type == "http" || t.Type == "https" {
			if t.CustomDomain != "" {
				toml += fmt.Sprintf(`customDomains = ["%s"]
`, escapeToml(t.CustomDomain))
			} else if t.Subdomain != "" {
				toml += fmt.Sprintf(`subdomain = "%s"
`, escapeToml(t.Subdomain))
			}
		} else if t.Type == "stcp" || t.Type == "sudp" || t.Type == "xtcp" {
			if t.SecretKey != "" {
				toml += fmt.Sprintf(`secretKey = "%s"
`, escapeToml(t.SecretKey))
			}
		} else {
			if t.RemotePort != nil && *t.RemotePort > 0 {
				toml += fmt.Sprintf(`remotePort = %d
`, *t.RemotePort)
			}
		}
	}

	os.WriteFile("/data/config/frpc.toml", []byte(toml), 0644)
}

func escapeToml(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
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
