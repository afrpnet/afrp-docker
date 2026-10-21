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
