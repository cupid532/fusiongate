package fusiongate

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseProxyShareLinks(t *testing.T) {
	vmessPayload, _ := json.Marshal(map[string]any{
		"v": "2", "ps": "vmess", "add": "vmess.example.com", "port": "443",
		"id": "bf000d23-0752-40b4-affe-68f7707a9661", "aid": "0", "scy": "auto",
		"net": "ws", "host": "cdn.example.com", "path": "/edge", "tls": "tls", "sni": "vmess.example.com",
	})
	tests := []struct {
		name, link, protocol, server string
	}{
		{"socks5", "socks5://user:pass@proxy.example.com:1080", "socks5", "proxy.example.com:1080"},
		{"http", "http://user:pass@proxy.example.com:8080", "http", "proxy.example.com:8080"},
		{"https", "https://user:pass@proxy.example.com:8443?sni=proxy.example.com", "https", "proxy.example.com:8443"},
		{"shadowsocks", "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) + "@ss.example.com:8388#SS", "shadowsocks", "ss.example.com:8388"},
		{"trojan", "trojan://secret@trojan.example.com:443?sni=trojan.example.com&type=ws&path=%2Fws", "trojan", "trojan.example.com:443"},
		{"vless reality", "vless://bf000d23-0752-40b4-affe-68f7707a9661@reality.example.com:443?security=reality&sni=www.microsoft.com&fp=chrome&pbk=jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0&sid=0123456789abcdef&flow=xtls-rprx-vision&type=tcp", "vless-reality", "reality.example.com:443"},
		{"vmess", "vmess://" + base64.RawStdEncoding.EncodeToString(vmessPayload), "vmess", "vmess.example.com:443"},
		{"hysteria2", "hysteria2://secret@hy2.example.com:443?sni=hy2.example.com&insecure=1&obfs=salamander&obfs-password=obfs-secret", "hysteria2", "hy2.example.com:443"},
		{"hysteria", "hysteria://hy.example.com:443?auth=secret&peer=hy.example.com&upmbps=20&downmbps=100", "hysteria", "hy.example.com:443"},
		{"tuic", "tuic://bf000d23-0752-40b4-affe-68f7707a9661:secret@tuic.example.com:443?sni=tuic.example.com&congestion_control=bbr", "tuic", "tuic.example.com:443"},
		{"anytls", "anytls://secret@anytls.example.com:443?sni=anytls.example.com&insecure=1", "anytls", "anytls.example.com:443"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outbound, protocol, server, err := parseProxyShareLink(test.link, "node-test")
			if err != nil {
				t.Fatalf("parse link: %v", err)
			}
			if protocol != test.protocol || server != test.server {
				t.Fatalf("got protocol=%q server=%q", protocol, server)
			}
			if outbound["tag"] != "node-test" {
				t.Fatalf("unexpected outbound tag: %#v", outbound["tag"])
			}
		})
	}
}

func TestParseProxyShareLinkRejectsUnsafeOutboundJSON(t *testing.T) {
	for _, raw := range []string{
		`{"type":"direct","server":"example.com"}`,
		`{"type":"block","server":"example.com"}`,
		`{"type":"selector","server":"example.com"}`,
	} {
		if _, _, _, err := parseProxyShareLink(raw, "node-test"); err == nil {
			t.Fatalf("expected outbound to be rejected: %s", raw)
		}
	}
}

func TestBuildSingBoxConfig(t *testing.T) {
	nodes := []storedIPPoolNode{
		{IPPoolNode: IPPoolNode{ID: 1, Name: "reality", LocalPort: 22000}, ShareLink: "vless://bf000d23-0752-40b4-affe-68f7707a9661@reality.example.com:443?security=reality&sni=www.microsoft.com&fp=chrome&pbk=jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0&sid=0123456789abcdef&flow=xtls-rprx-vision&type=tcp"},
		{IPPoolNode: IPPoolNode{ID: 2, Name: "anytls", LocalPort: 22001}, ShareLink: "anytls://secret@anytls.example.com:443?sni=anytls.example.com&insecure=1"},
	}
	config, valid, failures, err := buildSingBoxConfig(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 2 || len(failures) != 0 {
		t.Fatalf("valid=%d failures=%v", len(valid), failures)
	}
	var decoded struct {
		Inbounds  []map[string]any `json:"inbounds"`
		Outbounds []map[string]any `json:"outbounds"`
		Route     struct {
			Rules []map[string]any `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(config, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Inbounds) != 2 || len(decoded.Outbounds) != 2 || len(decoded.Route.Rules) != 2 {
		t.Fatalf("unexpected generated config: %s", config)
	}
}

func TestGeneratedSingBoxConfigsAreAccepted(t *testing.T) {
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box is not installed")
	}
	links := []string{
		"socks5://user:pass@proxy.example.com:1080",
		"http://user:pass@proxy.example.com:8080",
		"https://user:pass@proxy.example.com:8443?sni=proxy.example.com",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) + "@ss.example.com:8388",
		"trojan://secret@trojan.example.com:443?sni=trojan.example.com&type=ws&path=%2Fws",
		"vless://bf000d23-0752-40b4-affe-68f7707a9661@reality.example.com:443?security=reality&sni=www.microsoft.com&fp=chrome&pbk=jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0&sid=0123456789abcdef&flow=xtls-rprx-vision&type=tcp",
		"hysteria2://secret@hy2.example.com:443?sni=hy2.example.com&insecure=1&obfs=salamander&obfs-password=obfs-secret",
		"hysteria://hy.example.com:443?auth=secret&peer=hy.example.com&upmbps=20&downmbps=100",
		"tuic://bf000d23-0752-40b4-affe-68f7707a9661:secret@tuic.example.com:443?sni=tuic.example.com&congestion_control=bbr",
		"anytls://secret@anytls.example.com:443?sni=anytls.example.com&insecure=1",
	}
	for index, link := range links {
		node := storedIPPoolNode{IPPoolNode: IPPoolNode{ID: int64(index + 1), LocalPort: ipPoolPortStart + index}, ShareLink: link}
		config, valid, failures, err := buildSingBoxConfig([]storedIPPoolNode{node})
		if err != nil || len(valid) != 1 || len(failures) != 0 {
			t.Fatalf("build config for %s: valid=%d failures=%v err=%v", link[:strings.Index(link, "://")], len(valid), failures, err)
		}
		path := t.TempDir() + "/config.json"
		if err := os.WriteFile(path, config, 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "check", "-c", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("sing-box rejected %s config: %v\n%s\n%s", link[:strings.Index(link, "://")], err, output, config)
		}
	}
}
