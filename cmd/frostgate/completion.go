package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type commandSpec struct {
	Name    string
	Usage   string
	Summary string
	Flags   []flagSpec
}

type flagSpec struct {
	Name        string
	TakesValue  bool
	Description string
}

var globalFlags = []flagSpec{
	{Name: "config", TakesValue: true, Description: "path to config file"},
	{Name: "json", Description: "emit JSON"},
	{Name: "watch", Description: "watch continuously"},
	{Name: "interval", TakesValue: true, Description: "watch interval"},
	{Name: "timeout", TakesValue: true, Description: "probe timeout"},
	{Name: "doctor", Description: "run diagnostics"},
	{Name: "check", Description: "validate config"},
	{Name: "version", Description: "print version"},
	{Name: "verbose", Description: "verbose output"},
	{Name: "debug", Description: "debug output"},
	{Name: "setup", Description: "run setup"},
}

// commandSpecs is the public command registry. Help and shell completion both
// consume it so command names cannot drift independently.
var commandSpecs = []commandSpec{
	{Name: "setup", Usage: "frostgate setup", Summary: "create or update local configuration"},
	{Name: "add", Usage: "frostgate add <link> [--name name] [--config path]", Summary: "add one VLESS or Hysteria2 route", Flags: []flagSpec{{Name: "name", TakesValue: true, Description: "route name"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "import", Usage: "frostgate import <url> [--allow-insecure-http] [--config path]", Summary: "import a subscription", Flags: []flagSpec{{Name: "allow-insecure-http", Description: "allow HTTP subscription URLs"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "test", Usage: "frostgate test <link> [--json]", Summary: "test one route without saving it", Flags: []flagSpec{{Name: "json", Description: "emit JSON"}}},
	{Name: "benchmark", Usage: "frostgate benchmark [--network label] [--no-speed] [--json] [--config path]", Summary: "measure route health and download speed", Flags: []flagSpec{{Name: "network", TakesValue: true, Description: "network label"}, {Name: "no-speed", Description: "skip throughput"}, {Name: "json", Description: "emit JSON"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "diagnose", Usage: "frostgate diagnose [route] [--json] [--config path]", Summary: "diagnose route failures", Flags: []flagSpec{{Name: "json", Description: "emit JSON"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "network-check", Usage: "frostgate network-check [--json] [--config path]", Summary: "check local network signals", Flags: []flagSpec{{Name: "json", Description: "emit JSON"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "network_check", Usage: "frostgate network_check [--json] [--config path]", Summary: "alias for network-check", Flags: []flagSpec{{Name: "json", Description: "emit JSON"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "compare", Usage: "frostgate compare [--json] [--config path]", Summary: "compare recorded networks", Flags: []flagSpec{{Name: "json", Description: "emit JSON"}, {Name: "config", TakesValue: true, Description: "profile config path"}}},
	{Name: "recommend", Usage: "frostgate recommend [--json] [--config path]", Summary: "show the recommended route", Flags: []flagSpec{{Name: "json", Description: "emit JSON"}, {Name: "config", TakesValue: true, Description: "path to config file"}}},
	{Name: "history", Usage: "frostgate history [--since duration] [--route name] [--network label] [--config path] [--json]", Summary: "show local history", Flags: []flagSpec{{Name: "since", TakesValue: true, Description: "time range"}, {Name: "route", TakesValue: true, Description: "route name"}, {Name: "network", TakesValue: true, Description: "network label"}, {Name: "config", TakesValue: true, Description: "profile config path"}, {Name: "json", Description: "emit JSON"}}},
	{Name: "doctor", Usage: "frostgate doctor [--config path] [--verbose]", Summary: "check local setup", Flags: []flagSpec{{Name: "config", TakesValue: true, Description: "path to config file"}, {Name: "verbose", Description: "verbose output"}}},
	{Name: "completion", Usage: "frostgate completion <fish|bash|zsh>", Summary: "print shell completion"},
	{Name: "help", Usage: "frostgate help", Summary: "show help"},
}

func handleCompletion(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: frostgate completion <fish|bash|zsh>")
		return 3
	}
	var script string
	switch args[0] {
	case "fish":
		script = fishCompletion()
	case "bash":
		script = bashCompletion()
	case "zsh":
		script = zshCompletion()
	default:
		fmt.Fprintf(os.Stderr, "unsupported shell %q (supported: fish, bash, zsh)\n", sanitizeOutput(args[0]))
		return 3
	}
	_, _ = io.WriteString(os.Stdout, script)
	return 0
}

func commandNames() []string {
	names := make([]string, 0, len(commandSpecs))
	for _, spec := range commandSpecs {
		names = append(names, spec.Name)
	}
	return names
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("function __frostgate_needs_command\n    set -l words (commandline -opc)\n    test (count $words) -eq 1\nend\n\ncomplete -c frostgate -f\n")
	for _, spec := range commandSpecs {
		fmt.Fprintf(&b, "complete -c frostgate -n '__frostgate_needs_command' -a '%s' -d '%s'\n", spec.Name, fishEscape(spec.Summary))
	}
	for _, flag := range globalFlags {
		writeFishFlag(&b, "", flag)
	}
	for _, spec := range commandSpecs {
		for _, flag := range spec.Flags {
			writeFishFlag(&b, spec.Name, flag)
		}
	}
	return b.String()
}

func writeFishFlag(b *strings.Builder, command string, flag flagSpec) {
	if command == "" {
		fmt.Fprintf(b, "complete -c frostgate -l '%s'", flag.Name)
	} else {
		fmt.Fprintf(b, "complete -c frostgate -n '__fish_seen_subcommand_from %s' -l '%s'", command, flag.Name)
	}
	if flag.TakesValue {
		b.WriteString(" -r")
	}
	if flag.Description != "" {
		fmt.Fprintf(b, " -d '%s'", fishEscape(flag.Description))
	}
	b.WriteByte('\n')
}

func fishEscape(s string) string { return strings.ReplaceAll(s, "'", "\\'") }

func bashCompletion() string {
	var b strings.Builder
	fmt.Fprintf(&b, "_frostgate() {\n    local cur command opts\n    cur=\"${COMP_WORDS[COMP_CWORD]}\"\n    if [ \"$COMP_CWORD\" -eq 1 ]; then\n        COMPREPLY=( $(compgen -W '%s' -- \"$cur\") )\n        return\n    fi\n    command=\"${COMP_WORDS[1]}\"\n    opts=\"%s\"\n    case \"$command\" in\n", strings.Join(commandNames(), " "), bashFlags(globalFlags))
	for _, spec := range commandSpecs {
		if len(spec.Flags) > 0 {
			fmt.Fprintf(&b, "        %s) opts=\"$opts %s\" ;;\n", spec.Name, bashFlags(spec.Flags))
		}
	}
	b.WriteString("    esac\n    if [[ \"$cur\" == -* ]]; then\n        COMPREPLY=( $(compgen -W \"$opts\" -- \"$cur\") )\n    fi\n}\ncomplete -F _frostgate frostgate\n")
	return b.String()
}

func bashFlags(flags []flagSpec) string {
	var names []string
	for _, flag := range flags {
		names = append(names, "--"+flag.Name)
	}
	return strings.Join(names, " ")
}

func zshCompletion() string {
	var b strings.Builder
	b.WriteString("#compdef frostgate\n\n_frostgate() {\n  local -a commands\n  commands=(\n")
	for _, spec := range commandSpecs {
		fmt.Fprintf(&b, "    '%s:%s'\n", spec.Name, zshEscape(spec.Summary))
	}
	b.WriteString("  )\n  _arguments -C \\\n")
	for _, flag := range globalFlags {
		fmt.Fprintf(&b, "    '--%s[%s]' \\\n", flag.Name, zshEscape(flag.Description))
	}
	b.WriteString("    '1:command:->command' \\\n    '*::argument:->args'\n\n  case $state in\n    command) _describe -t commands 'frostgate command' commands ;;\n    args)\n      case $words[2] in\n")
	for _, spec := range commandSpecs {
		if len(spec.Flags) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %s) _arguments", spec.Name)
		for _, flag := range spec.Flags {
			fmt.Fprintf(&b, " '--%s[%s]'", flag.Name, zshEscape(flag.Description))
		}
		b.WriteString(" ;;\n")
	}
	b.WriteString("      esac ;;\n  esac\n}\n\n_frostgate \"$@\"\n")
	return b.String()
}

func zshEscape(s string) string { return strings.ReplaceAll(s, "]", "\\]") }
