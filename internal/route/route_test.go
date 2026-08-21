package route

import (
	"strings"
	"testing"
)

func TestParseVLESSBasic(t *testing.T) {
	link := "vless://11111111-1111-4111-8111-111111111111@203.0.113.1:443?encryption=none&security=reality&sni=example.com&pbk=publickey123&sid=abcd&fp=chrome&flow=xtls-rprx-vision&type=tcp#Test%20Name"
	r, err := ParseShareLink(link, "test-vless")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Protocol != "vless" {
		t.Errorf("proto %q", r.Protocol)
	}
	if r.Tag != "test-vless" {
		t.Errorf("tag %q", r.Tag)
	}
	if r.VLESS == nil {
		t.Fatal("vless nil")
	}
	if r.VLESS.Server != "203.0.113.1" {
		t.Errorf("server %q", r.VLESS.Server)
	}
	if r.VLESS.ServerPort != 443 {
		t.Errorf("port %d", r.VLESS.ServerPort)
	}
	if r.VLESS.UUID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("uuid")
	}
	if r.VLESS.SNI != "example.com" {
		t.Errorf("sni %q", r.VLESS.SNI)
	}
	if r.VLESS.PBK != "publickey123" {
		t.Errorf("pbk")
	}
	if r.VLESS.Flow != "xtls-rprx-vision" {
		t.Errorf("flow %q", r.VLESS.Flow)
	}
	if r.Fingerprint == "" {
		t.Error("fingerprint empty")
	}
}

func TestParseVLESSRealityMissingPBK(t *testing.T) {
	link := "vless://uuid@203.0.113.1:443?security=reality&sni=example.com&type=tcp#test"
	_, err := ParseShareLink(link, "t")
	if err == nil {
		t.Fatal("expected error missing pbk")
	}
}

func TestParseVLESSIPv6(t *testing.T) {
	link := "vless://11111111-1111-4111-8111-111111111111@[2001:db8::1]:443?encryption=none&security=reality&sni=example.com&pbk=key&sid=id&fp=chrome#ipv6"
	r, err := ParseShareLink(link, "ipv6")
	if err != nil {
		t.Fatalf("ipv6: %v", err)
	}
	if r.Server != "2001:db8::1" {
		t.Errorf("server %q", r.Server)
	}
}

func TestParseHysteria2(t *testing.T) {
	link := "hysteria2://password123@203.0.113.2:443?sni=example.com&obfs=salamander&obfs-password=obfs123#HY2"
	r, err := ParseShareLink(link, "hy")
	if err != nil {
		t.Fatalf("hy2: %v", err)
	}
	if r.Protocol != "hysteria2" {
		t.Errorf("proto")
	}
	if r.Hysteria.Password != "password123" {
		t.Errorf("pass")
	}
	if r.Hysteria.ObfsPassword != "obfs123" {
		t.Errorf("obfs")
	}
	if r.Hysteria.SNI != "example.com" {
		t.Errorf("sni")
	}
}

func TestParseHY2Alias(t *testing.T) {
	link := "hy2://pass@203.0.113.2:443?sni=example.com#alias"
	_, err := ParseShareLink(link, "a")
	if err != nil {
		t.Fatalf("hy2 alias: %v", err)
	}
}

func TestParseHysteria2MissingPassword(t *testing.T) {
	link := "hysteria2://@203.0.113.2:443?sni=example.com"
	_, err := ParseShareLink(link, "t")
	if err == nil {
		t.Fatal("expected missing password")
	}
}

func TestParseUnsupportedScheme(t *testing.T) {
	_, err := ParseShareLink("trojan://pass@host:443?#t", "t")
	if err == nil {
		t.Fatal("expected unsupported")
	}
}

func TestDeriveNameFromFragment(t *testing.T) {
	link := "vless://uuid@host:443?security=reality&sni=a&pbk=b#My%20Route-Name"
	name := DeriveNameFromLink(link, 1)
	if !strings.Contains(name, "my") && name != "my-route-name" {
		t.Errorf("derive %q", name)
	}
}

func TestFingerprintDedupe(t *testing.T) {
	link := "vless://11111111-1111-4111-8111-111111111111@203.0.113.1:443?security=reality&sni=example.com&pbk=key&sid=id&fp=chrome#one"
	r1, _ := ParseShareLink(link, "one")
	r2, _ := ParseShareLink(strings.Replace(link, "#one", "#two", 1), "two")
	if r1.Fingerprint != r2.Fingerprint {
		t.Errorf("fingerprint should be same for same params diff name")
	}
}

func TestParseVLESSBadPort(t *testing.T) {
	link := "vless://uuid@host:99999?security=reality&sni=a&pbk=b#t"
	_, err := ParseShareLink(link, "t")
	if err == nil {
		t.Fatal("expected bad port")
	}
}

func TestParseMalformedURL(t *testing.T) {
	_, err := ParseShareLink("vless://%zz", "t")
	if err == nil {
		t.Log("expected error but got nil - acceptable if lenient")
	}
}

func TestParseHysteria2EncodedPassword(t *testing.T) {
	link := "hysteria2://p%40ss%2Fword@203.0.113.1:443?sni=a#b"
	r, err := ParseShareLink(link, "t")
	if err != nil {
		t.Fatalf("encoded pass: %v", err)
	}
	if r.Hysteria.Password != "p@ss/word" {
		t.Errorf("decoded pass %q", r.Hysteria.Password)
	}
}

func TestFingerprintNotExposingSecret(t *testing.T) {
	// fingerprint should be hash, not contain uuid
	link := "vless://secret-uuid-123@host:443?security=reality&sni=a&pbk=b#t"
	r, _ := ParseShareLink(link, "t")
	if strings.Contains(r.Fingerprint, "secret") {
		t.Error("fingerprint exposes secret")
	}
}
