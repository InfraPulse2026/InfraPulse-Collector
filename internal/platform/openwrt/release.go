package openwrt

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"InfraPulseAgent/internal/model"
)

const maxReleaseBytes = 64 << 10

// Detect requires distribution evidence. Hostname, ubus presence, and CPU
// architecture alone never select OpenWrt.
func Detect(etcRoot string) bool {
	_, evidence := readRelease(context.Background(), etcRoot)
	return len(evidence) != 0
}

func readRelease(ctx context.Context, etcRoot string) (map[string]string, []model.Evidence) {
	merged := make(map[string]string)
	var evidence []model.Evidence
	for _, name := range []string{"openwrt_release", "os-release"} {
		data, err := readFile(ctx, filepath.Join(etcRoot, name), maxReleaseBytes)
		if err != nil {
			continue
		}
		values, err := parseRelease(string(data))
		if err != nil {
			continue
		}
		openwrt := values["DISTRIB_ID"] == "OpenWrt" || values["ID"] == "openwrt"
		if !openwrt {
			continue
		}
		for k, v := range values {
			merged[k] = v
		}
		evidence = append(evidence, model.Evidence{Source: "/etc/" + name, Value: "OpenWrt distribution identifier", Confidence: "high"})
	}
	return merged, evidence
}

// Release files are parsed as data. Quotes and simple escapes are supported;
// shell substitutions and variables are never evaluated.
func parseRelease(data string) (map[string]string, error) {
	result := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || len(key) > 64 || len(value) > maxFieldBytes {
			return nil, errors.New("malformed OpenWrt release file")
		}
		for _, c := range key {
			if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return nil, errors.New("malformed OpenWrt release key")
			}
		}
		if strings.HasPrefix(value, "'") || strings.HasPrefix(value, `"`) {
			quote := value[0]
			if len(value) < 2 || value[len(value)-1] != quote {
				return nil, errors.New("malformed OpenWrt release quote")
			}
			value = value[1 : len(value)-1]
			if quote == '"' {
				value = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(value)
			}
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("malformed OpenWrt release value")
		}
		result[key] = value
	}
	return result, nil
}

func readFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("OpenWrt source is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errOutputLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
