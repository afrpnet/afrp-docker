package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSite = "https://www.afrp.net"
)

// 运行时路径；做成变量方便测试时指向临时目录
var (
	configDir      = "/data/config"
	configFile     = "/data/config/config.json"
	frpcConfigFile = "/data/config/frpc.toml"
	logFile        = "/data/logs/frpc.log"
	frpcBinary     = "/usr/local/bin/frpc"
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
	AccountSecrets map[string]string `json:"account_secrets,omitempty"` // 账号 -> Client Secret，切换账号时按账号取用
	AccountServers map[string]int    `json:"account_servers,omitempty"` // 账号 -> 最后选择的服务器 ID
	UserInfo     map[string]interface{} `json:"user_info,omitempty"` // 用户信息（头像、用户名、邮箱、角色等）
	Tunnels      []Tunnel `json:"tunnels,omitempty"`

	// frpc 自带的管理面板（frpc.toml 的 webServer 段）
	WebServerEnabled  bool   `json:"web_server_enabled"`
	WebServerAddr     string `json:"web_server_addr,omitempty"`
	WebServerPort     int    `json:"web_server_port,omitempty"`
	WebServerUser     string `json:"web_server_user,omitempty"`
	WebServerPassword string `json:"web_server_password,omitempty"`
	WebServerPprof    bool   `json:"web_server_pprof"`

	// 管理面板的代理存储文件（frpc.toml 的 [store] 段）
	StorePath string `json:"store_path,omitempty"`
}

// 账号密钥的存储键：官网账号名全局唯一，统一小写去空格
func accountKey(identity string) string {
	return strings.ToLower(strings.TrimSpace(identity))
}

// 记录账号对应的 Client Secret
func (c *Config) setAccountSecret(identity, secret string) {
	if accountKey(identity) == "" {
		return
	}
	if c.AccountSecrets == nil {
		c.AccountSecrets = map[string]string{}
	}
	c.AccountSecrets[accountKey(identity)] = secret
}

// 读取账号对应的 Client Secret
func (c *Config) getAccountSecret(identity string) string {
	if c.AccountSecrets == nil {
		return ""
	}
	return c.AccountSecrets[accountKey(identity)]
}

// 当前登录账号的 Client Secret
func (c *Config) currentSecret() string {
	if s := c.getAccountSecret(c.User); s != "" {
		return s
	}
	if len(c.AccountSecrets) == 0 {
		// 旧版配置只有单个全局密钥，归属于当前账号
		return c.ClientSecret
	}
	return ""
}

// 记录账号最后选择的服务器
func (c *Config) setAccountServer(identity string, serverID int) {
	if accountKey(identity) == "" || serverID <= 0 {
		return
	}
	if c.AccountServers == nil {
		c.AccountServers = map[string]int{}
	}
	c.AccountServers[accountKey(identity)] = serverID
}

// 读取账号最后选择的服务器
func (c *Config) getAccountServer(identity string) int {
	if c.AccountServers == nil {
		return 0
	}
	return c.AccountServers[accountKey(identity)]
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
	SavedServerID int `json:"saved_server_id,omitempty"` // 该账号上次选择的服务器
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

// 页面展示用的隧道：额外标明来源，store 表示来自 frpc 管理面板的存储文件
type TunnelView struct {
	Tunnel
	Source  string `json:"source"` // config | store
	Enabled bool   `json:"enabled"`
}

var (
	frpcCmd  *exec.Cmd
	frpcDone chan struct{}
	frpcMu   sync.Mutex
)

func main() {
	os.MkdirAll(configDir, 0755)
	os.MkdirAll(filepath.Dir(logFile), 0755)

	// 旧版本写的是 INI 格式配置（frpc.ini），现已改为 frpc.toml；清掉残留文件，避免旧密钥明文留在磁盘上
	os.Remove(filepath.Join(configDir, "frpc.ini"))

	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/user", handleUser)
	http.HandleFunc("/api/plans", handlePlans)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/secret", handleSecret)
	http.HandleFunc("/api/frpc-toml", handleFrpcToml)
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
			// 保存 OIDC 认证状态和服务器密钥状态（从 API 获取）
			if authEnabled, ok := userResult.Data["auth_enabled"]; ok {
				userInfo["auth_enabled"] = authEnabled
			}
			if secretSet, ok := userResult.Data["secret_set"]; ok {
				userInfo["secret_set"] = secretSet
			}
		}
	}

	// 构建响应
	result := LoginResponse{
		OK:      true,
		Servers: serversResult.Servers,
		Config:  oidcConfig,
	}
	log.Printf("Building login response: OK=%v, Servers count=%d", result.OK, len(result.Servers))

	// 保存配置（包含 OIDC 参数和 Client Secret）
	cfg := loadConfig()
	prevUser := cfg.User
	prevSecret := cfg.ClientSecret

	// 账号标识统一用官网用户名（frpc 的 clientID 即用户名），用邮箱登录的也要归一
	username := req.Identity
	if u, ok := userInfo["username"].(string); ok && u != "" {
		username = u
	} else if u, ok := loginResponse.Data.User["username"].(string); ok && u != "" {
		username = u
	}

	// 切换账号时停止旧的 frpc，避免它继续用上一个账号的密钥运行
	if prevUser != "" && prevUser != username {
		frpcMu.Lock()
		stopFrpc()
		frpcMu.Unlock()
	}

	cfg.Enabled = true
	cfg.User = username
	cfg.AuthMethod = "oidc"
	cfg.AuthOIDCIssuer = req.Site
	cfg.LoginToken = loginResponse.Data.Token // 保存登录 token 用于后续 API 调用

	// Client Secret 按账号取用：官网 API 不返回明文时回退到该账号已保存的密钥
	// 旧版配置只有一个全局密钥，先归档到它所属的账号，避免切换账号后丢失
	if prevSecret != "" && prevUser != "" {
		cfg.setAccountSecret(prevUser, prevSecret)
		if prevUser == req.Identity && prevUser != username {
			// 旧配置按登录名（邮箱）记录，迁移到用户名
			cfg.setAccountSecret(username, prevSecret)
		}
	}
	secret := clientSecret
	if secret == "" {
		secret = cfg.getAccountSecret(username)
	}
	cfg.ClientSecret = secret
	cfg.setAccountSecret(username, secret)
	result.Secret = secret
	log.Printf("Client Secret for %s: %v", username, secret != "")
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

	// 恢复该账号上次选择的服务器
	if savedID := cfg.getAccountServer(username); savedID > 0 {
		for _, srv := range result.Servers {
			if srv.ID == savedID {
				cfg.ServerAddr = srv.Addr
				cfg.ServerPort = srv.Port
				result.SavedServerID = savedID
				break
			}
		}
	}

	saveConfig(cfg)

	// 用当前账号的密钥重新生成 frpc 配置，切换账号后直接可用
	writeFrpcConfig(cfg)

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
	// 添加 OIDC 认证状态和服务器密钥状态（如果 API 没有返回）
	userInfo := make(map[string]interface{})
	for k, v := range cfg.UserInfo {
		userInfo[k] = v
	}
	// 只在 userInfo 中没有这些字段时，才使用本地配置判断
	if _, hasAuth := userInfo["auth_enabled"]; !hasAuth {
		userInfo["auth_enabled"] = cfg.AuthMethod == "oidc"
	}
	if _, hasSecret := userInfo["secret_set"]; !hasSecret {
		userInfo["secret_set"] = cfg.ClientSecret != ""
	}
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
		if id, ok := updates["server_id"].(float64); ok {
			// 记录当前账号选择的服务器，下次登录自动选中
			cfg.setAccountServer(cfg.User, int(id))
		}
		if secret, ok := updates["client_secret"].(string); ok {
			cfg.ClientSecret = secret
			cfg.setAccountSecret(cfg.User, secret)
		}
		if enabled, ok := updates["enabled"].(bool); ok {
			cfg.Enabled = enabled
		}

		// frpc 管理面板（webServer）配置
		panelChanged := false
		if v, ok := updates["web_server_enabled"].(bool); ok {
			cfg.WebServerEnabled = v
			panelChanged = true
		}
		if v, ok := updates["web_server_addr"].(string); ok {
			cfg.WebServerAddr = strings.TrimSpace(v)
			panelChanged = true
		}
		if v, ok := updates["web_server_port"].(float64); ok {
			cfg.WebServerPort = int(v)
			panelChanged = true
		}
		if v, ok := updates["web_server_user"].(string); ok {
			cfg.WebServerUser = strings.TrimSpace(v)
			panelChanged = true
		}
		if v, ok := updates["web_server_password"].(string); ok {
			cfg.WebServerPassword = v
			panelChanged = true
		}
		if v, ok := updates["web_server_pprof"].(bool); ok {
			cfg.WebServerPprof = v
			panelChanged = true
		}
		if v, ok := updates["store_path"].(string); ok {
			cfg.StorePath = strings.TrimSpace(v)
			panelChanged = true
		}

		// 启用管理面板时补齐默认配置并随机生成账号密码
		if cfg.WebServerEnabled {
			if cfg.WebServerAddr == "" {
				cfg.WebServerAddr = "0.0.0.0"
			}
			if cfg.WebServerPort <= 0 || cfg.WebServerPort > 65535 {
				cfg.WebServerPort = 7400
			}
			if cfg.WebServerUser == "" {
				cfg.WebServerUser = randomPanelUser()
			}
			if cfg.WebServerPassword == "" {
				cfg.WebServerPassword = randomPanelPassword()
			}
		}

		saveConfig(cfg)

		// 重新生成 frpc 配置（服务器、密钥或面板配置变化）
		_, hasAddr := updates["server_addr"]
		_, hasSecret := updates["client_secret"]
		if hasAddr || hasSecret || panelChanged {
			writeFrpcConfig(cfg)
			log.Printf("Regenerated frpc config")
		}

		// 面板开关变化后重启 frpc，让新配置生效
		restarted := false
		if _, ok := updates["web_server_enabled"]; ok {
			restarted = restartFrpc(true)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":                  true,
			"web_server_enabled":  cfg.WebServerEnabled,
			"web_server_addr":     cfg.WebServerAddr,
			"web_server_port":     cfg.WebServerPort,
			"web_server_user":     cfg.WebServerUser,
			"web_server_password": cfg.WebServerPassword,
			"restarted":           restarted,
		})
	}
}

// 随机生成管理面板账号
func randomPanelUser() string {
	return "frp" + randomPanelSecret(6)
}

// 随机生成管理面板密码
func randomPanelPassword() string {
	return randomPanelSecret(16)
}

// 随机字符集：去掉容易混淆的 0/O/1/l
const panelSecretChars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPanelSecret(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("生成随机密码失败: %v", err)
		return ""
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = panelSecretChars[int(b)%len(panelSecretChars)]
	}
	return string(out)
}

// 查看 / 保存 frpc.toml
// 保存时把文件里的 [[proxies]] 同步到隧道列表，并原样写回配置文件
func handleFrpcToml(w http.ResponseWriter, r *http.Request) {
	cfg := loadConfig()

	switch r.Method {
	case "GET":
		// 还没有生成过就先生成一份，避免弹窗空白
		if _, err := os.Stat(frpcConfigFile); os.IsNotExist(err) {
			writeFrpcConfig(cfg)
		}
		data, err := os.ReadFile(frpcConfigFile)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "content": string(data)})

	case "POST":
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "invalid request"})
			return
		}
		if strings.TrimSpace(req.Content) == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "配置文件内容为空"})
			return
		}

		proxies, err := parseTomlProxies(req.Content)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}

		// 文件里的隧道同步到隧道列表
		cfg.Tunnels = proxies
		saveConfig(cfg)

		if err := os.WriteFile(frpcConfigFile, []byte(req.Content), 0644); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}

		// 重启 frpc 让新配置生效；不重新生成文件，保留用户手写的内容
		restarted := restartFrpc(false)

		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "count": len(proxies), "restarted": restarted})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func handleSecret(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := loadConfig()
		json.NewEncoder(w).Encode(map[string]string{"secret": cfg.currentSecret()})
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
		cfg.setAccountSecret(cfg.User, req.Secret)
		saveConfig(cfg)
		// 同步到 frpc 配置，避免启动时使用旧密钥
		writeFrpcConfig(cfg)
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

// frpc 管理面板的代理存储文件路径
func storeFilePath(cfg Config) string {
	p := strings.TrimSpace(cfg.StorePath)
	if p == "" {
		return filepath.Join(configDir, "frpc_store.json")
	}
	return p
}

// 读取管理面板存储文件里的代理配置
// 未启用管理面板时 frpc 不会加载 [store] 段，存储文件里的隧道也不会生效，故不参与合并
func readStoreProxies(cfg Config) []map[string]interface{} {
	if !cfg.WebServerEnabled {
		return nil
	}
	f, ok := loadStoreFile(cfg)
	if !ok {
		return nil
	}
	return f.Proxies
}

// 代理存储文件内容
type storeFile struct {
	Proxies  []map[string]interface{} `json:"proxies,omitempty"`
	Visitors []json.RawMessage          `json:"visitors,omitempty"`
}

// 读取代理存储文件；不管面板是否启用都读，用于判断隧道是否由面板维护
func loadStoreFile(cfg Config) (storeFile, bool) {
	data, err := os.ReadFile(storeFilePath(cfg))
	if err != nil {
		return storeFile{}, false
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("解析代理存储文件失败: %v", err)
		return storeFile{}, false
	}
	return f, true
}

// 保存代理存储文件
func saveStoreFile(cfg Config, f storeFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(storeFilePath(cfg), data, 0644)
}

// frpc 支持的代理类型
var tunnelTypes = map[string]bool{
	"tcp": true, "udp": true, "http": true, "https": true,
	"tcpmux": true, "stcp": true, "sudp": true, "xtcp": true,
}

// 需要绑定域名的代理类型
func tunnelNeedsDomain(t string) bool {
	return t == "http" || t == "https" || t == "tcpmux"
}

// 把网页里的隧道配置写回代理存储文件，保留面板设置的其他字段
// 存储文件里没有这条隧道时不做任何事，返回 false
func updateStoreProxy(cfg Config, t Tunnel) bool {
	f, ok := loadStoreFile(cfg)
	if !ok {
		return false
	}

	if tunnelIncomplete(t) {
		// 不完整的配置留在存储文件里会让 frpc 启动失败，直接移除
		log.Printf("隧道 %s 配置不完整，已从代理存储文件中移除", t.Name)
		return removeStoreProxy(cfg, t.Name)
	}

	found := false
	for _, p := range f.Proxies {
		name, _ := p["name"].(string)
		if name != t.Name {
			continue
		}
		found = true
		p["type"] = t.Type
		p["localIP"] = t.LocalIP
		p["localPort"] = t.LocalPort

		switch {
		case tunnelNeedsDomain(t.Type):
			delete(p, "remotePort")
			if t.CustomDomain != "" {
				p["customDomains"] = []string{t.CustomDomain}
				delete(p, "subdomain")
			} else {
				delete(p, "customDomains")
				if t.Subdomain != "" {
					p["subdomain"] = t.Subdomain
				} else {
					delete(p, "subdomain")
				}
			}
			delete(p, "secretKey")
		case t.Type == "stcp" || t.Type == "sudp" || t.Type == "xtcp":
			delete(p, "remotePort")
			delete(p, "subdomain")
			delete(p, "customDomains")
			if t.SecretKey != "" {
				p["secretKey"] = t.SecretKey
			} else {
				delete(p, "secretKey")
			}
		default:
			delete(p, "subdomain")
			delete(p, "customDomains")
			delete(p, "secretKey")
			if t.RemotePort != nil && *t.RemotePort > 0 {
				p["remotePort"] = *t.RemotePort
			} else {
				delete(p, "remotePort")
			}
		}
	}

	if !found {
		return false
	}
	if err := saveStoreFile(cfg, f); err != nil {
		log.Printf("写回代理存储文件失败: %v", err)
		return false
	}
	return true
}

// 从代理存储文件里删除隧道；不存在时返回 false
func removeStoreProxy(cfg Config, name string) bool {
	f, ok := loadStoreFile(cfg)
	if !ok {
		return false
	}

	kept := make([]map[string]interface{}, 0, len(f.Proxies))
	found := false
	for _, p := range f.Proxies {
		if n, _ := p["name"].(string); n == name {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return false
	}
	f.Proxies = kept
	if err := saveStoreFile(cfg, f); err != nil {
		log.Printf("写回代理存储文件失败: %v", err)
		return false
	}
	return true
}

// 存储文件里的代理配置转成页面用的隧道
func storeProxyToTunnel(p map[string]interface{}) Tunnel {
	t := Tunnel{}
	t.Name, _ = p["name"].(string)
	t.Type, _ = p["type"].(string)
	t.LocalIP, _ = p["localIP"].(string)
	t.Subdomain, _ = p["subdomain"].(string)
	t.SecretKey, _ = p["secretKey"].(string)
	if v, ok := p["localPort"].(float64); ok {
		t.LocalPort = int(v)
	}
	if v, ok := p["remotePort"].(float64); ok && v > 0 {
		rp := int(v)
		t.RemotePort = &rp
	}
	if domains, ok := p["customDomains"].([]interface{}); ok && len(domains) > 0 {
		if d, ok := domains[0].(string); ok {
			t.CustomDomain = d
		}
	}
	return t
}

// 存储文件里的代理是否处于启用状态
func storeProxyEnabled(p map[string]interface{}) bool {
	if v, ok := p["enabled"].(bool); ok {
		return v
	}
	return true
}

// 合并网页配置与管理面板存储文件里的隧道
// 管理面板新增的隧道会写进 config.json，之后就能在网页里直接编辑
func mergeTunnels(cfg Config) []TunnelView {
	storeProxies := readStoreProxies(cfg)
	storeNames := make(map[string]bool)
	storeEnabled := make(map[string]bool)
	invalid := make([]string, 0)
	changed := false

	for _, p := range storeProxies {
		t := storeProxyToTunnel(p)
		if t.Name == "" {
			continue
		}

		// 存储文件里的配置不合法会让 frpc 启动失败，先记录下来稍后清除；
		// config.json 里没有同名隧道时保留下来，方便在网页里修复
		if tunnelIncomplete(t) {
			invalid = append(invalid, t.Name)
			if tunnelIndex(cfg.Tunnels, t.Name) < 0 {
				cfg.Tunnels = append(cfg.Tunnels, t)
				changed = true
			}
			continue
		}

		storeNames[t.Name] = true
		storeEnabled[t.Name] = storeProxyEnabled(p)

		if idx := tunnelIndex(cfg.Tunnels, t.Name); idx >= 0 {
			// 面板里的记录是最新的，覆盖 config.json 里的同名隧道
			if !tunnelEqual(cfg.Tunnels[idx], t) {
				cfg.Tunnels[idx] = t
				changed = true
			}
		} else {
			cfg.Tunnels = append(cfg.Tunnels, t)
			changed = true
		}
	}

	if changed {
		saveConfig(cfg)
	}

	// 清掉存储文件里不合法的记录，避免 frpc 启动报错
	for _, name := range invalid {
		log.Printf("管理面板隧道 %s 配置不完整，已从代理存储文件中移除", name)
		removeStoreProxy(cfg, name)
	}

	views := make([]TunnelView, 0, len(cfg.Tunnels))
	for _, t := range cfg.Tunnels {
		src := "config"
		enabled := true
		if storeNames[t.Name] {
			src = "store"
			enabled = storeEnabled[t.Name]
		}
		views = append(views, TunnelView{Tunnel: t, Source: src, Enabled: enabled})
	}
	return views
}

// 隧道在列表里的下标，找不到返回 -1
func tunnelIndex(tunnels []Tunnel, name string) int {
	for i, t := range tunnels {
		if t.Name == name {
			return i
		}
	}
	return -1
}

// 两条隧道配置是否一致
func tunnelEqual(a, b Tunnel) bool {
	if a.Name != b.Name || a.Type != b.Type || a.LocalIP != b.LocalIP || a.LocalPort != b.LocalPort {
		return false
	}
	if a.Subdomain != b.Subdomain || a.CustomDomain != b.CustomDomain || a.SecretKey != b.SecretKey {
		return false
	}
	if (a.RemotePort == nil) != (b.RemotePort == nil) {
		return false
	}
	return a.RemotePort == nil || *a.RemotePort == *b.RemotePort
}

func handleTunnels(w http.ResponseWriter, r *http.Request) {
	cfg := loadConfig()

	switch r.Method {
	case "GET":
		// 返回所有隧道：网页配置 + 管理面板存储文件里的
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"tunnels": mergeTunnels(cfg),
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

		// 面板里的隧道同步写回存储文件，否则 frpc 仍会用面板里的旧配置
		updateStoreProxy(cfg, req)

		// 重新生成配置并重启 frpc，让新隧道立即生效
		writeFrpcConfig(cfg)
		restarted := restartFrpc(false)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "restarted": restarted})

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

		// 面板里的隧道要从存储文件里一并删掉，否则 frpc 重启后还会加载
		removeStoreProxy(cfg, req.Name)

		writeFrpcConfig(cfg)
		restarted := restartFrpc(false)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "restarted": restarted})
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
	writeFrpcConfig(cfg)

	if err := startFrpcProcess(cfg); err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// 启动 frpc 进程（调用方需持有 frpcMu 锁）
func startFrpcProcess(cfg Config) error {
	if frpcCmd != nil && frpcCmd.Process != nil {
		return fmt.Errorf("already running")
	}
	if !cfg.Enabled {
		return fmt.Errorf("not enabled")
	}
	if cfg.currentSecret() == "" {
		return fmt.Errorf("尚未设置服务器密钥，请先设置或生成 Client Secret")
	}

	logF, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	cmd := exec.Command(frpcBinary, "-c", frpcConfigFile)
	cmd.Stdout = logF
	cmd.Stderr = logF

	if err := cmd.Start(); err != nil {
		return err
	}
	frpcCmd = cmd

	done := make(chan struct{})
	frpcDone = done
	go func() {
		cmd.Wait()
		close(done)
		frpcMu.Lock()
		// 只有当前进程仍是这个时才清理，避免误清掉重新启动的实例
		if frpcCmd == cmd {
			frpcCmd = nil
		}
		frpcMu.Unlock()
	}()
	return nil
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	frpcMu.Lock()
	defer frpcMu.Unlock()

	stopFrpc()

	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// 停止正在运行的 frpc 进程（调用方需持有 frpcMu 锁）
func stopFrpc() {
	if frpcCmd != nil && frpcCmd.Process != nil {
		cmd := frpcCmd
		done := frpcDone
		frpcCmd = nil
		frpcDone = nil
		cmd.Process.Kill()
		if done != nil {
			// 等进程真正退出，端口才会释放
			<-done
		}
	}
}

// 重启 frpc；仅在运行中重启。regenerate 为 true 时按当前配置重新生成 frpc.toml
func restartFrpc(regenerate bool) bool {
	frpcMu.Lock()
	defer frpcMu.Unlock()

	if frpcCmd == nil || frpcCmd.Process == nil {
		return false
	}

	stopFrpc()

	cfg := loadConfig()
	if regenerate {
		writeFrpcConfig(cfg)
	}
	if err := startFrpcProcess(cfg); err != nil {
		log.Printf("重启 frpc 失败: %v", err)
		return false
	}
	log.Printf("frpc restarted")
	return true
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	frpcMu.Lock()
	running := frpcCmd != nil && frpcCmd.Process != nil
	frpcMu.Unlock()

	resp := map[string]interface{}{"running": running}
	// 当前账号选中的服务器，前端运行中据此回显并锁定下拉框
	cfg := loadConfig()
	if id := cfg.getAccountServer(cfg.User); id > 0 {
		resp["server_id"] = id
	}
	json.NewEncoder(w).Encode(resp)
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
	// 使用当前账号的密钥，避免切号后用错账号
	secret := cfg.currentSecret()
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
`, cfg.ServerAddr, cfg.ServerPort, cfg.User, cfg.User, secret,
		cfg.AuthOIDCTokenEndpoint, cfg.AuthOIDCAudience, cfg.AuthOIDCScope)

	// frpc 管理面板（webServer）
	if cfg.WebServerEnabled {
		addr := strings.TrimSpace(cfg.WebServerAddr)
		if addr == "" {
			addr = "0.0.0.0"
		}
		port := cfg.WebServerPort
		if port <= 0 || port > 65535 {
			port = 7400
		}
		toml += fmt.Sprintf(`
webServer.addr = "%s"
webServer.port = %d
webServer.user = "%s"
webServer.password = "%s"
webServer.pprofEnable = %t
`, escapeToml(addr), port, escapeToml(cfg.WebServerUser), escapeToml(cfg.WebServerPassword), cfg.WebServerPprof)

		// 管理面板的代理存储文件；放在数据卷里，容器重建不丢
		toml += fmt.Sprintf(`
[store]
path = "%s"
`, escapeToml(storeFilePath(cfg)))
	}

	// 添加隧道配置；字段不全的整段注释掉，避免 frpc 启动时报错
	for _, t := range cfg.Tunnels {
		block := tunnelTomlBlock(t)
		if tunnelIncomplete(t) {
			toml += "\n" + commentTomlBlock(block)
			continue
		}
		toml += "\n" + block
	}

	os.WriteFile(frpcConfigFile, []byte(toml), 0644)
}

// 隧道的 frpc.toml 配置块
func tunnelTomlBlock(t Tunnel) string {
	block := fmt.Sprintf(`[[proxies]]
name = "%s"
type = "%s"
localIP = "%s"
localPort = %d
`, escapeToml(t.Name), escapeToml(t.Type), escapeToml(t.LocalIP), t.LocalPort)

	if tunnelNeedsDomain(t.Type) {
		if t.CustomDomain != "" {
			block += fmt.Sprintf(`customDomains = ["%s"]
`, escapeToml(t.CustomDomain))
		} else if t.Subdomain != "" {
			block += fmt.Sprintf(`subdomain = "%s"
`, escapeToml(t.Subdomain))
		}
	} else if t.Type == "stcp" || t.Type == "sudp" || t.Type == "xtcp" {
		if t.SecretKey != "" {
			block += fmt.Sprintf(`secretKey = "%s"
`, escapeToml(t.SecretKey))
		}
	} else {
		if t.RemotePort != nil && *t.RemotePort > 0 {
			block += fmt.Sprintf(`remotePort = %d
`, *t.RemotePort)
		}
	}
	return block
}

// 隧道配置是否不完整；不完整的配置不能交给 frpc，否则启动会直接失败
func tunnelIncomplete(t Tunnel) bool {
	if t.Name == "" || t.Type == "" || t.LocalIP == "" {
		return true
	}
	if !tunnelTypes[t.Type] {
		return true
	}
	if t.LocalPort <= 0 || t.LocalPort > 65535 {
		return true
	}
	if tunnelNeedsDomain(t.Type) {
		return t.Subdomain == "" && t.CustomDomain == ""
	}
	return false
}

// 把配置块整段注释掉
func commentTomlBlock(block string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = "# " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func escapeToml(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

// 从 TOML 内容里解析 [[proxies]] 段，转成隧道列表
// 整段被注释掉的代理也会解析出来，保留在列表里但仍以注释形式写回
func parseTomlProxies(content string) ([]Tunnel, error) {
	lines := strings.Split(content, "\n")
	var (
		proxies       []Tunnel
		cur           Tunnel
		inProxy       bool
		curCommented  bool
		commentedList []bool
	)

	flush := func() {
		if inProxy {
			proxies = append(proxies, cur)
			commentedList = append(commentedList, curCommented)
			cur = Tunnel{}
		}
	}

	for i := 0; i < len(lines); i++ {
		raw := strings.TrimSpace(lines[i])
		commented := strings.HasPrefix(raw, "#")
		if commented {
			// 去掉行首注释符，剩下的内容才能当作配置解析
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "#"))
		}
		line := strings.TrimSpace(stripTomlComment(raw))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") {
			flush()
			inProxy = strings.HasPrefix(line, "[[") && strings.TrimSpace(strings.Trim(line, "[]")) == "proxies"
			curCommented = commented
			continue
		}

		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		// 数组可能跨行
		if strings.HasPrefix(val, "[") && !strings.HasSuffix(val, "]") {
			for i+1 < len(lines) {
				i++
				next := strings.TrimSpace(lines[i])
				if strings.HasPrefix(next, "#") != commented {
					break
				}
				if commented {
					next = strings.TrimSpace(strings.TrimPrefix(next, "#"))
				}
				val += " " + strings.TrimSpace(stripTomlComment(next))
				if strings.HasSuffix(strings.TrimSpace(val), "]") {
					break
				}
			}
		}

		if !inProxy {
			continue
		}
		// 注释块里只认注释行，正常块里只认正常行
		if commented != curCommented {
			continue
		}

		switch key {
		case "name":
			cur.Name = parseTomlScalar(val)
		case "type":
			cur.Type = parseTomlScalar(val)
		case "localIP":
			cur.LocalIP = parseTomlScalar(val)
		case "localPort":
			cur.LocalPort = atoiOrZero(parseTomlScalar(val))
		case "remotePort":
			if n := atoiOrZero(parseTomlScalar(val)); n > 0 {
				cur.RemotePort = &n
			}
		case "subdomain":
			cur.Subdomain = parseTomlScalar(val)
		case "secretKey":
			cur.SecretKey = parseTomlScalar(val)
		case "customDomains":
			if domains := parseTomlStringArray(val); len(domains) > 0 {
				cur.CustomDomain = domains[0]
			}
		}
	}
	flush()

	for i, t := range proxies {
		// 注释掉的代理允许不完整，写回时仍是注释状态
		if commentedList[i] {
			continue
		}
		if t.Name == "" || t.Type == "" || t.LocalIP == "" || t.LocalPort == 0 {
			return nil, fmt.Errorf("隧道配置不完整（name/type/localIP/localPort 必填）")
		}
	}
	return proxies, nil
}

// 去掉行尾注释，字符串里的 # 不算注释
func stripTomlComment(line string) string {
	var quote byte
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			quote = c
		case '#':
			return line[:i]
		}
	}
	return line
}

// 解析 TOML 标量值，非字符串返回原始文本
func parseTomlScalar(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return unescapeTomlString(raw[1 : len(raw)-1])
	}
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// 解析 TOML 字符串数组
func parseTomlStringArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")

	items := []string{}
	var cur strings.Builder
	var quote byte
	inStr := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inStr {
			cur.WriteByte(c)
			if c == '\\' && quote == '"' && i+1 < len(raw) {
				i++
				cur.WriteByte(raw[i])
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			quote = c
			cur.WriteByte(c)
		case ',':
			items = append(items, parseTomlScalar(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		items = append(items, parseTomlScalar(s))
	}
	return items
}

func unescapeTomlString(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
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
