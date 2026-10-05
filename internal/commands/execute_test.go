package commands

import (
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"context"
	"testing"
	"time"
)

type managedFixture struct {
	writes, resolutions int
	supported           bool
	verification        model.InterfaceVerification
	err                 error
}

func (f *managedFixture) Probe(context.Context) (model.Device, error)     { return model.Device{}, nil }
func (f *managedFixture) Inventory(context.Context) (model.Device, error) { return model.Device{}, nil }
func (f *managedFixture) Collect(context.Context) (model.TelemetrySample, error) {
	return model.TelemetrySample{}, nil
}
func (f *managedFixture) Topology(context.Context) (model.TopologyUpdate, error) {
	return model.TopologyUpdate{}, nil
}
func (f *managedFixture) Close() error { return nil }
func (f *managedFixture) Capabilities() []model.Capability {
	return []model.Capability{{Name: "interfaces.disable", Supported: f.supported}, {Name: "interfaces.enable", Supported: f.supported}}
}
func (f *managedFixture) ResolveInterface(context.Context, string) (driver.ResolvedInterface, error) {
	f.resolutions++
	return driver.ResolvedInterface{Interface: model.Interface{ID: "*1", Name: "renamed-management"}, Aliases: []string{"*1", "ether1", "renamed-management", "02:00:00:00:00:01", "bridge"}}, nil
}
func (f *managedFixture) SetInterface(context.Context, driver.ResolvedInterface, bool) (model.InterfaceVerification, error) {
	f.writes++
	return f.verification, f.err
}
func TestTypedExecutorAdmissionAndCanonicalProtection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Policy
		cap    bool
		target string
		mutate func(*model.Command)
		status string
	}{
		{"policy", Policy{}, true, "router", nil, "rejected"},
		{"capability", Policy{Enabled: true}, false, "router", nil, "unsupported"},
		{"target", Policy{Enabled: true}, true, "other", nil, "rejected"},
		{"device", Policy{Enabled: true, ProtectedDevices: []string{"router"}}, true, "router", nil, "rejected"},
		{"canonical ID", Policy{Enabled: true, ProtectedInterfaces: []string{"*1"}}, true, "router", nil, "rejected"},
		{"default name", Policy{Enabled: true, ProtectedInterfaces: []string{"ether1"}}, true, "router", nil, "rejected"},
		{"dependency", Policy{Enabled: true, ProtectedInterfaces: []string{"bridge"}}, true, "router", nil, "rejected"},
		{"expired", Policy{Enabled: true}, true, "router", func(c *model.Command) { c.RequestedAt = time.Now().Add(-2 * time.Second) }, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := command()
			f := &managedFixture{supported: tc.cap}
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			r := Execute(context.Background(), c, model.Device{ID: tc.target}, f, tc.policy, time.Now())
			if r.Status != tc.status || f.writes != 0 || r.Changed {
				t.Fatal(r, f.writes)
			}
		})
	}
}
func TestExecutorVerificationAndUncertain(t *testing.T) {
	for _, tc := range []struct {
		v         model.InterfaceVerification
		err       error
		status    string
		uncertain bool
	}{
		{model.InterfaceVerification{Verified: true, Changed: true}, nil, "success", false},
		{model.InterfaceVerification{Verified: false}, nil, "failed", false},
		{model.InterfaceVerification{Uncertain: true}, driver.ErrUncertain, "timeout", true},
	} {
		c := command()
		f := &managedFixture{supported: true, verification: tc.v, err: tc.err}
		r := Execute(context.Background(), c, model.Device{ID: "router"}, f, Policy{Enabled: true}, time.Now())
		if r.Status != tc.status || r.Uncertain != tc.uncertain || f.writes != 1 || r.Verification == nil {
			t.Fatal(r)
		}
	}
}
func TestBoundedCommandDeduplication(t *testing.T) {
	d := NewDeduplicator(2)
	c := command()
	c.IdempotencyKey = "once"
	if r, e := d.Begin(c); r != nil || e != nil {
		t.Fatal(e)
	}
	if _, e := d.Begin(c); e == nil {
		t.Fatal("in-flight duplicate admitted")
	}
	d.Complete(c, model.CommandResult{CommandID: c.ID, Status: "success", Changed: true})
	copy := c
	copy.ID = "new-id"
	r, e := d.Begin(copy)
	if e != nil || r == nil || r.CommandID != "new-id" || !r.Changed {
		t.Fatal(r, e)
	}
	copy.Interface = &model.InterfaceAction{InterfaceID: "ether9", Operation: "disable"}
	if _, e = d.Begin(copy); e == nil {
		t.Fatal("conflicting identity admitted")
	}
	for _, id := range []string{"second", "third"} {
		c := command()
		c.ID = id
		if _, e = d.Begin(c); e != nil {
			t.Fatal(e)
		}
		d.Complete(c, model.CommandResult{CommandID: id})
	}
	if len(d.order) != 2 || len(d.entries) > 4 {
		t.Fatal("cache unbounded")
	}
}
