package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSingBoxAssetFor(t *testing.T) {
	tests := []struct {
		goos, goarch, name, sum string
	}{
		{"linux", "amd64", "sing-box-1.13.19-linux-amd64.tar.gz", "ef88a9e577d474210867bd708933d042e9b70106529df2656182c9db90106aa1"},
		{"linux", "arm64", "sing-box-1.13.19-linux-arm64.tar.gz", "7fe3597a95a3c5ad67477b1d7653b9ce097e0be7c676758eba1fcf558f353d57"},
	}
	for _, tt := range tests {
		asset, err := SingBoxAssetFor(tt.goos, tt.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", tt.goos, tt.goarch, err)
		}
		if asset.Name != tt.name || asset.SHA256 != tt.sum {
			t.Errorf("%s/%s: got %+v", tt.goos, tt.goarch, asset)
		}
	}
	for _, tt := range [][2]string{{"darwin", "arm64"}, {"linux", "386"}} {
		if _, err := SingBoxAssetFor(tt[0], tt[1]); err == nil {
			t.Errorf("%s/%s accepted", tt[0], tt[1])
		}
	}
}

func TestSingBoxReleaseURL(t *testing.T) {
	got, err := SingBoxReleaseURL("linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/SagerNet/sing-box/releases/download/v1.13.19/sing-box-1.13.19-linux-arm64.tar.gz"
	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestVerifySHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte("frostgate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySHA256(path, "b2e6e5e7e1e6bdfe4867f8fc9f6d7cfda93b6bb36e12f4a2fd3e9f8df7efb7ba"); err == nil {
		t.Fatal("accepted incorrect digest")
	}
	if err := VerifySHA256(path, "ab1e7c5182305270745f94f0e955d29527b1afb5c1fd47823f7ac5b5fe978be5"); err != nil {
		t.Fatalf("valid digest rejected: %v", err)
	}
}
