package openwrt

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestOnlyFixedTypedArguments(t *testing.T) {
	for _, tc := range []struct {
		op   Operation
		args []string
	}{
		{Board, []string{"-S", "call", "system", "board", "{}"}},
		{Networks, []string{"-S", "call", "network.interface", "dump", "{}"}},
		{Services, []string{"-S", "call", "service", "list", `{"verbose":false}`}},
		{Firewall, []string{"-S", "call", "uci", "get", `{"config":"firewall"}`}},
	} {
		args, err := arguments(tc.op)
		if err != nil || !reflect.DeepEqual(args, tc.args) {
			t.Fatalf("op %v = %v, %v", tc.op, args, err)
		}
	}
	if _, err := arguments(Operation(255)); err == nil {
		t.Fatal("unsupported operation accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (execRunner{}).Run(ctx, Board); !errors.Is(err, context.Canceled) {
		t.Fatalf("command cancellation = %v", err)
	}
}

func TestProcessOutputRemainsBounded(t *testing.T) {
	b := limitedBuffer{limit: 16}
	for range 1000 {
		n, err := b.Write(make([]byte, 1024))
		if err != nil || n != 1024 || b.Len() > 16 {
			t.Fatalf("buffer len=%d, written=%d, err=%v", b.Len(), n, err)
		}
	}
	if !b.exceeded || b.Len() != 16 {
		t.Fatal("output limit not recorded")
	}
}
