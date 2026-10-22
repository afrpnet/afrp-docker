package main

import (
	"os"
	"strings"
	"testing"
)

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.71.1", "0.71.0", 1},
		{"0.71.0", "0.71.1", -1},
		{"0.71.1", "0.71.1", 0},
		{"v0.71.1", "0.71.1", 0},
		{"0.72.0", "0.71.9", 1},
		{"0.71", "0.71.0", 0},
		{"1.0.0", "0.99.99", 1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFindCoreEntry(t *testing.T) {
	doc := &manifestDoc{Items: []manifestItem{
		{Kind: "app", Platform: "windows", Arch: "amd64", Version: "9.9.9", URL: "http://x/app.exe", Latest: true},
		{Kind: "frpc", Platform: "linux", Arch: "amd64", Version: "0.70.0", URL: "http://x/old"},
		{Kind: "frpc", Platform: "linux", Arch: "amd64", Version: "0.71.1", URL: "http://x/new", Latest: true},
		{Kind: "frpc", Platform: "linux", Arch: "arm64", Version: "0.71.2", URL: "http://x/arm", Latest: true},
	}}

	it := findCoreEntry(doc, "amd64")
	if it == nil {
		t.Fatal("未找到 amd64 条目")
	}
	if it.Version != "0.71.1" {
		t.Errorf("版本 = %q, want 0.71.1", it.Version)
	}
	if it.URL != "http://x/new" {
		t.Errorf("地址 = %q, want http://x/new", it.URL)
	}

	if it := findCoreEntry(doc, "mips64le"); it != nil {
		t.Errorf("不应匹配 mips64le，实际得到 %q", it.URL)
	}
}

func TestFetchManifestAndChecksum(t *testing.T) {
	if os.Getenv("AFRP_TEST_NETWORK") != "1" {
		t.Skip("设置 AFRP_TEST_NETWORK=1 后运行网络用例")
	}

	doc, err := fetchManifest("")
	if err != nil {
		t.Fatalf("拉取清单失败: %v", err)
	}
	it := findCoreEntry(doc, "amd64")
	if it == nil {
		t.Fatal("清单里没有 linux/amd64 的 frpc 条目")
	}
	if it.SHA256 == "" {
		t.Fatal("清单条目缺少 sha256")
	}
	t.Logf("最新版本 %s，地址 %s", it.Version, it.URL)

	dest := t.TempDir() + "/frpc"
	sum, err := downloadToFile(it.URL, dest)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if !strings.EqualFold(sum, it.SHA256) {
		t.Errorf("sha256 不匹配：下载得到 %s，清单写的是 %s", sum, it.SHA256)
	}
}
