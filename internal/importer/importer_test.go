package importer

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

func TestParseLinkList(t *testing.T) {
	data := []byte("vless://uuid@host:443?security=reality&sni=a&pbk=b#one\nhysteria2://pass@host2:443?sni=a#two\ntrojan://pass@host:443#skipped\n")
	res := ParseSubscription(data)
	if len(res.Links) != 2 {
		t.Fatalf("links %d", len(res.Links))
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped %d", len(res.Skipped))
	}
	if !strings.Contains(res.Skipped[0].Reason, "unsupported") {
		t.Error("reason")
	}
}

func TestBase64Subscription(t *testing.T) {
	plain := "vless://uuid@host:443?security=reality&sni=a&pbk=b#one\nhysteria2://pass@host2:443?sni=a#two\n"
	b64 := base64.StdEncoding.EncodeToString([]byte(plain))
	res := ParseSubscription([]byte(b64))
	if len(res.Links) != 2 {
		t.Fatalf("b64 links %d", len(res.Links))
	}
}

func TestClashYAML(t *testing.T) {
	yaml := `
proxies:
  - name: VLESS Test
    type: vless
    server: 203.0.113.1
    port: 443
    uuid: 11111111-1111-4111-8111-111111111111
    network: tcp
    flow: xtls-rprx-vision
    servername: example.com
    client-fingerprint: chrome
    reality-opts:
      public-key: testpbk
      short-id: abcd
  - name: HY2 Test
    type: hysteria2
    server: 203.0.113.2
    port: 443
    password: pass123
    sni: example.com
    obfs: salamander
    obfs-password: obfspass
  - name: Trojan Skip
    type: trojan
    server: 1.2.3.4
    port: 443
    password: x
`
	res := ParseSubscription([]byte(yaml))
	if len(res.Links) != 2 {
		t.Fatalf("clash links %d want 2, skipped %v", len(res.Links), res.Skipped)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("clash skipped %d", len(res.Skipped))
	}
}

func TestSingBoxJSON(t *testing.T) {
	jsonData := `{
  "outbounds": [
    {"type": "vless", "tag": "v1", "server": "203.0.113.1", "server_port": 443, "uuid": "uuid1", "flow": "xtls-rprx-vision", "tls": {"enabled": true, "server_name": "example.com", "utls": {"enabled": true, "fingerprint": "chrome"}, "reality": {"enabled": true, "public_key": "pbk1", "short_id": "sid"}}},
    {"type": "hysteria2", "tag": "h1", "server": "203.0.113.2", "server_port": 443, "password": "pass1", "tls": {"enabled": true, "server_name": "example.com"}},
    {"type": "direct", "tag": "direct"},
    {"type": "trojan", "tag": "trojan1", "server": "1.1.1.1"}
  ]
}`
	res := ParseSubscription([]byte(jsonData))
	if len(res.Links) != 2 {
		t.Fatalf("singbox links %d", len(res.Links))
	}
	foundTrojan := false
	for _, s := range res.Skipped {
		if strings.Contains(s.Reason, "trojan") {
			foundTrojan = true
		}
	}
	if !foundTrojan {
		t.Error("expected trojan skipped")
	}
}

func TestImportRejectsUnsupportedVLESSTransports(t *testing.T) {
	for _, data := range [][]byte{[]byte(`proxies:
  - name: bad
    type: vless
    server: 203.0.113.10
    port: 443
    uuid: 11111111-1111-4111-8111-111111111111
    network: ws
    servername: example.com
    reality-opts: {public-key: fixture-key}
	`), []byte(`{"outbounds":[{"type":"vless","server":"203.0.113.10","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","transport":{"type":"grpc"},"tls":{"enabled":true,"server_name":"example.com","reality":{"enabled":true,"public_key":"fixture-key"}}}]}`)} {
		result := ParseSubscription(data)
		if len(result.Links) != 0 || len(result.Skipped) != 1 {
			t.Fatalf("unsupported transport was rewritten: %+v", result)
		}
	}
}

func TestImportedFieldFidelityAndIPv6(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		check func(t *testing.T, link string)
	}{
		{
			name: "direct vless share",
			data: "vless://11111111-1111-4111-8111-111111111111@[2001:db8::3]:443?encryption=none&security=reality&sni=example.com&pbk=fixture-key&sid=abcd&fp=firefox&flow=xtls-rprx-vision&spx=%2Fedge&packet_encoding=packetaddr&type=raw#direct",
			check: func(t *testing.T, link string) {
				v, err := setup.ParseVLESSShareLink(link, "route")
				if err != nil || v.Server != "2001:db8::3" || v.Flow != "xtls-rprx-vision" || v.SpiderX != "/edge" || v.PacketEncoding != "packetaddr" || v.FP != "firefox" || v.PBK != "fixture-key" || v.SID != "abcd" {
					t.Fatalf("direct VLESS fidelity failed: %+v err=%v link=%s", v, err, link)
				}
			},
		},
		{
			name: "clash vless",
			data: `proxies:
  - name: v6-reality
    type: vless
    server: "2001:db8::1"
    port: 443
    uuid: 11111111-1111-4111-8111-111111111111
    network: tcp
    flow: xtls-rprx-vision
    servername: example.com
    client-fingerprint: firefox
    packet-encoding: packetaddr
    spider-x: /edge
    reality-opts: {public-key: fixture-key, short-id: abcd}
`,
			check: func(t *testing.T, link string) {
				v, err := setup.ParseVLESSShareLink(link, "route")
				if err != nil || v.Server != "2001:db8::1" || v.Flow != "xtls-rprx-vision" || v.SpiderX != "/edge" || v.PacketEncoding != "packetaddr" || v.FP != "firefox" || v.PBK != "fixture-key" || v.SID != "abcd" {
					t.Fatalf("vless fidelity failed: %+v err=%v link=%s", v, err, link)
				}
			},
		},
		{
			name: "clash hysteria",
			data: `proxies:
  - name: h
    type: hysteria2
    server: "2001:db8::4"
    port: 443
    password: "p%25+ text"
    sni: example.com
    obfs: salamander
    obfs-password: "obfs%2B+ text"
    skip-cert-verify: true
`,
			check: func(t *testing.T, link string) {
				h, err := setup.ParseHysteria2ShareLink(link, "route")
				if err != nil || h.Server != "2001:db8::4" || h.Password != "p%25+ text" || h.ObfsType != "salamander" || h.ObfsPassword != "obfs%2B+ text" || !h.Insecure {
					t.Fatalf("clash Hysteria2 fidelity failed: %+v err=%v link=%s", h, err, link)
				}
			},
		},
		{
			name: "singbox vless",
			data: `{"outbounds":[{"type":"vless","tag":"v","server":"2001:db8::5","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","flow":"xtls-rprx-vision","packet_encoding":"packetaddr","tls":{"enabled":true,"server_name":"example.com","utls":{"enabled":true,"fingerprint":"firefox"},"reality":{"enabled":true,"public_key":"fixture-key","short_id":"abcd","spider_x":"/edge"}}}]}`,
			check: func(t *testing.T, link string) {
				v, err := setup.ParseVLESSShareLink(link, "route")
				if err != nil || v.Server != "2001:db8::5" || v.Flow != "xtls-rprx-vision" || v.SpiderX != "/edge" || v.PacketEncoding != "packetaddr" || v.FP != "firefox" || v.PBK != "fixture-key" || v.SID != "abcd" {
					t.Fatalf("sing-box VLESS fidelity failed: %+v err=%v link=%s", v, err, link)
				}
			},
		},
		{
			name: "singbox hysteria",
			data: `{"outbounds":[{"type":"hysteria2","tag":"h","server":"2001:db8::2","server_port":443,"password":"p%25+ text","obfs":{"type":"salamander","password":"obfs%2B+ text"},"tls":{"enabled":true,"server_name":"example.com","insecure":true}}]}`,
			check: func(t *testing.T, link string) {
				h, err := setup.ParseHysteria2ShareLink(link, "route")
				if err != nil || h.Server != "2001:db8::2" || h.Password != "p%25+ text" || h.ObfsType != "salamander" || h.ObfsPassword != "obfs%2B+ text" || !h.Insecure {
					t.Fatalf("hysteria fidelity failed: %+v err=%v link=%s", h, err, link)
				}
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseSubscription([]byte(tt.data))
			if len(result.Links) != 1 {
				t.Fatalf("links=%d skipped=%+v", len(result.Links), result.Skipped)
			}
			tt.check(t, result.Links[0])
		})
	}
}

func TestFetchSubscriptionTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vless://uuid@host:443?security=reality&sni=a&pbk=b#t"))
	}))
	defer srv.Close()
	data, err := FetchSubscriptionWithOpts(nilContext(), srv.URL, true)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(data) == 0 {
		t.Error("empty")
	}
}

func TestFetchRejectFile(t *testing.T) {
	_, err := FetchSubscription(nilContext(), "file:///etc/passwd")
	if err == nil {
		t.Fatal("expected reject file")
	}
}

func TestFetchRejectData(t *testing.T) {
	_, err := FetchSubscription(nilContext(), "data:text/plain,hello")
	if err == nil {
		t.Fatal("expected reject data")
	}
}

func TestFetchSizeLimit(t *testing.T) {
	large := strings.Repeat("a", 2*1024*1024+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(large))
	}))
	defer srv.Close()
	_, err := FetchSubscriptionWithOpts(nilContext(), srv.URL, true)
	if err == nil {
		t.Fatal("expected size limit")
	}
}

func TestFetchRedirectLimit(t *testing.T) {
	redirects := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects++
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}))
	defer srv.Close()
	_, err := FetchSubscriptionWithOpts(nilContext(), srv.URL, true)
	if err == nil {
		t.Fatal("expected redirect limit")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err %v", err)
	}
}

func TestFetchRejectHTTPWithoutFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vless://uuid@host:443?security=reality&sni=a&pbk=b#t"))
	}))
	defer srv.Close()
	_, err := FetchSubscription(nilContext(), srv.URL)
	if err == nil {
		t.Fatal("expected http without flag to be rejected")
	}
	if !strings.Contains(err.Error(), "allow-insecure") {
		t.Errorf("err %v", err)
	}
}

func TestFetchAllowHTTPWithFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vless://uuid@host:443?security=reality&sni=a&pbk=b#t"))
	}))
	defer srv.Close()
	_, err := FetchSubscriptionWithOpts(nilContext(), srv.URL, true)
	if err != nil {
		t.Fatalf("allow insecure should succeed: %v", err)
	}
}

func TestFetchDowngradeBlocked(t *testing.T) {
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("must not be fetched"))
	}))
	defer httpTarget.Close()
	srvHTTPS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpTarget.URL, http.StatusFound)
	}))
	defer srvHTTPS.Close()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = srvHTTPS.Client().Transport
	defer func() { http.DefaultTransport = originalTransport }()
	_, err := FetchSubscriptionWithOpts(nilContext(), srvHTTPS.URL, false)
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("HTTPS-to-HTTP downgrade must be blocked, got %v", err)
	}
}

func TestEmptySubscription(t *testing.T) {
	res := ParseSubscription([]byte(""))
	if len(res.Links) != 0 {
		t.Error("expected 0")
	}
}

func TestMixedSupportedUnsupported(t *testing.T) {
	data := []byte("vless://uuid@host:443?security=reality&sni=a&pbk=b#good\ntrojan://pass@host:443#bad\nss://pass@host:443#bad2\nnotalink\n")
	res := ParseSubscription(data)
	if len(res.Links) != 1 {
		t.Fatalf("links %d", len(res.Links))
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("skipped %d", len(res.Skipped))
	}
}

func nilContext() context.Context {
	// helper to get context.Background without import
	return context.Background()
}

func TestUserAgentIsVersionNeutral(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("vless://uuid@host:443?security=reality&sni=a&pbk=b#t"))
	}))
	defer srv.Close()
	if _, err := FetchSubscriptionWithOpts(context.Background(), srv.URL, true); err != nil {
		t.Fatal(err)
	}
	if got != "frostgate" || strings.Contains(got, "0.4") {
		t.Fatalf("actual request UA must be version-neutral, got %q", got)
	}
}
