package redfish

import (
	"net/url"
	"strings"
	"testing"
)

// The parser is exercised without requests: hostile BMC links must never move
// reads to another origin or out of the fixed Redfish service tree.
func FuzzRedfishResourceReference(f *testing.F) {
	for _, link := range []string{
		"/redfish/v1/Systems/A", "/redfish/v1/Systems?$skip=2",
		"https://bmc.fixture/redfish/v1/Chassis/A", "//foreign.fixture/redfish/v1/Systems",
		"/redfish/v1/%2e%2e/private", "/redfish/v1/Systems%2fprivate",
		"/redfish/v1/Systems?%70assword=private", "/redfish/v1/Chassis/A#/Fans/0",
		"/redfish/v1/Systems?x=%0d%0a", "https://user:secret@bmc.fixture/redfish/v1/Systems",
	} {
		f.Add(link, false)
		f.Add(link, true)
	}
	origin, _ := url.Parse("https://bmc.fixture")
	a := &Adapter{origin: origin}
	f.Fuzz(func(t *testing.T, link string, fragment bool) {
		if len(link) > 4096 {
			t.Skip()
		}
		canonical, err := a.canonical(link, fragment)
		if err != nil {
			return
		}
		u, err := url.Parse(canonical)
		if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || len(canonical) > 2048 {
			t.Fatal("accepted reference escaped its origin or bound")
		}
		if u.Path != "/redfish/v1" && !strings.HasPrefix(u.Path, "/redfish/v1/") {
			t.Fatal("accepted reference escaped service tree")
		}
		for _, part := range strings.Split(u.Path, "/") {
			if part == "." || part == ".." {
				t.Fatal("accepted traversal segment")
			}
		}
		repeat, err := a.canonical(canonical, fragment)
		if err != nil || repeat != canonical {
			t.Fatal("reference canonicalization is not stable")
		}
	})
}
