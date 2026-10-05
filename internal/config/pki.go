package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Enrollment contains public bootstrap settings and an environment reference,
// never an enrollment token or a private key.
type Enrollment struct {
	Enabled    bool          `yaml:"enabled"`
	URL        string        `yaml:"url"`
	CAFile     string        `yaml:"ca_file"`
	ServerName string        `yaml:"server_name"`
	TokenEnv   string        `yaml:"token_env"`
	Timeout    time.Duration `yaml:"timeout"`
}

func ValidatePKI(c Config) error {
	e := c.Security.Enrollment
	if c.Security.RenewBefore < 0 || c.Security.RenewBefore > 90*24*time.Hour {
		return errors.New("invalid security.renew_before")
	}
	if !e.Enabled {
		if e.URL != "" || e.CAFile != "" || e.ServerName != "" || e.TokenEnv != "" || e.Timeout != 0 {
			return errors.New("enrollment settings require security.enrollment.enabled")
		}
		return nil
	}
	u, err := url.Parse(e.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(e.URL, "\r\n\x00") {
		return errors.New("invalid security.enrollment.url: verified HTTPS origin required")
	}
	if c.Security.Mode != "production" || e.CAFile == "" || e.TokenEnv == "" || len(e.TokenEnv) > 128 {
		return errors.New("enrollment requires production mode, bootstrap CA and token environment reference")
	}
	for i, r := range e.TokenEnv {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return errors.New("invalid security.enrollment.token_env")
		}
	}
	if e.Timeout != 0 && (e.Timeout < 100*time.Millisecond || e.Timeout > time.Minute) {
		return errors.New("invalid security.enrollment.timeout")
	}
	if c.Security.CertFile != "" || c.Security.KeyFile != "" {
		return errors.New("enrollment manages its certificate bundle; manual cert_file/key_file must be absent")
	}
	return nil
}

func ResolvePKIPaths(c *Config, baseDir string) {
	if p := &c.Security.Enrollment.CAFile; *p != "" && !filepath.IsAbs(*p) {
		*p = filepath.Join(baseDir, *p)
	}
}
