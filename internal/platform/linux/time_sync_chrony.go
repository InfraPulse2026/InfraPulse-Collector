package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type ntpChronyLayout struct {
	managed string
	servers []string
}

func (p *TimeSync) chronyConfig(ctx context.Context, service ntpService) (string, error) {
	if service.pid <= 0 {
		return "", errors.New("setup_required: active chrony MainPID is unavailable")
	}
	cmdline := "/proc/" + strconv.Itoa(service.pid) + "/cmdline"
	if err := p.safePath(cmdline, false); err != nil {
		return "", err
	}
	info, err := os.Lstat(p.physical(cmdline))
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("setup_required: active chronyd command line is not readable as a regular procfs file")
	}
	data, err := readBounded(ctx, p.physical(cmdline))
	if err != nil {
		return "", fmt.Errorf("setup_required: cannot verify active chronyd configuration: %w", err)
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(args) == 0 || path.Base(args[0]) != "chronyd" {
		return "", errors.New("setup_required: active service is not a recognized chronyd process")
	}
	var config string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-f", "-u", "-F", "-L":
			if i+1 >= len(args) {
				return "", errors.New("setup_required: malformed chronyd command line")
			}
			if args[i] == "-f" {
				if config != "" {
					return "", errors.New("setup_required: multiple chronyd configuration paths")
				}
				config = args[i+1]
			}
			i++
		case "-n", "-d", "-r", "-R", "-s", "-4", "-6", "-m":
		case "-x":
			return "", errors.New("setup_required: chronyd -x does not synchronize the system clock")
		default:
			return "", errors.New("setup_required: custom chronyd command line requires administrator configuration")
		}
	}
	if config != "" {
		if !strings.HasPrefix(config, "/etc/") || path.Clean(config) != config {
			return "", errors.New("setup_required: custom chronyd configuration must be a normalized absolute /etc path")
		}
		return config, nil
	}
	// Package defaults differ between distributions. An ambiguous installation
	// requires the administrator to specify -f; never guess the active file.
	for _, candidate := range []string{"/etc/chrony.conf", "/etc/chrony/chrony.conf"} {
		if _, err := os.Lstat(p.physical(candidate)); err == nil {
			if config != "" {
				return "", errors.New("setup_required: ambiguous chronyd default configuration paths; administrator must specify -f")
			}
			config = candidate
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if config == "" {
		return "", errors.New("setup_required: chronyd persistent configuration was not found")
	}
	return config, nil
}

func (p *TimeSync) chronyLayout(ctx context.Context, service ntpService, desired []string) (ntpChronyLayout, error) {
	config, err := p.chronyConfig(ctx, service)
	if err != nil {
		return ntpChronyLayout{}, err
	}
	var layout ntpChronyLayout
	type source struct{ host, file string }
	var sources []source
	visited := make(map[string]bool)
	var read func(string, int) error
	read = func(file string, depth int) error {
		if depth > 4 || len(visited) >= 64 || visited[file] {
			return errors.New("setup_required: complex or cyclic chrony includes require administrator configuration")
		}
		visited[file] = true
		if err := p.safePath(file, false); err != nil {
			return err
		}
		info, err := os.Lstat(p.physical(file))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("setup_required: chrony configuration is not a regular file")
		}
		data, err := readBounded(ctx, p.physical(file))
		if err != nil {
			return err
		}
		if len(data) > 256<<10 {
			return errors.New("chrony configuration exceeds bounded setup-helper limit")
		}
		for _, line := range strings.Split(string(data), "\n") {
			line, _, _ = strings.Cut(line, "#")
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "server", "pool", "peer":
				if len(fields) < 2 || len(sources) >= 1024 {
					return errors.New("invalid or oversized chrony source configuration")
				}
				sources = append(sources, source{fields[1], file})
			case "local", "refclock":
				return errors.New("setup_required: chrony local/reference-clock sources require administrator review for corporate NTP")
			case "confdir", "sourcedir", "include":
				// Deliberately support the common, direct single-directory forms.
				// Multi-directory shadowing/quoted paths are left to an operator.
				if len(fields) != 2 || !strings.HasPrefix(fields[1], "/") || strings.ContainsAny(fields[1], "'\"\\") {
					return errors.New("setup_required: custom chrony include form requires administrator configuration")
				}
				pattern, managed := fields[1], ""
				if fields[0] != "include" {
					suffix := ".conf"
					if fields[0] == "sourcedir" {
						suffix = ".sources"
					}
					pattern = path.Join(pattern, "*"+suffix)
					managed = path.Join(fields[1], "90-infrapulse"+suffix)
				} else if path.Base(pattern) == "*.conf" {
					managed = path.Join(path.Dir(pattern), "90-infrapulse.conf")
				}
				if path.Clean(pattern) != pattern || strings.ContainsAny(path.Dir(pattern), "*?[") {
					return errors.New("setup_required: complex chrony include pattern is unsupported")
				}
				if fields[0] == "sourcedir" && !strings.HasPrefix(pattern, "/etc/") {
					return errors.New("setup_required: chrony dynamic/DHCP source directory must be disabled or configured by the administrator")
				}
				if layout.managed == "" && strings.HasPrefix(managed, "/etc/") {
					layout.managed = managed
				}
				matches, err := filepath.Glob(p.physical(pattern))
				if err != nil || len(matches) > 64 || (fields[0] == "include" && len(matches) == 0) {
					return errors.New("setup_required: chrony include is invalid, empty, or exceeds limits")
				}
				for _, match := range matches {
					rel, err := filepath.Rel(p.root, match)
					if err != nil || strings.HasPrefix(rel, "..") {
						return errors.New("chrony include escaped the OS root")
					}
					if err := read("/"+filepath.ToSlash(rel), depth+1); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := read(config, 0); err != nil {
		return layout, err
	}
	for _, src := range sources {
		layout.servers = append(layout.servers, src.host)
		if slices.Contains(desired, src.host) {
			continue
		}
		if src.file == layout.managed {
			// Only our previously managed sources may be replaced. All other
			// NTP settings remain owned by the administrator.
			if _, exists, err := p.ownedFile(ctx, src.file); err == nil && exists {
				continue
			}
		}
		return layout, errors.New("setup_required: administrator-owned chrony configuration contains other NTP sources; remove or replace them manually")
	}
	layout.servers = ntpUnique(layout.servers)
	return layout, nil
}
