package commands

import (
	"InfraPulseAgent/internal/model"
	"errors"
	"testing"
	"time"
)

func command() model.Command {
	return model.Command{ID: "c1", TargetDeviceID: "router", RequestedAt: time.Now().UTC(), Timeout: time.Second, Initiator: "ai_suggested", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
}
func TestValidate(t *testing.T) {
	base := command()
	if e := Validate(base, time.Now()); e != nil {
		t.Fatal(e)
	}
	changes := []func(*model.Command){func(c *model.Command) { c.ID = "" }, func(c *model.Command) { c.Timeout = 0 }, func(c *model.Command) { c.RequestedAt = time.Now().Add(-10 * time.Minute) }, func(c *model.Command) { c.Interface.Operation = "shell.exec" }, func(c *model.Command) { c.Service = &model.ServiceAction{Service: "sshd", Operation: "restart"} }, func(c *model.Command) { c.Initiator = "trusted_ai" }}
	for _, change := range changes {
		c := command()
		change(&c)
		if Validate(c, time.Now()) == nil {
			t.Fatal("invalid accepted")
		}
	}
}
func TestProtectedAliasAndDefaultDeny(t *testing.T) {
	c := command()
	if !errors.Is((Policy{}).Check(c), ErrPolicy) {
		t.Fatal("default not deny")
	}
	p := Policy{Enabled: true, ProtectedInterfaces: []string{"mgmt"}}
	if !errors.Is(p.Check(c, "mgmt"), ErrProtected) {
		t.Fatal("resolved alias not protected")
	}
	if p.Check(c) != nil {
		t.Fatal("unprotected denied")
	}
	p.ProtectedDevices = []string{"router"}
	if !errors.Is(p.Check(c), ErrProtected) {
		t.Fatal("device unprotected")
	}
}
func TestCapabilityAndP0CannotMutate(t *testing.T) {
	c := command()
	caps := []model.Capability{{Name: "interfaces.disable", Supported: true}}
	if !Supported(caps, RequiredCapability(c)) || Supported(caps, "interfaces.enable") {
		t.Fatal("granularity lost")
	}
	r := RejectUnsupported(c, []model.Device{{ID: "router", Capabilities: caps}}, Policy{Enabled: true}, time.Now())
	if r.Status != "unsupported" || r.Changed {
		t.Fatal("P0 allowed mutation")
	}
}
func TestProtectedMACCanonicalForms(t *testing.T) {
	c := command()
	for _, protected := range []string{"02:aB:00:00:00:01", "02ab.0000.0001", "02-AB-00-00-00-01"} {
		p := Policy{Enabled: true, ProtectedInterfaces: []string{protected}}
		if !errors.Is(p.Check(c, "02:ab:00:00:00:01"), ErrProtected) {
			t.Fatal("MAC alias bypass", protected)
		}
	}
}
