package config

import "testing"

func TestQueueMaximumPayloadPreservesMemoryCompatibility(t *testing.T) {
	for _, mode := range []string{"memory", "durable"} {
		t.Run(mode, func(t *testing.T) {
			c := Defaults()
			c.Security.Mode = "development"
			c.Queue.Mode = mode
			c.Queue.MaxBytes = 256 << 20
			c.Queue.MaxStorageBytes = 0
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := c.Queue.WithDefaults().MaxStorageBytes; got != 512<<20 {
				t.Fatalf("physical file default = %d", got)
			}
		})
	}
}
