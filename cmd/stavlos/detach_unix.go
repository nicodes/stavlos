//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func signalCtx(ctx context.Context) context.Context {
	c, _ := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	return c
}
