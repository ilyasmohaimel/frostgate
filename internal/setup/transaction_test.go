package setup

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilyasmohaimel/frostgate/internal/config"
)

type transactionSystemd struct {
	calls          []string
	runErrors      map[string][]error
	output         map[string][]byte
	outputSequence map[string][][]byte
}

func (f *transactionSystemd) Run(_ string, args ...string) error {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if errorsForCall := f.runErrors[key]; len(errorsForCall) > 0 {
		err := errorsForCall[0]
		f.runErrors[key] = errorsForCall[1:]
		return err
	}
	return nil
}

func (f *transactionSystemd) Output(_ string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if sequence := f.outputSequence[key]; len(sequence) > 0 {
		out := sequence[0]
		f.outputSequence[key] = sequence[1:]
		return out, nil
	}
	return f.output[key], nil
}

func existingSetupForTransaction(t *testing.T) (string, []byte, []byte, []byte, string) {
	t.Helper()
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := ConfigDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configData := []byte("singbox:\n  controller: http://127.0.0.1:9090\n  test_url: https://example.test/\nendpoints:\n  - name: old-proxy\n    type: singbox\n    outbound: old-proxy\n")
	singData := []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`)
	secretData := []byte("old-fixture-secret\n")
	for _, file := range []struct {
		name string
		data []byte
	}{{"config.yaml", configData}, {"sing-box.json", singData}, {"controller-secret", secretData}} {
		if err := os.WriteFile(filepath.Join(dir, file.name), file.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unitPath := filepath.Join(xdg, "systemd", "user", "frostgate-singbox.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(BuildSystemdUnit("/bin/sing-box", dir)), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, configData, singData, secretData, unitPath
}

func TestDirectOnlyTransactionRollsBackSidecarCleanupFailure(t *testing.T) {
	home, oldConfig, oldSing, oldSecret, unitPath := existingSetupForTransaction(t)
	fake := &transactionSystemd{
		runErrors: map[string][]error{"--user disable --now frostgate-singbox.service": {errors.New("forced stop failure")}},
		output: map[string][]byte{
			"--user is-enabled frostgate-singbox.service": []byte("enabled\n"),
			"--user is-active frostgate-singbox.service":  []byte("active\n"),
		},
	}
	oldFactory := systemdRunnerFactory
	systemdRunnerFactory = func() SystemdRunner { return fake }
	t.Cleanup(func() { systemdRunnerFactory = oldFactory })
	err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader("r\n3\n\n"))})
	if err == nil || !strings.Contains(err.Error(), "forced stop failure") {
		t.Fatalf("setup error = %v", err)
	}
	for _, want := range []struct {
		path string
		data []byte
	}{{filepath.Join(ConfigDir(home), "config.yaml"), oldConfig}, {filepath.Join(ConfigDir(home), "sing-box.json"), oldSing}, {filepath.Join(ConfigDir(home), "controller-secret"), oldSecret}} {
		got, readErr := os.ReadFile(want.path)
		if readErr != nil || string(got) != string(want.data) {
			t.Fatalf("rollback file %s = %q, %v", want.path, got, readErr)
		}
	}
	if unit, readErr := os.ReadFile(unitPath); readErr != nil || !unitOwnedByFrostgate(string(unit), filepath.Join(ConfigDir(home), "sing-box.json")) {
		t.Fatalf("unit was not restored: %v", readErr)
	}
	if !containsCall(fake.calls, "--user enable --now frostgate-singbox.service") {
		t.Fatalf("old enabled/active service was not restored: %v", fake.calls)
	}
}

func TestRequestedServiceInstallationFailureRollsBackSetup(t *testing.T) {
	for _, tc := range []struct {
		name string
		call string
	}{
		{"daemon reload", "--user daemon-reload"},
		{"enable", "--user enable --now frostgate-singbox.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, oldConfig, oldSing, oldSecret, _ := existingSetupForTransaction(t)
			binDir := t.TempDir()
			singBox := filepath.Join(binDir, "sing-box")
			if err := os.WriteFile(singBox, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir)
			fake := &transactionSystemd{
				runErrors: map[string][]error{tc.call: {errors.New("forced " + tc.name + " failure")}},
				output: map[string][]byte{
					"--user is-enabled frostgate-singbox.service": []byte("enabled\n"),
					"--user is-active frostgate-singbox.service":  []byte("active\n"),
				},
			}
			oldFactory := systemdRunnerFactory
			systemdRunnerFactory = func() SystemdRunner { return fake }
			t.Cleanup(func() { systemdRunnerFactory = oldFactory })
			link := "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?encryption=none&security=reality&sni=example.com&pbk=fixture-key&type=raw#safe"
			input := "r\n1\n" + link + "\nnew-route\nn\n\n\n"
			err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader(input))})
			if err == nil || !strings.Contains(err.Error(), "forced "+tc.name+" failure") {
				t.Fatalf("setup succeeded after requested service failure: %v", err)
			}
			for _, want := range []struct {
				path string
				data []byte
			}{{filepath.Join(ConfigDir(home), "config.yaml"), oldConfig}, {filepath.Join(ConfigDir(home), "sing-box.json"), oldSing}, {filepath.Join(ConfigDir(home), "controller-secret"), oldSecret}} {
				got, readErr := os.ReadFile(want.path)
				if readErr != nil || string(got) != string(want.data) {
					t.Fatalf("service-failure rollback file %s = %q, %v", want.path, got, readErr)
				}
			}
			if !containsCall(fake.calls, "--user enable --now frostgate-singbox.service") {
				t.Fatalf("old enabled/active service was not restored: %v", fake.calls)
			}
		})
	}
}

func TestRequestedServiceRestartFailureRollsBackSetup(t *testing.T) {
	home, oldConfig, oldSing, oldSecret, unitPath := existingSetupForTransaction(t)
	binDir := t.TempDir()
	singBox := filepath.Join(binDir, "sing-box")
	if err := os.WriteFile(singBox, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	fake := &transactionSystemd{
		runErrors: map[string][]error{"--user restart frostgate-singbox.service": {errors.New("forced restart failure")}},
		output: map[string][]byte{
			"--user is-enabled frostgate-singbox.service": []byte("enabled\n"),
			"--user is-active frostgate-singbox.service":  []byte("active\n"),
		},
	}
	oldFactory := systemdRunnerFactory
	systemdRunnerFactory = func() SystemdRunner { return fake }
	t.Cleanup(func() { systemdRunnerFactory = oldFactory })
	link := "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?encryption=none&security=reality&sni=example.com&pbk=fixture-key&type=raw#safe"
	input := "r\n1\n" + link + "\nnew-route\nn\n\n\n"
	err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader(input))})
	if err == nil || !strings.Contains(err.Error(), "forced restart failure") {
		t.Fatalf("setup succeeded after restart failure: %v", err)
	}
	for _, want := range []struct {
		path string
		data []byte
	}{{filepath.Join(ConfigDir(home), "config.yaml"), oldConfig}, {filepath.Join(ConfigDir(home), "sing-box.json"), oldSing}, {filepath.Join(ConfigDir(home), "controller-secret"), oldSecret}} {
		got, readErr := os.ReadFile(want.path)
		if readErr != nil || string(got) != string(want.data) {
			t.Fatalf("restart-failure rollback file %s = %q, %v", want.path, got, readErr)
		}
	}
	if unit, readErr := os.ReadFile(unitPath); readErr != nil || !unitOwnedByFrostgate(string(unit), filepath.Join(ConfigDir(home), "sing-box.json")) {
		t.Fatalf("restart-failure rollback did not restore unit: %v", readErr)
	}
	if !containsCall(fake.calls, "--user enable --now frostgate-singbox.service") {
		t.Fatalf("restart-failure rollback did not restore old enabled/active state: %v", fake.calls)
	}
}

func TestSetupEOFDoesNotAcceptDefault(t *testing.T) {
	home, oldConfig, _, _, _ := existingSetupForTransaction(t)
	err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader(""))})
	if err == nil || !strings.Contains(err.Error(), "input ended unexpectedly") {
		t.Fatalf("EOF error = %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(ConfigDir(home), "config.yaml"))
	if readErr != nil || string(got) != string(oldConfig) {
		t.Fatalf("EOF changed config: %q, %v", got, readErr)
	}
}

func TestDirectOnlyConversionRemovesOwnedSidecar(t *testing.T) {
	home, _, _, _, unitPath := existingSetupForTransaction(t)
	fake := &transactionSystemd{
		output: map[string][]byte{
			"--user is-enabled frostgate-singbox.service": []byte("enabled\n"),
		},
		outputSequence: map[string][][]byte{
			"--user is-active frostgate-singbox.service": {[]byte("active\n"), []byte("inactive\n")},
		},
	}
	oldFactory := systemdRunnerFactory
	systemdRunnerFactory = func() SystemdRunner { return fake }
	t.Cleanup(func() { systemdRunnerFactory = oldFactory })
	if err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader("r\n3\n\n"))}); err != nil {
		t.Fatalf("direct-only conversion: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("owned unit still exists: %v", err)
	}
	for _, name := range []string{"sing-box.json", "controller-secret"} {
		if _, err := os.Stat(filepath.Join(ConfigDir(home), name)); !os.IsNotExist(err) {
			t.Fatalf("obsolete sidecar file %s remains: %v", name, err)
		}
	}
	cfg, err := config.Load(filepath.Join(ConfigDir(home), "config.yaml"))
	if err != nil || hasProxyEndpoints(cfg) {
		t.Fatalf("direct-only config was not activated: cfg=%+v err=%v", cfg, err)
	}
	for _, call := range []string{"--user disable --now frostgate-singbox.service", "--user daemon-reload"} {
		if !containsCall(fake.calls, call) {
			t.Fatalf("missing %q from %v", call, fake.calls)
		}
	}
}

func TestManagedToExternalTransitionRemovesOwnedSidecarState(t *testing.T) {
	home, _, _, _, unitPath := existingSetupForTransaction(t)
	t.Setenv("PATH", "")
	fake := &transactionSystemd{
		output: map[string][]byte{
			"--user is-enabled frostgate-singbox.service": []byte("enabled\n"),
		},
		outputSequence: map[string][][]byte{
			"--user is-active frostgate-singbox.service": {[]byte("active\n"), []byte("inactive\n")},
		},
	}
	oldFactory := systemdRunnerFactory
	systemdRunnerFactory = func() SystemdRunner { return fake }
	t.Cleanup(func() { systemdRunnerFactory = oldFactory })

	input := "r\n2\nhttps://controller.example.test/base/\nnew-controller-secret\ny\nremote-route\n"
	if err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader(input))}); err != nil {
		t.Fatalf("managed to external transition: %v", err)
	}

	cfg, err := config.Load(filepath.Join(ConfigDir(home), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SingBox.Controller != "https://controller.example.test/base/" {
		t.Fatalf("external controller = %q", cfg.SingBox.Controller)
	}
	if _, err := os.Stat(filepath.Join(ConfigDir(home), "sing-box.json")); !os.IsNotExist(err) {
		t.Fatalf("stale managed sing-box state remains: %v", err)
	}
	secret, err := os.ReadFile(filepath.Join(ConfigDir(home), "controller-secret"))
	if err != nil || string(secret) != "new-controller-secret\n" {
		t.Fatalf("external controller secret = %q err=%v", secret, err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("stale managed systemd unit remains: %v", err)
	}
	if containsCall(fake.calls, "--user restart frostgate-singbox.service") {
		t.Fatalf("external transition restarted the removed local sidecar: %v", fake.calls)
	}
}

func TestDirectOnlyConversionLeavesUnownedUnitAlone(t *testing.T) {
	home, oldConfig, oldSing, oldSecret, unitPath := existingSetupForTransaction(t)
	original := []byte("[Unit]\nDescription=unrelated service\n")
	if err := os.WriteFile(unitPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &transactionSystemd{}
	oldFactory := systemdRunnerFactory
	systemdRunnerFactory = func() SystemdRunner { return fake }
	t.Cleanup(func() { systemdRunnerFactory = oldFactory })
	if err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader("r\n3\n\n"))}); err == nil {
		t.Fatal("direct-only conversion unexpectedly removed ambiguous state")
	}
	got, err := os.ReadFile(unitPath)
	if err != nil || string(got) != string(original) {
		t.Fatalf("unowned unit changed: %q, %v", got, err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unowned unit caused systemd calls: %v", fake.calls)
	}
	for _, want := range []struct {
		name string
		data []byte
	}{{"config.yaml", oldConfig}, {"sing-box.json", oldSing}, {"controller-secret", oldSecret}} {
		got, readErr := os.ReadFile(filepath.Join(ConfigDir(home), want.name))
		if readErr != nil || string(got) != string(want.data) {
			t.Fatalf("ambiguous cleanup changed %s: %q, %v", want.name, got, readErr)
		}
	}
}

func TestRollbackDoesNotTouchUnownedUnit(t *testing.T) {
	home, _, _, _, unitPath := existingSetupForTransaction(t)
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=other service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &transactionSystemd{}
	oldFactory := systemdRunnerFactory
	systemdRunnerFactory = func() SystemdRunner { return fake }
	t.Cleanup(func() { systemdRunnerFactory = oldFactory })

	// Reconfigure to a deliberately invalid choice after transaction setup.
	// The rollback path must leave a same-named, unowned unit alone.
	err := RunInteractive(Options{HomeDir: home, Stdin: bufio.NewReader(strings.NewReader("r\n9\n"))})
	if err == nil {
		t.Fatal("invalid setup unexpectedly succeeded")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unowned unit caused systemd calls: %v", fake.calls)
	}
	got, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(got) != "[Unit]\nDescription=other service\n" {
		t.Fatalf("unowned unit changed: %q, %v", got, readErr)
	}
}

func containsCall(calls []string, wanted string) bool {
	for _, call := range calls {
		if call == wanted {
			return true
		}
	}
	return false
}
