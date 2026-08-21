package setup

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type VLESSOutbound struct {
	Tag        string
	Server     string
	ServerPort int
	UUID       string
	Flow       string
	SNI        string
	FP         string
	PBK        string
	SID        string
	SpiderX    string
}

type Hysteria2Outbound struct {
	Tag          string
	Server       string
	ServerPort   int
	Password     string
	ObfsPassword string
	SNI          string
	Insecure     bool
	PinSHA256    string
}

func ParseVLESSShareLink(link, tag string) (*VLESSOutbound, error) {
	if tag == "" {
		return nil, fmt.Errorf("tag required")
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "vless" {
		return nil, fmt.Errorf("not a vless:// URL")
	}
	uuid := u.User.Username()
	if uuid == "" {
		return nil, fmt.Errorf("missing UUID")
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
	q := u.Query()
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("serverName")
	}
	pbk := q.Get("pbk")
	sid := q.Get("sid")
	fp := q.Get("fp")
	if fp == "" {
		fp = "chrome"
	}
	flow := q.Get("flow")
	spx := q.Get("spx")
	if pbk == "" {
		return nil, fmt.Errorf("missing pbk (REALITY public key)")
	}
	if sid == "" {
		// sid may be empty for some configs, allow but warn
	}
	if sni == "" {
		return nil, fmt.Errorf("missing sni")
	}
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
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "hysteria2" && scheme != "hy2" {
		return nil, fmt.Errorf("not a hysteria2:// URL (got %s)", u.Scheme)
	}
	// password is userinfo
	if u.User == nil {
		return nil, fmt.Errorf("missing password")
	}
	// hysteria may have user:pass but we treat username as password, or password field if user:pass
	pass, _ := u.User.Password()
	if pass != "" {
		// if username:password, combine? For now use password if present, else username
		// Some clients use userinfo as password directly (no username)
		// If User contains ":", Password() returns second part
		if u.User.Username() != "" && pass == "" {
			pass = u.User.Username()
		} else if u.User.Username() != "" && pass != "" {
			// treat as user:pass combined? but sing-box expects password field as auth
			// Use password part if present, fallback to username:password
			// For compatibility, if both present, use pass (or username + ":" + pass?)
			// We'll use pass if username looks like auth? Simpler: if Username() is not empty and pass != "", use pass
		}
	} else {
		pass = u.User.Username()
	}
	if pass == "" {
		return nil, fmt.Errorf("missing password")
	}
	// decode percent
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
	q := u.Query()
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	obfs := q.Get("obfs")
	obfsPass := q.Get("obfs-password")
	if obfsPass == "" {
		obfsPass = q.Get("obfsPassword")
	}
	if obfs == "salamander" && obfsPass == "" {
		return nil, fmt.Errorf("salamander obfs requires obfs-password")
	}
	insecure := false
	if v := q.Get("insecure"); v == "1" || strings.EqualFold(v, "true") {
		insecure = true
	}
	pin := q.Get("pinSHA256")
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
