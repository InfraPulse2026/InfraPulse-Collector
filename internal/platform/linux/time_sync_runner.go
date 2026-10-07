package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// ntpOperation is a closed set of OS service/status operations. No command or
// argument from configuration, network data, or a Controller is executable.
type ntpOperation uint8

const (
	ntpChronyService ntpOperation = iota + 1
	ntpChronydService
	ntpTimesyncdService
	ntpOtherService
	ntpNtpdService
	ntpOpenntpdService
	ntpNtpsecService
	ntpTracking
	ntpSources
	ntpTimesyncStatus
	ntpClockStatus
	ntpRestartChrony
	ntpRestartChronyd
	ntpRestartTimesyncd
)

type ntpRunner interface {
	Run(context.Context, ntpOperation) ([]byte, error)
}

type ntpExecRunner struct{}

func ntpCommand(op ntpOperation) (string, []string, error) {
	var unit string
	switch op {
	case ntpChronyService:
		unit = "chrony.service"
	case ntpChronydService:
		unit = "chronyd.service"
	case ntpTimesyncdService:
		unit = "systemd-timesyncd.service"
	case ntpOtherService:
		unit = "ntp.service"
	case ntpNtpdService:
		unit = "ntpd.service"
	case ntpOpenntpdService:
		unit = "openntpd.service"
	case ntpNtpsecService:
		unit = "ntpsec.service"
	case ntpTracking:
		return "chronyc", []string{"-c", "tracking"}, nil
	case ntpSources:
		// -N preserves the configured hostname rather than reverse DNS.
		return "chronyc", []string{"-N", "-c", "sources", "-a"}, nil
	case ntpTimesyncStatus:
		return "timedatectl", []string{"show-timesync", "--no-pager", "--property=SystemNTPServers", "--property=LinkNTPServers", "--property=FallbackNTPServers"}, nil
	case ntpClockStatus:
		return "timedatectl", []string{"show", "--no-pager", "--property=NTPSynchronized"}, nil
	case ntpRestartChrony:
		return "systemctl", []string{"--no-ask-password", "restart", "chrony.service"}, nil
	case ntpRestartChronyd:
		return "systemctl", []string{"--no-ask-password", "restart", "chronyd.service"}, nil
	case ntpRestartTimesyncd:
		return "systemctl", []string{"--no-ask-password", "restart", "systemd-timesyncd.service"}, nil
	default:
		return "", nil, errors.New("unsupported time synchronization operation")
	}
	return "systemctl", []string{"show", unit, "--no-pager", "--property=Id", "--property=LoadState", "--property=ActiveState", "--property=MainPID"}, nil
}

func (ntpExecRunner) Run(ctx context.Context, op ntpOperation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, args, err := ntpCommand(op)
	if err != nil {
		return nil, err
	}
	// Only standard absolute binary paths are considered; PATH is never used.
	var binary string
	for _, dir := range []string{"/usr/bin/", "/bin/", "/usr/sbin/", "/sbin/"} {
		candidate := dir + name
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			binary = candidate
			break
		}
	}
	if binary == "" {
		return nil, fmt.Errorf("time synchronization OS tool %s is unavailable", name)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0")
	cmd.WaitDelay = 100 * time.Millisecond
	stdout, stderr := ntpOutput{limit: 64 << 10}, ntpOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errors.New("time synchronization OS response exceeds limit")
	}
	if err != nil {
		// Do not report OS output, which can contain unrelated administrator data.
		return nil, fmt.Errorf("time synchronization OS operation failed: %w", err)
	}
	return stdout.Bytes(), nil
}

type ntpOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *ntpOutput) Write(p []byte) (int, error) {
	n := len(p)
	if n > b.limit-b.Len() {
		b.exceeded = true
		p = p[:b.limit-b.Len()]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
