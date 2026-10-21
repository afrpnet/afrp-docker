package main

import "testing"

func TestParseTomlProxies(t *testing.T) {
	content := `# 由 AFRP Web UI 自动生成，请勿手动修改（修改会在下次连接时覆盖）
serverAddr = "test.afrp.net"
serverPort = 7000
user = "darny"

webServer.addr = "0.0.0.0"
webServer.port = 7400

[store]
path = "/data/config/frpc_store.json"

[[proxies]]
name = "web-local"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
remotePort = 18080

[[proxies]]
name = "site" # 带注释的名字
type = "http"
localIP = "127.0.0.1"
localPort = 80
subdomain = "demo"

[[proxies]]
name = "site2"
type = "https"
localIP = "127.0.0.1"
localPort = 443
customDomains = [
  "a.example.com",
  "b.example.com"
]

[[proxies]]
name = "p2p"
type = "stcp"
localIP = "127.0.0.1"
localPort = 9000
secretKey = "abc123"

[[proxies]]
name = "udp-1"
type = "udp"
localIP = "127.0.0.1"
localPort = 5353
remotePort = 15353
`
	proxies, err := parseTomlProxies(content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(proxies) != 5 {
		t.Fatalf("expected 5 proxies, got %d: %+v", len(proxies), proxies)
	}

	p := proxies[0]
	if p.Name != "web-local" || p.Type != "tcp" || p.LocalIP != "127.0.0.1" || p.LocalPort != 8080 {
		t.Errorf("proxy0 wrong: %+v", p)
	}
	if p.RemotePort == nil || *p.RemotePort != 18080 {
		t.Errorf("proxy0 remotePort wrong: %+v", p.RemotePort)
	}

	p = proxies[1]
	if p.Name != "site" || p.Type != "http" || p.Subdomain != "demo" || p.LocalPort != 80 {
		t.Errorf("proxy1 wrong: %+v", p)
	}
	if p.RemotePort != nil {
		t.Errorf("proxy1 should have no remotePort: %+v", p.RemotePort)
	}

	p = proxies[2]
	if p.Name != "site2" || p.Type != "https" || p.CustomDomain != "a.example.com" {
		t.Errorf("proxy2 wrong: %+v", p)
	}

	p = proxies[3]
	if p.Name != "p2p" || p.Type != "stcp" || p.SecretKey != "abc123" {
		t.Errorf("proxy3 wrong: %+v", p)
	}

	p = proxies[4]
	if p.Name != "udp-1" || p.Type != "udp" || p.RemotePort == nil || *p.RemotePort != 15353 {
		t.Errorf("proxy4 wrong: %+v", p)
	}
}

func TestParseTomlProxiesEmpty(t *testing.T) {
	proxies, err := parseTomlProxies("serverAddr = \"x\"\n[store]\npath = \"/a\"\n")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("expected 0 proxies, got %d", len(proxies))
	}
}

func TestParseTomlProxiesIncomplete(t *testing.T) {
	_, err := parseTomlProxies("[[proxies]]\nname = \"a\"\ntype = \"tcp\"\n")
	if err == nil {
		t.Fatal("expected error for incomplete proxy")
	}
}

// 整段注释掉的代理也要能读回来，避免保存配置文件时丢失
func TestParseTomlProxiesCommented(t *testing.T) {
	content := `[[proxies]]
name = "good"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080

# [[proxies]]
# name = "broken"
# type = "tcp"
# localIP = "127.0.0.1"
# localPort = 0
`
	proxies, err := parseTomlProxies(content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d: %+v", len(proxies), proxies)
	}
	if proxies[1].Name != "broken" || proxies[1].LocalPort != 0 {
		t.Errorf("commented proxy wrong: %+v", proxies[1])
	}
}

// 正常块里的注释行不参与解析
func TestParseTomlProxiesCommentInsideBlock(t *testing.T) {
	content := `[[proxies]]
name = "good"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
# localPort = 9999
`
	proxies, err := parseTomlProxies(content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(proxies))
	}
	if proxies[0].LocalPort != 8080 {
		t.Errorf("commented key should be ignored: %+v", proxies[0])
	}
}

func TestTunnelIncomplete(t *testing.T) {
	cases := []struct {
		name string
		t    Tunnel
		want bool
	}{
		{"完整", Tunnel{Name: "a", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80}, false},
		{"缺名字", Tunnel{Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80}, true},
		{"缺类型", Tunnel{Name: "a", LocalIP: "127.0.0.1", LocalPort: 80}, true},
		{"缺本地IP", Tunnel{Name: "a", Type: "tcp", LocalPort: 80}, true},
		{"缺本地端口", Tunnel{Name: "a", Type: "tcp", LocalIP: "127.0.0.1"}, true},
		{"端口越界", Tunnel{Name: "a", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 70000}, true},
		{"http缺域名", Tunnel{Name: "a", Type: "http", LocalIP: "127.0.0.1", LocalPort: 80}, true},
		{"http有域名", Tunnel{Name: "a", Type: "http", LocalIP: "127.0.0.1", LocalPort: 80, Subdomain: "x"}, false},
	}
	for _, c := range cases {
		if got := tunnelIncomplete(c.t); got != c.want {
			t.Errorf("%s: tunnelIncomplete = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCommentTomlBlock(t *testing.T) {
	block := buildProxyBlock(Tunnel{Name: "a", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80}, nil)
	got := commentTomlBlock(block)
	want := "# [[proxies]]\n# name = \"a\"\n# type = \"tcp\"\n# localIP = \"127.0.0.1\"\n# localPort = 80\n"
	if got != want {
		t.Errorf("commentTomlBlock =\n%q\nwant\n%q", got, want)
	}
}

func TestTunnelEqual(t *testing.T) {
	rp := 8080
	a := Tunnel{Name: "a", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80, RemotePort: &rp}
	b := Tunnel{Name: "a", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: 80, RemotePort: &rp}
	if !tunnelEqual(a, b) {
		t.Error("expected equal tunnels")
	}
	b.LocalPort = 81
	if tunnelEqual(a, b) {
		t.Error("expected different tunnels")
	}
	b.LocalPort = 80
	b.RemotePort = nil
	if tunnelEqual(a, b) {
		t.Error("expected different tunnels when remotePort differs")
	}
}
