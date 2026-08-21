package setup

import (
	"strings"
	"testing"
)

func TestParseVLESSValid(t *testing.T) {
	link := "vless://11111111-1111-1111-1111-111111111111@203.0.113.10:443?encryption=none&security=reality&sni=example.com&fp=chrome&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=abcd1234ef561234&flow=xtls-rprx-vision"
	v, err := ParseVLESSShareLink(link, "test-reality")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.UUID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("uuid %q", v.UUID)
	}
	if v.Server != "203.0.113.10" {
		t.Errorf("server %q", v.Server)
	}
	if v.SNI != "example.com" {
		t.Errorf("sni %q", v.SNI)
	}
	if v.PBK != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("pbk %q", v.PBK)
	}
}

func TestParseVLESSMissingPBK(t *testing.T) {
	link := "vless://uuid@1.1.1.1:443?security=reality&sni=example.com&fp=chrome&sid=abc"
	_, err := ParseVLESSShareLink(link, "tag")
	if err == nil || !strings.Contains(err.Error(), "pbk") {
		t.Fatalf("expected pbk error, got %v", err)
	}
}

func TestParseVLESSMissingSNI(t *testing.T) {
	link := "vless://uuid@1.1.1.1:443?pbk=abc&sid=abc"
	_, err := ParseVLESSShareLink(link, "tag")
	if err == nil || !strings.Contains(err.Error(), "sni") {
		t.Fatalf("expected sni error, got %v", err)
	}
}

func TestParseVLESSInvalidScheme(t *testing.T) {
	_, err := ParseVLESSShareLink("hysteria2://pass@1.1.1.1:443?insecure=1", "tag")
	if err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestParseVLESSMissingTag(t *testing.T) {
	_, err := ParseVLESSShareLink("vless://uuid@1.1.1.1:443?pbk=x&sni=y", "")
	if err == nil {
		t.Fatal("expected tag error")
	}
}

func TestParseHysteria2Valid(t *testing.T) {
	link := "hysteria2://test-password-123@203.0.113.10:443?obfs=salamander&obfs-password=test-obfs-456&sni=example.com&insecure=1"
	h, err := ParseHysteria2ShareLink(link, "hyst")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h.Server != "203.0.113.10" {
		t.Errorf("server %q", h.Server)
	}
	if h.Password != "test-password-123" {
		t.Errorf("password %q", h.Password)
	}
	if h.ObfsPassword != "test-obfs-456" {
		t.Errorf("obfs %q", h.ObfsPassword)
	}
	if !h.Insecure {
		t.Error("insecure should be true")
	}
}

func TestParseHysteria2Hy2Scheme(t *testing.T) {
	link := "hy2://pass@1.1.1.1:443?sni=example.com"
	h, err := ParseHysteria2ShareLink(link, "tag")
	if err != nil {
		t.Fatalf("hy2 parse: %v", err)
	}
	if h.Tag != "tag" {
		t.Errorf("tag %q", h.Tag)
	}
}

func TestParseHysteria2MissingPassword(t *testing.T) {
	_, err := ParseHysteria2ShareLink("hysteria2://@1.1.1.1:443?sni=x", "tag")
	if err == nil {
		t.Fatal("expected password error")
	}
}

func TestParseHysteria2SalamanderMissingObfsPass(t *testing.T) {
	_, err := ParseHysteria2ShareLink("hysteria2://pass@1.1.1.1:443?obfs=salamander&sni=x", "tag")
	if err == nil {
		t.Fatal("expected obfs-password error")
	}
}

func TestRedactURL(t *testing.T) {
	link := "vless://uuid@1.1.1.1:443?pbk=secret123&sid=secret456&sni=example.com"
	redacted := RedactURL(link)
	if strings.Contains(redacted, "secret123") || strings.Contains(redacted, "secret456") {
		t.Errorf("redact failed: %q", redacted)
	}
	if !strings.Contains(redacted, "REDACTED") {
		t.Errorf("should contain REDACTED")
	}
	// Ensure hysteria redacts password
	hlink := "hysteria2://mypassword@1.1.1.1:443?obfs-password=secret&sni=x"
	redactedH := RedactURL(hlink)
	if strings.Contains(redactedH, "mypassword") || strings.Contains(redactedH, "secret") {
		t.Errorf("hysteria redact failed: %q", redactedH)
	}
}

func TestRedactDoesNotLeakFullURL(t *testing.T) {
	link := "vless://test-uuid@203.0.113.1:443?pbk=key&sid=id&sni=example.com#fragment"
	redacted := RedactURL(link)
	if strings.Contains(redacted, "test-uuid") {
		// user info should be redacted
		t.Errorf("user should be redacted: %q", redacted)
	}
}
