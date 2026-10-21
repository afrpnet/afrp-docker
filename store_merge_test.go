package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 把运行时路径指向临时目录，测试结束后还原
func useTempPaths(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldFile, oldToml, oldLog := configDir, configFile, frpcConfigFile, logFile
	configDir = dir
	configFile = filepath.Join(dir, "config.json")
	frpcConfigFile = filepath.Join(dir, "frpc.toml")
	logFile = filepath.Join(dir, "frpc.log")
	t.Cleanup(func() {
		configDir, configFile, frpcConfigFile, logFile = oldDir, oldFile, oldToml, oldLog
	})
	return dir
}

// 读取存储文件里的代理名称
func storeProxyNames(t *testing.T, cfg Config) []string {
	t.Helper()
	f, ok := loadStoreFile(cfg)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(f.Proxies))
	for _, p := range f.Proxies {
		name, _ := p["name"].(string)
		names = append(names, name)
	}
	return names
}

// 面板新增的隧道会并进 config.json，并标记为 store 来源
func TestMergeTunnelsAdoptsStoreProxy(t *testing.T) {
	useTempPaths(t)

	cfg := Config{
		WebServerEnabled: true,
		Tunnels: []Tunnel{
			{Name: "web", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080},
		},
	}
	saveConfig(cfg)

	store := storeFile{Proxies: []map[string]interface{}{
		{"name": "site", "type": "http", "localIP": "127.0.0.1", "localPort": 80, "subdomain": "demo"},
	}}
	if err := saveStoreFile(cfg, store); err != nil {
		t.Fatalf("写存储文件失败: %v", err)
	}

	views := mergeTunnels(loadConfig())
	if len(views) != 2 {
		t.Fatalf("期望 2 条隧道，实际 %d: %+v", len(views), views)
	}

	var found bool
	for _, v := range views {
		if v.Name != "site" {
			continue
		}
		found = true
		if v.Source != "store" {
			t.Errorf("site 来源应为 store，实际 %s", v.Source)
		}
		if v.Subdomain != "demo" {
			t.Errorf("site 子域名未合并: %+v", v.Tunnel)
		}
	}
	if !found {
		t.Fatal("面板里的隧道没有出现在列表中")
	}

	// config.json 里应已写入这条隧道
	saved := loadConfig()
	idx := tunnelIndex(saved.Tunnels, "site")
	if idx < 0 {
		t.Fatalf("config.json 未写入面板隧道: %+v", saved.Tunnels)
	}
	if saved.Tunnels[idx].Subdomain != "demo" {
		t.Errorf("config.json 里的面板隧道字段不对: %+v", saved.Tunnels[idx])
	}
}

// 存储文件里不合法的记录会让 frpc 启动失败，合并时清掉，配置保留待修复
func TestMergeTunnelsDropsInvalidStoreProxy(t *testing.T) {
	useTempPaths(t)

	cfg := Config{WebServerEnabled: true}
	saveConfig(cfg)

	store := storeFile{Proxies: []map[string]interface{}{
		{"name": "bad", "type": "http", "localIP": "127.0.0.1", "localPort": 80},
		{"name": "ok", "type": "tcp", "localIP": "127.0.0.1", "localPort": 8080},
	}}
	if err := saveStoreFile(cfg, store); err != nil {
		t.Fatalf("写存储文件失败: %v", err)
	}

	views := mergeTunnels(loadConfig())
	if len(views) != 2 {
		t.Fatalf("期望 2 条隧道，实际 %d: %+v", len(views), views)
	}

	names := storeProxyNames(t, cfg)
	if len(names) != 1 || names[0] != "ok" {
		t.Errorf("存储文件里的非法记录应被清掉，实际 %v", names)
	}

	// 非法配置保留在 config.json 里，方便在网页里修改
	saved := loadConfig()
	if tunnelIndex(saved.Tunnels, "bad") < 0 {
		t.Errorf("非法配置应保留在 config.json 里待修复: %+v", saved.Tunnels)
	}
}

// 网页里修改面板的隧道时同步写回存储文件，面板设置的其他字段保留
func TestUpdateStoreProxyWritesBack(t *testing.T) {
	useTempPaths(t)

	cfg := Config{WebServerEnabled: true}
	store := storeFile{Proxies: []map[string]interface{}{
		{
			"name": "site", "type": "http", "localIP": "127.0.0.1", "localPort": 80,
			"subdomain": "demo", "annotations": map[string]interface{}{"note": "keep"},
		},
	}}
	if err := saveStoreFile(cfg, store); err != nil {
		t.Fatalf("写存储文件失败: %v", err)
	}

	rp := 18080
	if !updateStoreProxy(cfg, Tunnel{Name: "site", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 8080, RemotePort: &rp}) {
		t.Fatal("updateStoreProxy 未写回存储文件")
	}

	f, _ := loadStoreFile(cfg)
	if len(f.Proxies) != 1 {
		t.Fatalf("存储文件记录数不对: %+v", f.Proxies)
	}
	p := f.Proxies[0]
	if p["type"] != "tcp" {
		t.Errorf("类型未更新: %+v", p)
	}
	if _, ok := p["remotePort"]; !ok {
		t.Errorf("远程端口未写入: %+v", p)
	}
	if _, ok := p["subdomain"]; ok {
		t.Errorf("子域名未清除: %+v", p)
	}
	if _, ok := p["annotations"]; !ok {
		t.Errorf("面板设置的其他字段被覆盖了: %+v", p)
	}
}

// 配置不完整时不该写进存储文件，否则 frpc 启动会失败
func TestUpdateStoreProxyRemovesIncomplete(t *testing.T) {
	useTempPaths(t)

	cfg := Config{WebServerEnabled: true}
	store := storeFile{Proxies: []map[string]interface{}{
		{"name": "site", "type": "http", "localIP": "127.0.0.1", "localPort": 80, "subdomain": "demo"},
	}}
	if err := saveStoreFile(cfg, store); err != nil {
		t.Fatalf("写存储文件失败: %v", err)
	}

	// http 没填域名，配置不完整
	updateStoreProxy(cfg, Tunnel{Name: "site", Type: "http", LocalIP: "127.0.0.1", LocalPort: 80})

	names := storeProxyNames(t, cfg)
	if len(names) != 0 {
		t.Errorf("不完整的配置应从存储文件移除，实际 %v", names)
	}
}

// 未启用管理面板时不读取存储文件
func TestMergeTunnelsSkipsStoreWhenPanelOff(t *testing.T) {
	useTempPaths(t)

	cfg := Config{WebServerEnabled: false}
	saveConfig(cfg)
	store := storeFile{Proxies: []map[string]interface{}{
		{"name": "site", "type": "tcp", "localIP": "127.0.0.1", "localPort": 8080},
	}}
	if err := saveStoreFile(cfg, store); err != nil {
		t.Fatalf("写存储文件失败: %v", err)
	}

	views := mergeTunnels(loadConfig())
	if len(views) != 0 {
		t.Fatalf("未启用面板时不应合并存储文件，实际 %+v", views)
	}
}

// 存储文件内容损坏时不影响主流程
func TestLoadStoreFileBroken(t *testing.T) {
	useTempPaths(t)

	cfg := Config{WebServerEnabled: true}
	if err := os.WriteFile(storeFilePath(cfg), []byte("{invalid"), 0644); err != nil {
		t.Fatalf("写存储文件失败: %v", err)
	}
	if _, ok := loadStoreFile(cfg); ok {
		t.Error("损坏的存储文件应返回失败")
	}
}

// 存储文件里的隧道能正确转成网页用的结构
func TestStoreProxyToTunnel(t *testing.T) {
	p := map[string]interface{}{
		"name": "site", "type": "https", "localIP": "127.0.0.1", "localPort": 443.0,
		"customDomains": []interface{}{"a.example.com"},
	}
	tun := storeProxyToTunnel(p)
	if tun.Name != "site" || tun.Type != "https" || tun.LocalPort != 443 {
		t.Errorf("转换结果不对: %+v", tun)
	}
	if tun.CustomDomain != "a.example.com" {
		t.Errorf("自定义域名没取到: %+v", tun)
	}

	// 数字类型不一致时也要能解析
	p2 := map[string]interface{}{"name": "x", "type": "tcp", "localIP": "127.0.0.1", "localPort": json.Number("8080")}
	if tun := storeProxyToTunnel(p2); tun.LocalPort != 0 {
		t.Errorf("非 float64 的端口应保持 0: %+v", tun)
	}
}
