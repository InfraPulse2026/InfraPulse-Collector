package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEnrollmentConfig(t *testing.T) {
	base := Defaults()
	base.Local.Enabled = false
	base.Security.Enrollment = Enrollment{Enabled: true, URL: "https://enroll.example", CAFile: "ca.pem", TokenEnv: "ENROLLMENT_TOKEN", Timeout: time.Second}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"http", "userinfo", "path", "query", "development", "literal env", "manual key", "timeout"} {
		t.Run(name, func(t *testing.T) {
			c := base
			switch name {
			case "http":
				c.Security.Enrollment.URL = "http://enroll.example"
			case "userinfo":
				c.Security.Enrollment.URL = "https://sensitive:secret@enroll.example"
			case "path":
				c.Security.Enrollment.URL = "https://enroll.example/arbitrary"
			case "query":
				c.Security.Enrollment.URL = "https://enroll.example?token=secret"
			case "development":
				c.Security.Mode = "development"
			case "literal env":
				c.Security.Enrollment.TokenEnv = "token with secret"
			case "manual key":
				c.Security.KeyFile = "key.pem"
			case "timeout":
				c.Security.Enrollment.Timeout = time.Hour
			}
			if err := c.Validate(); err == nil {
				t.Fatal("unsafe enrollment config accepted")
			}
		})
	}
	ResolvePKIPaths(&base, "/configuration")
	if base.Security.Enrollment.CAFile != filepath.Join("/configuration", "ca.pem") {
		t.Fatal("bootstrap CA path not resolved")
	}
}
