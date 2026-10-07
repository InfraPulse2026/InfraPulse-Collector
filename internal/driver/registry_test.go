package driver

import (
	"errors"
	"testing"

	"InfraPulseAgent/internal/model"
)

type limitedAdapter struct{ name string }

func (a *limitedAdapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: a.name, Supported: true}}
}
func (*limitedAdapter) Close() error { return nil }

func TestRegistryConstructsAdapterWithoutUnsupportedReaders(t *testing.T) {
	r := NewRegistry[string]()
	if err := r.Register("redfish", func(input string) (Adapter, error) { return &limitedAdapter{name: input}, nil }); err != nil {
		t.Fatal(err)
	}
	adapter, err := r.Create("redfish", "inventory.read")
	if err != nil || adapter.Capabilities()[0].Name != "inventory.read" {
		t.Fatal("registered constructor did not receive typed input", err)
	}
	if _, full := adapter.(Driver); full {
		t.Fatal("registry required or fabricated a full driver")
	}
	if _, topology := adapter.(TopologyReader); topology {
		t.Fatal("registry fabricated an unsupported reader")
	}
}

func TestRegistryRejectsDuplicateMissingAndInvalidConstructors(t *testing.T) {
	r := NewRegistry[int]()
	constructor := func(int) (Adapter, error) { return &limitedAdapter{}, nil }
	if err := r.Register("snmp", constructor); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{r.Register("snmp", constructor), r.Register("", constructor), r.Register("routeros", nil)} {
		if err == nil {
			t.Fatal("accepted invalid or duplicate adapter registration")
		}
	}
	if _, err := r.Create("unknown", 0); err == nil {
		t.Fatal("unknown protocol selected an adapter")
	}
	want := errors.New("constructor unavailable")
	if err := r.Register("failed", func(int) (Adapter, error) { return nil, want }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create("failed", 0); !errors.Is(err, want) {
		t.Fatal("constructor error was lost", err)
	}
	if err := r.Register("empty", func(int) (Adapter, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create("empty", 0); err == nil {
		t.Fatal("accepted missing adapter")
	}
}
