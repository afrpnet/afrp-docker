package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// frpc 核心有两份：
//   镜像内置基线 /usr/local/bin/frpc ——随镜像固定，升级镜像才能换
//   持久卷里的 /data/core/frpc ——面板一键升级后的版本，重建容器不丢
const (
	builtinFrpcBinary  = "/usr/local/bin/frpc"
	builtinVersionFile = "/usr/local/share/frpc-version"
	coreDir            = "/data/core"
	coreBinary         = "/data/core/frpc"
)

// 版本清单：网站源站取不到时用 R2 副本
var manifestUrls = []string{
	"https://www.afrp.net/download/manifest.json",
	"https://dl.afrp.net/manifest.json",
}

var versionRe = regexp.MustCompile(`([0-9]+\.[0-9]+\.[0-9]+)`)

type manifestItem struct {
	Kind     string `json:"kind"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	File     string `json:"file"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Date     string `json:"date"`
	Notes    string `json:"notes"`
	Latest   bool   `json:"latest"`
}

type manifestDoc struct {
	Updated string         `json:"updated"`
	Items   []manifestItem `json:"items"`
}

// 当前生效的核心路径：优先用升级过的副本，没有则用镜像内置的
func activeCorePath() string {
	if _, err := os.Stat(coreBinary); err == nil {
		return coreBinary
	}
	return builtinFrpcBinary
}

// 读取可执行文件版本号
func binaryVersion(path string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	out, err := exec.Command(path, "-v").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return versionRe.FindString(string(out))
}

// 镜像内置的基线版本，构建时写入标记文件
func builtinCoreVersion() string {
	if data, err := os.ReadFile(builtinVersionFile); err == nil {
		if m := versionRe.FindString(string(data)); m != "" {
			return m
		}
	}
	return binaryVersion(builtinFrpcBinary)
}

// 正在使用的核心版本
func currentCoreVersion() string {
	if v := binaryVersion(activeCorePath()); v != "" {
		return v
	}
	return builtinCoreVersion()
}

// 比较版本号，返回 1/0/-1
func compareVersion(a, b string) int {
	as := strings.Split(strings.TrimPrefix(strings.TrimSpace(a), "v"), ".")
	bs := strings.Split(strings.TrimPrefix(strings.TrimSpace(b), "v"), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(as) {
			x = atoiOrZero(as[i])
		}
		if i < len(bs) {
			y = atoiOrZero(bs[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// 拉取版本清单，站点地址优先，取不到再走 R2 副本
func fetchManifest(site string) (*manifestDoc, error) {
	urls := []string{}
	if site != "" {
		urls = append(urls, strings.TrimRight(site, "/")+"/download/manifest.json")
	}
	urls = append(urls, manifestUrls...)

	var lastErr error
	for _, u := range urls {
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var doc manifestDoc
		if err := json.Unmarshal(body, &doc); err != nil {
			lastErr = err
			continue
		}
		return &doc, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的清单地址")
	}
	return nil, lastErr
}

// 找出当前架构可用的 frpc 条目
func findCoreEntry(doc *manifestDoc, arch string) *manifestItem {
	var best *manifestItem
	for i := range doc.Items {
		it := &doc.Items[i]
		if it.Kind != "frpc" || !it.Latest {
			continue
		}
		if it.Platform != "linux" && it.Platform != "openwrt" {
			continue
		}
		if it.Arch != "" && it.Arch != "all" && it.Arch != arch {
			continue
		}
		if it.URL == "" {
			continue
		}
		if best == nil || compareVersion(it.Version, best.Version) > 0 {
			best = it
		}
	}
	return best
}

// 下载文件并返回 sha256
func downloadToFile(url, dest string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(dest)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// 核心信息：当前版本、内置版本、清单里的最新版本
func handleCoreInfo(w http.ResponseWriter, r *http.Request) {
	cfg := loadConfig()
	cur := currentCoreVersion()
	resp := map[string]interface{}{
		"current_version": cur,
		"builtin_version": builtinCoreVersion(),
		"upgraded":        activeCorePath() == coreBinary,
		"arch":            runtime.GOARCH,
	}

	doc, err := fetchManifest(cfg.AuthOIDCIssuer)
	if err != nil {
		resp["error"] = "获取版本清单失败：" + err.Error()
		json.NewEncoder(w).Encode(resp)
		return
	}
	it := findCoreEntry(doc, runtime.GOARCH)
	if it == nil {
		resp["error"] = "清单中没有适用于当前架构的核心"
		json.NewEncoder(w).Encode(resp)
		return
	}
	resp["latest_version"] = it.Version
	resp["latest_date"] = it.Date
	resp["latest_size"] = it.Size
	resp["can_upgrade"] = compareVersion(it.Version, cur) > 0
	json.NewEncoder(w).Encode(resp)
}

// 一键升级核心：下载到 /data/core/frpc，校验后替换并重启
func handleCoreUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := loadConfig()
	doc, err := fetchManifest(cfg.AuthOIDCIssuer)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "获取版本清单失败：" + err.Error()})
		return
	}
	it := findCoreEntry(doc, runtime.GOARCH)
	if it == nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "清单中没有适用于当前架构的核心"})
		return
	}
	if it.SHA256 == "" {
		json.NewEncoder(w).Encode(map[string]string{"error": "清单条目缺少 SHA-256 校验值，已拒绝安装"})
		return
	}
	cur := currentCoreVersion()
	if compareVersion(it.Version, cur) <= 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":             true,
			"version":        cur,
			"message":        "当前已是最新版本",
			"already_latest": true,
		})
		return
	}

	if err := os.MkdirAll(coreDir, 0755); err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "创建核心目录失败：" + err.Error()})
		return
	}
	tmp := filepath.Join(coreDir, "frpc.new")
	sum, err := downloadToFile(it.URL, tmp)
	if err != nil {
		os.Remove(tmp)
		json.NewEncoder(w).Encode(map[string]string{"error": "下载核心失败：" + err.Error()})
		return
	}
	if !strings.EqualFold(sum, it.SHA256) {
		os.Remove(tmp)
		json.NewEncoder(w).Encode(map[string]string{"error": "SHA-256 校验失败，安装包不匹配"})
		return
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		os.Remove(tmp)
		json.NewEncoder(w).Encode(map[string]string{"error": "设置执行权限失败：" + err.Error()})
		return
	}
	// 架构不符时执行会失败，先确认新核心能跑起来再替换
	if v := binaryVersion(tmp); v == "" {
		os.Remove(tmp)
		json.NewEncoder(w).Encode(map[string]string{"error": "新核心无法运行（通常是架构不匹配）"})
		return
	}
	if err := os.Rename(tmp, coreBinary); err != nil {
		os.Remove(tmp)
		json.NewEncoder(w).Encode(map[string]string{"error": "替换核心失败：" + err.Error()})
		return
	}

	// 运行中才重启，让新核心立刻生效
	restarted := restartFrpc(false)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       true,
		"version":  it.Version,
		"message":  "核心已升级到 v" + it.Version,
		"restarted": restarted,
	})
}

// 恢复内置版本：删掉升级副本，回到镜像自带的基线
func handleCoreRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := os.Stat(coreBinary); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"version": builtinCoreVersion(),
			"message": "当前已使用镜像内置版本",
		})
		return
	}
	if err := os.Remove(coreBinary); err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "恢复内置版本失败：" + err.Error()})
		return
	}
	restarted := restartFrpc(false)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"version":   builtinCoreVersion(),
		"message":   "已恢复镜像内置版本",
		"restarted": restarted,
	})
}
