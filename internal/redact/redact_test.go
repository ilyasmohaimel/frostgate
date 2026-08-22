package redact

import (
	"errors"
	"strings"
	"testing"
)

const (
	tokenSub  = "SUPER_SECRET_SUB_TOKEN_123"
	tokenUUID = "SUPER_SECRET_UUID_456"
	tokenPass = "SUPER_SECRET_PASSWORD_789"
)

func TestRedactShareLinks(t *testing.T) {
	cases := []string{
		"vless://" + tokenUUID + "@203.0.113.10:443?pbk=PUB&sid=SID&spx=/#Route",
		"hysteria2://" + tokenPass + "@203.0.113.20:8443?obfs=salamander&obfs-password=" + tokenPass + "#H",
		"hy2://u:" + tokenPass + "@203.0.113.30:443#x",
	}
	for _, c := range cases {
		out := URL(c)
		if strings.Contains(out, tokenUUID) || strings.Contains(out, tokenPass) {
			t.Errorf("leaked secret in %q -> %q", c, out)
		}
		if strings.Contains(out, "REDACTED") == false {
			t.Errorf("expected redaction marker in %q -> %q", c, out)
		}
	}
}

func TestRedactHTTPURLError(t *testing.T) {
	errStr := "Get \"https://provider.example/sub?token=" + tokenSub + "\": timeout"
	out := Error(errors.New(errStr))
	if strings.Contains(out, tokenSub) {
		t.Errorf("leaked token in %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("expected [redacted] in %q", out)
	}
}

func TestRedactEmbeddedShareLink(t *testing.T) {
	in := "parse failed for vless://" + tokenUUID + "@203.0.113.10:443?pbk=key#route"
	out := String(in)
	if strings.Contains(out, tokenUUID) || strings.Contains(out, "pbk=key") {
		t.Fatalf("embedded share URL leaked: %q", out)
	}
}

func TestRedactBearer(t *testing.T) {
	inp := "Authorization: Bearer " + tokenUUID
	if out := String(inp); strings.Contains(out, tokenUUID) {
		t.Errorf("leaked bearer token: %q", out)
	}
}

func TestRedactUUID(t *testing.T) {
	realUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	inp := "connection failed for " + realUUID + " at host"
	out := String(inp)
	if strings.Contains(out, realUUID) {
		t.Errorf("leaked uuid: %q", out)
	}
	if !strings.Contains(out, "REDACTED-UUID") {
		t.Errorf("expected REDACTED-UUID in %q", out)
	}
}

func TestRedactQueryAndLong(t *testing.T) {
	inp := "https://x.example/path?secret=" + tokenPass + "&next=1"
	out := URL(inp)
	if strings.Contains(out, tokenPass) {
		t.Errorf("leaked query secret: %q", out)
	}
	long := strings.Repeat("x", 200)
	if out := String(long); strings.Contains(out, "...") == false && len(out) > 120 {
		t.Errorf("long string not truncated: %q", out)
	}
}

func TestRedactEmpty(t *testing.T) {
	if URL("") != "[redacted]" {
		t.Errorf("empty should redact")
	}
	if Error(nil) != "" {
		t.Errorf("nil error should be empty")
	}
}
