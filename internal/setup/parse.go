package setup

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type VLESSOutbound struct {
	Tag            string
	Server         string
	ServerPort     int
	UUID           string
	Flow           string
	SNI            string
	FP             string
	PBK            string
	SID            string
	SpiderX        string
	PacketEncoding string
}

type Hysteria2Outbound struct {
	Tag          string
	Server       string
	ServerPort   int
	Password     string
	ObfsType     string
	ObfsPassword string
	SNI          string
	Insecure     bool
	PinSHA256    string
}

func ParseVLESSShareLink(link, tag string) (*VLESSOutbound, error) {
	if tag == "" {
		return nil, fmt.Errorf("tag required")
	}
	link = strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "vless") {
		return nil, fmt.Errorf("not a vless:// URL")
	}
	uuid := u.User.Username()
	if uuid == "" {
		return nil, fmt.Errorf("missing UUID")
	}
	if _, err := url.QueryUnescape(uuid); err == nil {
		if decoded, e2 := url.QueryUnescape(uuid); e2 == nil {
			_ = decoded
		}
	}
	host := u.Hostname()
	portStr := u.Port()
	if host == "" || portStr == "" {
		return nil, fmt.Errorf("missing server or port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	q := u.Query()
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("serverName")
	}
	if sni == "" {
		sni = q.Get("peer")
	}
	pbk := q.Get("pbk")
	sid := q.Get("sid")
	if sid == "" {
		sid = q.Get("shortId")
	}
	fp := q.Get("fp")
	if fp == "" {
		fp = q.Get("fingerprint")
	}
	if fp == "" {
		fp = "chrome"
	}
	flow := q.Get("flow")
	spx := q.Get("spx")
	if spx == "" {
		spx = q.Get("spiderX")
	}
	security := q.Get("security")
	if security == "" {
		security = q.Get("type")
		// type may be tcp etc, not security, so fallback to empty; we already validated transport separately
		if security == "tcp" || security == "raw" {
			security = ""
		}
	}
	transport := q.Get("type")
	if transport != "" && transport != "tcp" && transport != "raw" {
		return nil, fmt.Errorf("unsupported VLESS transport %q", transport)
	}
	if flow != "" && flow != "xtls-rprx-vision" {
		return nil, fmt.Errorf("unsupported VLESS flow %q", flow)
	}
	if pbk == "" {
		return nil, fmt.Errorf("missing pbk (REALITY public key)")
	}
	if sni == "" {
		return nil, fmt.Errorf("missing sni/serverName for reality")
	}
	if security != "" && !strings.EqualFold(security, "reality") {
		return nil, fmt.Errorf("non-REALITY VLESS is not currently supported: security=%q", security)
	}
	// Fragment may contain name but not needed
	return &VLESSOutbound{
		Tag:        tag,
		Server:     host,
		ServerPort: port,
		UUID:       uuid,
		Flow:       flow,
		SNI:        sni,
		FP:         fp,
		PBK:        pbk,
		SID:        sid,
		SpiderX:    spx,
	}, nil
}

func ParseHysteria2ShareLink(link, tag string) (*Hysteria2Outbound, error) {
	if tag == "" {
		return nil, fmt.Errorf("tag required")
	}
	link = strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "hysteria2" && scheme != "hy2" {
		return nil, fmt.Errorf("not a hysteria2:// URL (got %s)", u.Scheme)
	}
	if u.User == nil {
		return nil, fmt.Errorf("missing password")
	}
	pass, hasPass := u.User.Password()
	username := u.User.Username()
	if hasPass {
		// If both username and password present, treat password as auth (username ignored)
		// username may be empty, password holds auth
		if pass == "" {
			pass = username
		}
	} else {
		pass = username
	}
	if pass == "" {
		return nil, fmt.Errorf("missing password")
	}
	if decoded, err := url.QueryUnescape(pass); err == nil {
		pass = decoded
	}
	host := u.Hostname()
	portStr := u.Port()
	if host == "" || portStr == "" {
		return nil, fmt.Errorf("missing server or port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	q := u.Query()
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	if sni == "" {
		sni = q.Get("serverName")
	}
	obfs := q.Get("obfs")
	obfsPass := q.Get("obfs-password")
	if obfsPass == "" {
		obfsPass = q.Get("obfsPassword")
	}
	if obfs == "salamander" && obfsPass == "" {
		return nil, fmt.Errorf("salamander obfs requires obfs-password")
	}
	if decoded, err := url.QueryUnescape(obfsPass); err == nil {
		obfsPass = decoded
	}
	insecure := false
	if v := q.Get("insecure"); v == "1" || strings.EqualFold(v, "true") {
		insecure = true
	}
	pin := q.Get("pinSHA256")
	if pin != "" {
		return nil, fmt.Errorf("unsupported Hysteria2 pinSHA256: certificate pinning is not currently supported")
	}
	if q.Get("pinSHA256") != "" || q.Get("pin") != "" {
		return nil, fmt.Errorf("unsupported Hysteria2 pinSHA256")
	}
	return &Hysteria2Outbound{
		Tag:          tag,
		Server:       host,
		ServerPort:   port,
		Password:     pass,
		ObfsPassword: obfsPass,
		SNI:          sni,
		Insecure:     insecure,
		PinSHA256:    pin,
	}, nil
}

func RedactURL(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return "[redacted]"
	}
	if u.User != nil {
		u.User = url.UserPassword("REDACTED", "REDACTED")
	}
	// redact sensitive query params
	q := u.Query()
	for _, k := range []string{"pbk", "sid", "password", "obfs-password", "obfsPassword"} {
		if q.Get(k) != "" {
			q.Set(k, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
