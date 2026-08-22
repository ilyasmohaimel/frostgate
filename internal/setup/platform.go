package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const singBoxVersion = "1.13.19"

type SingBoxAsset struct {
	Name   string
	SHA256 string
}

func SingBoxAssetFor(goos, goarch string) (SingBoxAsset, error) {
	if goos != "linux" {
		return SingBoxAsset{}, fmt.Errorf("automatic sing-box install is unsupported on %s/%s", goos, goarch)
	}
	switch goarch {
	case "amd64":
		return SingBoxAsset{
			Name:   "sing-box-1.13.19-linux-amd64.tar.gz",
			SHA256: "ef88a9e577d474210867bd708933d042e9b70106529df2656182c9db90106aa1",
		}, nil
	case "arm64":
		return SingBoxAsset{
			Name:   "sing-box-1.13.19-linux-arm64.tar.gz",
			SHA256: "7fe3597a95a3c5ad67477b1d7653b9ce097e0be7c676758eba1fcf558f353d57",
		}, nil
	default:
		return SingBoxAsset{}, fmt.Errorf("automatic sing-box install is unsupported on %s/%s", goos, goarch)
	}
}

func SingBoxReleaseURL(goos, goarch string) (string, error) {
	asset, err := SingBoxAssetFor(goos, goarch)
	if err != nil {
		return "", err
	}
	return "https://github.com/SagerNet/sing-box/releases/download/v" + singBoxVersion + "/" + asset.Name, nil
}

func VerifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", path, actual, expected)
	}
	return nil
}
