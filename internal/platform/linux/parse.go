package linux

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"InfraPulseAgent/internal/model"
)

type cpuTimes struct {
	fields [8]uint64
	total  uint64
	idle   uint64
}

func parseCPUStat(text string) (cpuTimes, int, error) {
	var times cpuTimes
	logical, found := 0, false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] != "cpu" {
			if strings.HasPrefix(fields[0], "cpu") {
				if _, err := strconv.ParseUint(strings.TrimPrefix(fields[0], "cpu"), 10, 32); err == nil {
					logical++
				}
			}
			continue
		}
		if found || len(fields) < 5 {
			return cpuTimes{}, 0, errors.New("missing or duplicate aggregate CPU counters")
		}
		found = true
		// guest and guest_nice are already included in user and nice. Summing
		// them a second time inflates both utilization and total CPU time.
		for i := 1; i < len(fields) && i <= len(times.fields); i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil || math.MaxUint64-times.total < v {
				return cpuTimes{}, 0, errors.New("invalid aggregate CPU counter")
			}
			times.fields[i-1] = v
			times.total += v
		}
		times.idle = times.fields[3] + times.fields[4]
	}
	if !found || logical == 0 {
		return cpuTimes{}, 0, errors.New("aggregate or logical CPU counters absent")
	}
	return times, logical, nil
}

func cpuUsage(previous, current cpuTimes) *float64 {
	for i := range current.fields {
		if current.fields[i] < previous.fields[i] {
			return nil
		}
	}
	if current.total <= previous.total || current.idle < previous.idle {
		return nil
	}
	total, idle := current.total-previous.total, current.idle-previous.idle
	if idle > total {
		return nil
	}
	usage := float64(total-idle) / float64(total) * 100
	return &usage
}

func parseLoad(text string) ([3]float64, error) {
	var result [3]float64
	fields := strings.Fields(text)
	if len(fields) < len(result) {
		return result, errors.New("load averages absent")
	}
	for i := range result {
		v, err := finiteNonnegative(fields[i])
		if err != nil {
			return result, err
		}
		result[i] = v
	}
	return result, nil
}

func finiteNonnegative(text string) (float64, error) {
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, errors.New("invalid nonnegative measurement")
	}
	return v, nil
}

func parseMemory(text string) (model.MemoryStats, error) {
	keys := map[string]bool{"MemTotal": true, "MemFree": true, "MemAvailable": true,
		"Buffers": true, "Cached": true, "SReclaimable": true, "Shmem": true,
		"SwapTotal": true, "SwapFree": true}
	values := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		key, remainder, ok := strings.Cut(line, ":")
		if !ok || !keys[key] {
			continue
		}
		fields := strings.Fields(remainder)
		if len(fields) != 2 || fields[1] != "kB" {
			return model.MemoryStats{}, fmt.Errorf("invalid memory unit for %s", key)
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || v > math.MaxUint64/1024 {
			return model.MemoryStats{}, fmt.Errorf("invalid memory value for %s", key)
		}
		if _, duplicate := values[key]; duplicate {
			return model.MemoryStats{}, fmt.Errorf("duplicate memory value for %s", key)
		}
		values[key] = v * 1024
	}
	total, totalOK := values["MemTotal"]
	free, freeOK := values["MemFree"]
	if !totalOK || !freeOK || total == 0 || free > total {
		return model.MemoryStats{}, errors.New("invalid or absent total/free memory")
	}
	available, availableOK := values["MemAvailable"]
	if !availableOK {
		// Older kernels lack MemAvailable. Approximate reclaimable memory
		// without counting shared memory twice, and cap it at total memory.
		for _, key := range []string{"MemFree", "Buffers", "Cached", "SReclaimable"} {
			value := values[key]
			if math.MaxUint64-available < value {
				return model.MemoryStats{}, errors.New("memory fallback overflow")
			}
			available += value
		}
		if values["Shmem"] > available {
			available = 0
		} else {
			available -= values["Shmem"]
		}
		if available > total {
			available = total
		}
	}
	if available > total || values["SwapFree"] > values["SwapTotal"] {
		return model.MemoryStats{}, errors.New("memory availability exceeds total")
	}
	return model.MemoryStats{TotalBytes: total, UsedBytes: total - available,
		AvailableBytes: available, FreeBytes: free, SwapTotalBytes: values["SwapTotal"],
		SwapUsedBytes: values["SwapTotal"] - values["SwapFree"]}, nil
}

func parseInterfaces(text string) ([]model.Interface, error) {
	var interfaces []model.Interface
	names := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		name, remainder, ok := strings.Cut(line, ":")
		if !ok {
			continue // /proc/net/dev has two header lines without a colon.
		}
		name = strings.TrimSpace(name)
		// Linux interface names cannot contain a slash. Rejecting dot paths
		// also protects metadata lookups against a corrupted fixture/source.
		if name == "" || len(name) > 15 || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || names[name] {
			return nil, errors.New("invalid or duplicate interface name")
		}
		if len(interfaces) >= maxInterfaces {
			return nil, errors.New("interface count exceeds limit")
		}
		names[name] = true
		fields := strings.Fields(remainder)
		if len(fields) != 16 {
			return nil, fmt.Errorf("interface %s: expected 16 counters", name)
		}
		var values [16]uint64
		for i, field := range fields {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("interface %s: invalid counter", name)
			}
			values[i] = v
		}
		interfaces = append(interfaces, model.Interface{Name: name, Counters: model.Counters{
			RXBytes: values[0], RXPackets: values[1], RXErrors: values[2], RXDropped: values[3],
			TXBytes: values[8], TXPackets: values[9], TXErrors: values[10], TXDropped: values[11]}})
	}
	if len(interfaces) == 0 {
		return nil, errors.New("interface counters absent")
	}
	sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Name < interfaces[j].Name })
	return interfaces, nil
}

func parsePressure(text string) *float64 {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if ok && key == "avg10" {
				v, err := finiteNonnegative(value)
				if err == nil && v <= 100 {
					return &v
				}
			}
		}
	}
	return nil
}

func parseOSRelease(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	return values
}

func parseCPUInfo(text string) (int, string) {
	physicalIDs := map[string]bool{}
	var cpuModel string
	for _, block := range strings.Split(strings.TrimSpace(text), "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok {
				fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
		if cpuModel == "" {
			cpuModel = fields["model name"]
			if cpuModel == "" {
				cpuModel = fields["Hardware"]
			}
		}
		physical, hasPhysical := fields["physical id"]
		core, hasCore := fields["core id"]
		if hasPhysical && hasCore {
			physicalIDs[physical+":"+core] = true
		}
	}
	// When IDs are unavailable, physical core count remains unavailable (0)
	// rather than falsely treating logical threads as physical cores.
	return len(physicalIDs), cpuModel
}
