// Package openwrt adds OpenWrt-specific, read-only inventory to Linux collectors.
package openwrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Operation is a closed set of local reads. It cannot contain a command, an
// endpoint, or arguments supplied by the Controller.
type Operation uint8

const (
	Board Operation = iota + 1
	Networks
	Services
	Firewall
	NTPConfig
)

const (
	maxResponseBytes = 1 << 20
	maxStderrBytes   = 4096
	maxEntries       = 1024
	maxFieldBytes    = 1024
	defaultTimeout   = 2 * time.Second
)

var errOutputLimit = errors.New("OpenWrt response exceeds limit")

// Runner permits fixtures without making the production API an arbitrary
// process runner. Implementations must honor context cancellation.
type Runner interface {
	Run(context.Context, Operation) ([]byte, error)
}

type execRunner struct{}

func arguments(op Operation) ([]string, error) {
	switch op {
	case Board:
		return []string{"-S", "call", "system", "board", "{}"}, nil
	case Networks:
		return []string{"-S", "call", "network.interface", "dump", "{}"}, nil
	case Services:
		return []string{"-S", "call", "service", "list", `{"verbose":false}`}, nil
	case Firewall:
		return []string{"-S", "call", "uci", "get", `{"config":"firewall"}`}, nil
	case NTPConfig:
		return []string{"-S", "call", "uci", "get", `{"config":"system","section":"ntp"}`}, nil
	default:
		return nil, errors.New("unsupported OpenWrt operation")
	}
}

func (execRunner) Run(ctx context.Context, op Operation) ([]byte, error) {
	args, err := arguments(op)
	if err != nil {
		return nil, err
	}
	// Absolute binary and literal arguments avoid PATH replacement and shell
	// interpretation. No init scripts, uci mutations, or nft commands are run.
	cmd := exec.CommandContext(ctx, "/bin/ubus", args...)
	cmd.WaitDelay = 100 * time.Millisecond
	stdout := limitedBuffer{limit: maxResponseBytes}
	stderr := limitedBuffer{limit: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errOutputLimit
	}
	if err != nil {
		// ubus output can contain sensitive data; never include stdout/stderr.
		return nil, fmt.Errorf("OpenWrt typed read failed: %w", err)
	}
	return stdout.Bytes(), nil
}

// Writes consume excess bytes without allocating them. exec keeps draining its
// pipes while CommandContext/WaitDelay bounds the process lifetime.
type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
