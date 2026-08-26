package setup

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ilyasmohaimel/frostgate/internal/redact"
)

type VLESSOutbound struct {
	Tag               string
	Server            string
	ServerPort        int
	UUID              string
	Flow              string
	SNI               string
	FP                string
	PBK               string
	SID               string
	SpiderX           string
	PacketEncoding    string
	PacketEncodingSet bool
	Network           string
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
	Network      string
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
	security := strings.ToLower(q.Get("security"))
	transport := strings.ToLower(q.Get("type"))
	if transport != "" && transport != "tcp" && transport != "raw" {
		return nil, fmt.Errorf("unsupported VLESS transport %q", transport)
	}
	network := strings.ToLower(q.Get("network"))
	switch network {
	case "":
		// Share links predate the native sing-box TCP-and-UDP default. Keep
		// their historical raw TCP meaning when they do not state a network.
		network = "tcp"
	case "tcp":
	case "both":
		// An empty native sing-box network enables TCP and UDP. `both` is an
		// internal importer marker used when a native outbound omitted it.
		network = ""
	default:
		return nil, fmt.Errorf("unsupported VLESS network %q", network)
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
	if security != "reality" {
		return nil, fmt.Errorf("non-REALITY VLESS is not currently supported: security=%q", security)
	}
	// Fragment may contain name but not needed
	packetEncoding, packetEncodingSet := q["packet_encoding"]
	return &VLESSOutbound{
		Tag:               tag,
		Server:            host,
		ServerPort:        port,
		UUID:              uuid,
		Flow:              flow,
		SNI:               sni,
		FP:                fp,
		PBK:               pbk,
		SID:               sid,
		SpiderX:           spx,
		PacketEncoding:    firstValue(packetEncoding),
		PacketEncodingSet: packetEncodingSet,
		Network:           network,
	}, nil
}

func firstValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
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
	if obfs != "" && obfs != "salamander" {
		return nil, fmt.Errorf("unsupported Hysteria2 obfs %q", obfs)
	}
	network := strings.ToLower(q.Get("network"))
	if network != "" && network != "tcp" && network != "udp" {
		return nil, fmt.Errorf("unsupported Hysteria2 network %q", network)
	}
	insecure := false
	if v := q.Get("insecure"); v == "1" || strings.EqualFold(v, "true") {
		insecure = true
	}
	pin := q.Get("pinSHA256")
	if pin == "" {
		pin = q.Get("certificate_public_key_sha256")
	}
	if pin != "" {
		return nil, fmt.Errorf("unsupported Hysteria2 pinSHA256: certificate pinning is not currently supported")
	}
	if q.Get("pin") != "" || q.Get("certificate_fingerprint") != "" || q.Get("fingerprint") != "" {
		return nil, fmt.Errorf("unsupported Hysteria2 pinSHA256")
	}
	return &Hysteria2Outbound{
		Tag:          tag,
		Server:       host,
		ServerPort:   port,
		Password:     pass,
		ObfsType:     obfs,
		ObfsPassword: obfsPass,
		SNI:          sni,
		Insecure:     insecure,
		PinSHA256:    pin,
		Network:      network,
	}, nil
}

func RedactURL(link string) string {
	return redact.String(link)
}
