package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverRegisteredCommandsAndFlags(t *testing.T) {
	scripts := map[string]string{
		"fish": fishCompletion(),
		"bash": bashCompletion(),
		"zsh":  zshCompletion(),
	}
	for shell, script := range scripts {
		t.Run(shell, func(t *testing.T) {
			for _, spec := range commandSpecs {
				if !strings.Contains(script, spec.Name) {
					t.Errorf("missing command %q", spec.Name)
				}
			}
			for _, flag := range globalFlags {
				if !strings.Contains(script, completionFlagToken(shell, flag.Name)) {
					t.Errorf("missing global flag --%s", flag.Name)
				}
			}
			for _, spec := range commandSpecs {
				for _, flag := range spec.Flags {
					if !strings.Contains(script, completionFlagToken(shell, flag.Name)) {
						t.Errorf("missing %s flag --%s", spec.Name, flag.Name)
					}
				}
			}
		})
	}
}

func completionFlagToken(shell, name string) string {
	if shell == "fish" {
		return "-l '" + name + "'"
	}
	return "--" + name
}

func TestCompletionCommandGeneratesScripts(t *testing.T) {
	f := newCLIFixture(t)
	for _, shell := range []string{"fish", "bash", "zsh"} {
		stdout, stderr, code := runCLI(t, f.env(), "completion", shell)
		if code != 0 || len(stdout) == 0 || len(stderr) != 0 {
			t.Fatalf("completion %s: code=%d stdout=%q stderr=%q", shell, code, stdout, stderr)
		}
	}
}

func TestFishCompletionEngine(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish not installed")
	}
	path := writeCompletionScript(t, "frostgate.fish", fishCompletion())
	out := runShellCompletion(t, "fish", "source $argv[1]; complete -C 'frostgate recom'", path)
	if !containsLine(out, "recommend") {
		t.Fatalf("frostgate recom did not complete to recommend: %q", out)
	}
	flags := runShellCompletion(t, "fish", "source $argv[1]; complete -C 'frostgate benchmark --'", path)
	if !containsLine(flags, "--json") || !containsLine(flags, "--network") {
		t.Fatalf("benchmark flags missing from fish completion: %q", flags)
	}
	for _, invalid := range []string{"--watch", "--doctor", "--version"} {
		if containsLine(flags, invalid) {
			t.Fatalf("benchmark completion advertised invalid root flag %s: %q", invalid, flags)
		}
	}
}

func TestBashCompletionScriptAndEngine(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	path := writeCompletionScript(t, "frostgate.bash", bashCompletion())
	cmd := exec.Command("bash", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash syntax: %v\n%s", err, out)
	}
	cmd = exec.Command("bash", "-c", `source "$1"; COMP_WORDS=(frostgate recom); COMP_CWORD=1; _frostgate; printf '%s\n' "${COMPREPLY[@]}"`, "bash", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash completion: %v\n%s", err, out)
	}
	if !containsLine(string(out), "recommend") {
		t.Fatalf("frostgate recom did not complete to recommend: %q", out)
	}
}

func TestZshCompletionScriptSyntax(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not installed")
	}
	path := writeCompletionScript(t, "_frostgate", zshCompletion())
	cmd := exec.Command("zsh", "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zsh syntax: %v\n%s", err, out)
	}
}

func writeCompletionScript(t *testing.T, name, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runShellCompletion(t *testing.T, shell, expression, path string) string {
	t.Helper()
	cmd := exec.Command(shell, "-c", expression, "--", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s completion: %v\n%s", shell, err, out.String())
	}
	return out.String()
}

func containsLine(output, want string) bool {
	for _, line := range strings.Fields(output) {
		if line == want {
			return true
		}
	}
	return false
}
