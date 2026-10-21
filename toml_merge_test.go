package main

import (
	"os"
	"strings"
	"testing"
)

// 写一份 frpc.toml，模拟管理面板直接改配置文件
func writeDiskToml(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(frpcConfigFile, []byte(content), 0644); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}
}

// 配置文件里新增的隧道要并进 config.json，刷新后能列出来
func TestMergeTunnelsAdoptsTomlProxy(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{})

	writeDiskToml(t, `[[proxies]]
name = "panel-tcp"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
remotePort = 18080
`)

	views := mergeTunnels(loadConfig())
	if len(views) != 1 {
		t.Fatalf("期望 1 条隧道，实际 %d: %+v", len(views), views)
	}
	if views[0].Name != "panel-tcp" {
		t.Fatalf("隧道名不对: %+v", views[0])
	}
	if views[0].Source != "toml" {
		t.Errorf("来源应为 toml，实际 %s", views[0].Source)
	}
	if views[0].LocalPort != 8080 {
		t.Errorf("本地端口不对: %+v", views[0])
	}

	// config.json 里应已写入这条隧道
	saved := loadConfig()
	idx := tunnelIndex(saved.Tunnels, "panel-tcp")
	if idx < 0 {
		t.Fatalf("config.json 未写入配置文件里的隧道: %+v", saved.Tunnels)
	}
	if saved.Tunnels[idx].Origin != "toml" {
		t.Errorf("来源标记不对: %+v", saved.Tunnels[idx])
	}
}

// 配置文件里改了已有隧道的字段时以文件为准
func TestMergeTunnelsTomlOverridesConfig(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{Tunnels: []Tunnel{
		{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080},
	}})

	writeDiskToml(t, `[[proxies]]
name = "web"
type = "tcp"
localIP = "127.0.0.1"
localPort = 9090
`)

	views := mergeTunnels(loadConfig())
	if len(views) != 1 {
		t.Fatalf("期望 1 条隧道，实际 %d", len(views))
	}
	if views[0].LocalPort != 9090 {
		t.Errorf("应以配置文件为准，实际端口 %d", views[0].LocalPort)
	}

	saved := loadConfig()
	if saved.Tunnels[0].LocalPort != 9090 {
		t.Errorf("config.json 未同步: %+v", saved.Tunnels[0])
	}
}

// 注释掉的代理不覆盖已有配置，也不并进列表
func TestMergeTunnelsIgnoresCommentedToml(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{Tunnels: []Tunnel{
		{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080},
	}})

	writeDiskToml(t, `# [[proxies]]
# name = "web"
# type = "tcp"
# localIP = "127.0.0.1"
# localPort = 9999
`)

	views := mergeTunnels(loadConfig())
	if len(views) != 1 {
		t.Fatalf("期望 1 条隧道，实际 %d", len(views))
	}
	if views[0].LocalPort != 8080 {
		t.Errorf("注释里的代理不该覆盖配置，实际端口 %d", views[0].LocalPort)
	}
}

// 重新生成配置时，配置文件里新增的隧道要保留下来
func TestWriteFrpcConfigKeepsTomlProxies(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{Tunnels: []Tunnel{
		{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080},
	}})

	writeDiskToml(t, `[[proxies]]
name = "panel-tcp"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8081
`)

	writeFrpcConfig(loadConfig())

	data, err := os.ReadFile(frpcConfigFile)
	if err != nil {
		t.Fatalf("读配置文件失败: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `name = "panel-tcp"`) {
		t.Errorf("配置文件里的隧道被覆盖掉了:\n%s", content)
	}
	if !strings.Contains(content, `name = "web"`) {
		t.Errorf("原有隧道丢失:\n%s", content)
	}

	// 并进来的隧道要写进 config.json
	saved := loadConfig()
	if tunnelIndex(saved.Tunnels, "panel-tcp") < 0 {
		t.Errorf("config.json 未写入: %+v", saved.Tunnels)
	}
}

// 面板里设置的其它字段在重新生成配置后要保留
func TestWriteFrpcConfigKeepsUnmanagedFields(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{Tunnels: []Tunnel{
		{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080},
	}})

	writeDiskToml(t, `[[proxies]]
name = "web"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
remotePort = 18080
transport.useEncryption = true
transport.useCompression = true
`)

	writeFrpcConfig(loadConfig())

	data, _ := os.ReadFile(frpcConfigFile)
	content := string(data)
	if !strings.Contains(content, "transport.useEncryption = true") {
		t.Errorf("面板设置的字段丢失:\n%s", content)
	}
	if !strings.Contains(content, "remotePort = 18080") {
		t.Errorf("远程端口丢失:\n%s", content)
	}
}

// 删除的隧道不能被配置文件里的旧记录并回来
func TestWriteFrpcConfigExceptKeepsDeletion(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{Tunnels: []Tunnel{
		{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080},
		{Name: "gone", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8081},
	}})

	writeDiskToml(t, `[[proxies]]
name = "web"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080

[[proxies]]
name = "gone"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8081
`)

	cfg := loadConfig()
	newTunnels := make([]Tunnel, 0, len(cfg.Tunnels))
	for _, tun := range cfg.Tunnels {
		if tun.Name != "gone" {
			newTunnels = append(newTunnels, tun)
		}
	}
	cfg.Tunnels = newTunnels
	saveConfig(cfg)

	writeFrpcConfigExcept(cfg, map[string]bool{"gone": true})

	data, _ := os.ReadFile(frpcConfigFile)
	content := string(data)
	if strings.Contains(content, `name = "gone"`) {
		t.Errorf("已删除的隧道又回来了:\n%s", content)
	}
	if !strings.Contains(content, `name = "web"`) {
		t.Errorf("其它隧道丢失:\n%s", content)
	}

	saved := loadConfig()
	if tunnelIndex(saved.Tunnels, "gone") >= 0 {
		t.Errorf("已删除的隧道被并回 config.json: %+v", saved.Tunnels)
	}
}

// 配置文件里没写 localIP 时按 frpc 的默认值补上
func TestMergeTunnelsTomlDefaultLocalIP(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{})
	writeDiskToml(t, `[[proxies]]
name = "no-ip"
type = "tcp"
localPort = 8080
`)

	views := mergeTunnels(loadConfig())
	if len(views) != 1 {
		t.Fatalf("期望 1 条隧道，实际 %d", len(views))
	}
	if views[0].LocalIP != "127.0.0.1" {
		t.Errorf("本地 IP 应默认为 127.0.0.1，实际 %q", views[0].LocalIP)
	}
	if tunnelIncomplete(views[0].Tunnel) {
		t.Errorf("补齐默认值后不应判为不完整: %+v", views[0].Tunnel)
	}
}

// 配置文件里改过面板账号密码后要同步进配置
func TestMergeTomlPanelAccount(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{WebServerEnabled: true, WebServerUser: "old", WebServerPassword: "oldpass"})
	writeDiskToml(t, `webServer.addr = "0.0.0.0"
webServer.port = 7400
webServer.user = "newuser"
webServer.password = "newpass"
`)

	cfg, changed := mergeTomlPanelAccount(loadConfig())
	if !changed {
		t.Fatal("应检测到账号密码变化")
	}
	if cfg.WebServerUser != "newuser" || cfg.WebServerPassword != "newpass" {
		t.Errorf("账号密码没同步: %+v", cfg)
	}
}

// 重新生成配置时保留用户改过的面板账号密码，并同步回 config.json
func TestWriteFrpcConfigKeepsPanelAccount(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{WebServerEnabled: true, WebServerUser: "old", WebServerPassword: "oldpass"})
	writeDiskToml(t, `webServer.addr = "0.0.0.0"
webServer.port = 7400
webServer.user = "newuser"
webServer.password = "newpass"
`)

	writeFrpcConfig(loadConfig())

	data, _ := os.ReadFile(frpcConfigFile)
	content := string(data)
	if !strings.Contains(content, `webServer.user = "newuser"`) {
		t.Errorf("账号被旧值覆盖:\n%s", content)
	}
	if !strings.Contains(content, `webServer.password = "newpass"`) {
		t.Errorf("密码被旧值覆盖:\n%s", content)
	}

	saved := loadConfig()
	if saved.WebServerUser != "newuser" || saved.WebServerPassword != "newpass" {
		t.Errorf("config.json 未同步: %+v", saved)
	}
}

// 配置文件里没有面板段时保持原有账号密码
func TestMergeTomlPanelAccountKeepsConfig(t *testing.T) {
	useTempPaths(t)

	saveConfig(Config{WebServerEnabled: true, WebServerUser: "u", WebServerPassword: "p"})
	writeDiskToml(t, "serverAddr = \"x\"\n")

	cfg, changed := mergeTomlPanelAccount(loadConfig())
	if changed {
		t.Error("配置文件里没有面板段时不应有变化")
	}
	if cfg.WebServerUser != "u" || cfg.WebServerPassword != "p" {
		t.Errorf("账号密码不该被改动: %+v", cfg)
	}
}
