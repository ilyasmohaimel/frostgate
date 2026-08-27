// Package process centralizes bounded external command execution.
package process

import (
	"context"
	"os/exec"
	"time"
)

const (
	QuickTimeout      = 3 * time.Second
	ValidationTimeout = 20 * time.Second
	ServiceTimeout    = 15 * time.Second
	InstallTimeout    = 5 * time.Minute
)

func CommandContext(parent context.Context, timeout time.Duration, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return exec.CommandContext(ctx, name, args...), cancel
}

func Output(parent context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cmd, cancel := CommandContext(parent, timeout, name, args...)
	defer cancel()
	return cmd.Output()
}

func Run(parent context.Context, timeout time.Duration, name string, args ...string) error {
	cmd, cancel := CommandContext(parent, timeout, name, args...)
	defer cancel()
	return cmd.Run()
}
