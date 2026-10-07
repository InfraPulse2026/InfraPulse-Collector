package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"InfraPulseAgent/internal/platform/timesync"
)

type ntpFixture struct {
	provider      *TimeSync
	services      map[ntpOperation][]byte
	operations    []ntpOperation
	system        []string
	link          []string
	fallback      []string
	sources       []string
	synchronized  bool
	restartError  error
	restartFails  int
	onRestartFail context.CancelFunc
}

func newNTPFixture(t *testing.T, selected ntpOperation) *ntpFixture {
	t.Helper()
	f := &ntpFixture{services: make(map[ntpOperation][]byte), synchronized: true}
	f.provider = &TimeSync{root: t.TempDir(), runner: f}
	units := map[ntpOperation]string{ntpChronyService: "chrony.service", ntpChronydService: "chronyd.service", ntpTimesyncdService: "systemd-timesyncd.service", ntpOtherService: "ntp.service", ntpNtpdService: "ntpd.service", ntpOpenntpdService: "openntpd.service", ntpNtpsecService: "ntpsec.service"}
	for op, unit := range units {
		f.services[op] = []byte("Id=" + unit + "\nLoadState=not-found\nActiveState=inactive\nMainPID=0\n")
	}
	if selected != 0 {
		f.service(selected, true, true, 101)
	}
	return f
}

func (f *ntpFixture) service(op ntpOperation, available, active bool, pid int) {
	props, _ := ntpProperties(f.services[op])
	load, state := "not-found", "inactive"
	if available {
		load = "loaded"
	}
	if active {
		state = "active"
	}
	f.services[op] = fmt.Appendf(nil, "Id=%s\nLoadState=%s\nActiveState=%s\nMainPID=%d\n", props["Id"], load, state, pid)
}

func (f *ntpFixture) write(t *testing.T, logical, text string) {
	t.Helper()
	file := f.provider.physical(logical)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *ntpFixture) chrony(t *testing.T, config, text string) {
	t.Helper()
	f.write(t, config, text)
	f.write(t, "/proc/101/cmdline", "/usr/sbin/chronyd\x00-f\x00"+config+"\x00-u\x00chrony\x00")
}

func (f *ntpFixture) Run(ctx context.Context, op ntpOperation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.operations = append(f.operations, op)
	if data, ok := f.services[op]; ok {
		return data, nil
	}
	switch op {
	case ntpTracking:
		leap := "Normal"
		if !f.synchronized {
			leap = "Not synchronised"
		}
		return []byte("0A0A0005,10.10.0.5,3,1791370000,0,0,0,0,0,0,0,0,1," + leap + "\n"), nil
	case ntpSources:
		var b strings.Builder
		for _, host := range f.sources {
			fmt.Fprintf(&b, "^,*,%s,2,6,377,1,0,0,0\n", host)
		}
		return []byte(b.String()), nil
	case ntpTimesyncStatus:
		return []byte("SystemNTPServers=" + strings.Join(f.system, " ") + "\nLinkNTPServers=" + strings.Join(f.link, " ") + "\nFallbackNTPServers=" + strings.Join(f.fallback, " ") + "\n"), nil
	case ntpClockStatus:
		if f.synchronized {
			return []byte("NTPSynchronized=yes\n"), nil
		}
		return []byte("NTPSynchronized=no\n"), nil
	case ntpRestartChrony, ntpRestartChronyd, ntpRestartTimesyncd:
		if f.restartFails > 0 {
			f.restartFails--
			if f.onRestartFail != nil {
				f.onRestartFail()
			}
			return nil, f.restartError
		}
		if op == ntpRestartTimesyncd {
			data, err := os.ReadFile(f.provider.physical(ntpManagedTimesyncd))
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil // rollback recovery to previously absent fragment
			}
			if err != nil {
				return nil, err
			}
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "NTP=") {
					f.system = strings.Fields(strings.TrimPrefix(line, "NTP="))
				}
				if strings.HasPrefix(line, "FallbackNTP=") {
					f.fallback = strings.Fields(strings.TrimPrefix(line, "FallbackNTP="))
				}
			}
		} else {
			f.sources = nil
			err := filepath.WalkDir(f.provider.physical("/etc"), func(file string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				for _, line := range strings.Split(string(data), "\n") {
					fields := strings.Fields(line)
					if len(fields) >= 2 && fields[0] == "server" {
						f.sources = append(f.sources, fields[1])
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	return nil, errors.New("unexpected fixture operation")
}

func (f *ntpFixture) restarts() int {
	count := 0
	for _, op := range f.operations {
		if op >= ntpRestartChrony {
			count++
		}
	}
	return count
}

var corporateNTP = timesync.Settings{Enabled: true, Servers: []string{"10.10.0.5", "ntp2.company.local"}}

func TestNTPDisabledDoesNotReadOrChangeOS(t *testing.T) {
	f := newNTPFixture(t, 0)
	for _, method := range []func(context.Context, timesync.Settings) (timesync.Status, error){f.provider.Status, f.provider.Apply} {
		s, err := method(context.Background(), timesync.Settings{})
		if err != nil || s.Enabled || len(f.operations) != 0 {
			t.Fatal(s, err, f.operations)
		}
	}
}

func TestNTPProviderSelection(t *testing.T) {
	for _, test := range []struct {
		name     string
		chrony   bool
		active   bool
		timesync bool
		want     string
	}{
		{"chrony preferred", true, true, false, "chrony"},
		{"timesyncd fallback", false, false, true, "systemd-timesyncd"},
		{"inactive chrony does not displace active timesyncd", true, false, true, "systemd-timesyncd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNTPFixture(t, 0)
			f.service(ntpChronyService, test.chrony, test.active, 101)
			f.service(ntpTimesyncdService, true, test.timesync, 102)
			service, err := f.provider.selectService(context.Background())
			if err != nil || ntpProviderName(service) != test.want {
				t.Fatal(service, err)
			}
		})
	}
}

func TestNTPUnavailableInactiveAndConflictingServices(t *testing.T) {
	for _, scenario := range []string{"unavailable", "inactive", "timesyncd conflict", "ntpd conflict", "two chrony processes"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNTPFixture(t, 0)
			if scenario != "unavailable" {
				f.service(ntpChronyService, true, scenario != "inactive", 101)
			}
			if scenario == "timesyncd conflict" {
				f.service(ntpTimesyncdService, true, true, 102)
			}
			if scenario == "ntpd conflict" {
				f.service(ntpNtpdService, true, true, 103)
			}
			if scenario == "two chrony processes" {
				f.service(ntpChronydService, true, true, 104)
			}
			s, err := f.provider.Apply(context.Background(), corporateNTP)
			if err == nil || s.Diagnostic == "" || f.restarts() != 0 {
				t.Fatal(s, err, f.operations)
			}
		})
	}
}

func TestNTPTimesyncdApplyIdempotentAndPendingSync(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	f.synchronized = false
	s, err := f.provider.Apply(context.Background(), corporateNTP)
	if err != nil || !s.Changed || !s.Configured || s.Synchronized == nil || *s.Synchronized || f.restarts() != 1 {
		t.Fatal(s, err, f.operations)
	}
	data, err := os.ReadFile(f.provider.physical(ntpManagedTimesyncd))
	if err != nil || !bytes.HasPrefix(data, []byte(ntpManagedMarker)) || !strings.Contains(string(data), "NTP=\nNTP=10.10.0.5 ntp2.company.local\nFallbackNTP=\nFallbackNTP=10.10.0.5 ntp2.company.local\n") {
		t.Fatal(string(data), err)
	}
	s, err = f.provider.Apply(context.Background(), corporateNTP)
	if err != nil || s.Changed || f.restarts() != 1 {
		t.Fatal(s, err, f.operations)
	}
}

func TestNTPTimesyncdCorrectAdministratorConfigIsNoOp(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	f.system = slices.Clone(corporateNTP.Servers)
	s, err := f.provider.Apply(context.Background(), corporateNTP)
	if err != nil || !s.Configured || s.Changed || f.restarts() != 0 {
		t.Fatal(s, err)
	}
	if _, err := os.Stat(f.provider.physical(ntpManagedTimesyncd)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("correct administrator configuration created a managed file", err)
	}
}

func TestNTPTimesyncdPersistentOldContentIsUpdatedEvenWhenRuntimeMatches(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	f.system = slices.Clone(corporateNTP.Servers)
	f.write(t, ntpManagedTimesyncd, ntpManagedMarker+"[Time]\nNTP=10.10.0.6\n")
	s, err := f.provider.Apply(context.Background(), corporateNTP)
	if err != nil || !s.Changed || f.restarts() != 1 {
		t.Fatal(s, err, f.operations)
	}
}

func TestNTPTimesyncdExtraLinkSourcesRefused(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	f.link = []string{"10.20.0.5"}
	_, err := f.provider.Apply(context.Background(), corporateNTP)
	if err == nil || !strings.Contains(err.Error(), "per-link/DHCP") || f.restarts() != 0 {
		t.Fatal(err)
	}
}

func TestNTPManagedFileOwnershipAndRestartRollback(t *testing.T) {
	for _, scenario := range []string{"unowned", "restart failure with previous file", "restart failure new file"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNTPFixture(t, ntpTimesyncdService)
			previous := "[Time]\nNTP=10.10.0.6\n"
			if scenario == "restart failure with previous file" {
				previous = ntpManagedMarker + previous
			}
			if scenario != "restart failure new file" {
				f.write(t, ntpManagedTimesyncd, previous)
			}
			if scenario != "unowned" {
				f.restartError, f.restartFails = os.ErrPermission, 1
			}
			s, err := f.provider.Apply(context.Background(), corporateNTP)
			if err == nil || s.Changed || s.Diagnostic == "" {
				t.Fatal(s, err)
			}
			data, readErr := os.ReadFile(f.provider.physical(ntpManagedTimesyncd))
			if scenario == "restart failure new file" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal("new fragment was not rolled back", string(data), readErr)
				}
			} else if readErr != nil || string(data) != previous {
				t.Fatal("previous file not preserved", string(data), readErr)
			}
			if scenario == "unowned" && f.restarts() != 0 {
				t.Fatal("unowned fragment restarted service")
			}
		})
	}
}

func TestNTPUnchangedFragmentRuntimeMismatchDoesNotRestartRepeatedly(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	f.write(t, ntpManagedTimesyncd, ntpManagedMarker+"[Time]\nNTP=\nNTP=10.10.0.5 ntp2.company.local\nFallbackNTP=\nFallbackNTP=10.10.0.5 ntp2.company.local\n")
	for range 2 {
		_, err := f.provider.Apply(context.Background(), corporateNTP)
		if err == nil || !strings.Contains(err.Error(), "managed file is unchanged") || f.restarts() != 0 {
			t.Fatal(err, f.operations)
		}
	}
}

func TestNTPChronyLayoutsAndIdempotency(t *testing.T) {
	for _, test := range []struct{ name, config, directive, managed string }{
		{"redhat confdir", "/etc/chrony.conf", "confdir /etc/chrony.d", "/etc/chrony.d/90-infrapulse.conf"},
		{"debian sourcedir", "/etc/chrony/chrony.conf", "sourcedir /etc/chrony/sources.d", "/etc/chrony/sources.d/90-infrapulse.sources"},
		{"include", "/etc/chrony/chrony.conf", "include /etc/chrony/conf.d/*.conf", "/etc/chrony/conf.d/90-infrapulse.conf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNTPFixture(t, ntpChronyService)
			text := "driftfile /var/lib/chrony/drift\n" + test.directive + "\nmakestep 1.0 3\n"
			f.chrony(t, test.config, text)
			if test.name == "include" {
				f.write(t, "/etc/chrony/conf.d/admin.conf", "# administrator settings\nrtcsync\n")
			}
			s, err := f.provider.Apply(context.Background(), corporateNTP)
			if err != nil || !s.Changed || !s.Configured || s.ManagedFile != test.managed || f.restarts() != 1 {
				t.Fatal(s, err, f.operations)
			}
			main, _ := os.ReadFile(f.provider.physical(test.config))
			if string(main) != text {
				t.Fatal("administrator main file modified", string(main))
			}
			s, err = f.provider.Apply(context.Background(), corporateNTP)
			if err != nil || s.Changed || f.restarts() != 1 {
				t.Fatal(s, err)
			}
		})
	}
}

func TestNTPChronyUnsafeConfigurationRequiresAdministrator(t *testing.T) {
	for _, directive := range []string{"server 10.20.0.5 iburst\nconfdir /etc/chrony.d", "sourcedir /run/chrony-dhcp", "local stratum 10", "refclock SHM 0", "confdir /etc/first /etc/second", "include /etc/empty/*.conf", "confdir '/etc/quoted'", "driftfile /var/lib/chrony/drift"} {
		t.Run(strings.ReplaceAll(directive, "/", "-"), func(t *testing.T) {
			f := newNTPFixture(t, ntpChronyService)
			f.chrony(t, "/etc/chrony.conf", directive+"\n")
			s, err := f.provider.Apply(context.Background(), corporateNTP)
			if err == nil || !strings.Contains(err.Error(), "setup_required") || f.restarts() != 0 || s.Changed {
				t.Fatal(s, err)
			}
		})
	}
}

func TestNTPChronyManagedServersMayBeReplaced(t *testing.T) {
	f := newNTPFixture(t, ntpChronyService)
	f.chrony(t, "/etc/chrony.conf", "confdir /etc/chrony.d\n")
	f.write(t, "/etc/chrony.d/90-infrapulse.conf", ntpManagedMarker+"server 10.10.0.6 iburst\n")
	s, err := f.provider.Apply(context.Background(), corporateNTP)
	if err != nil || !s.Changed || !s.Configured || f.restarts() != 1 {
		t.Fatal(s, err)
	}
}

func TestNTPChronyCorrectAdministratorSourcesDoNotNeedManagedFragment(t *testing.T) {
	f := newNTPFixture(t, ntpChronydService)
	f.chrony(t, "/etc/chrony.conf", "server 10.10.0.5 iburst\nserver ntp2.company.local iburst\nrtcsync\n")
	f.sources = slices.Clone(corporateNTP.Servers)
	s, err := f.provider.Apply(context.Background(), corporateNTP)
	if err != nil || !s.Configured || s.Changed || s.ManagedFile != "" || f.restarts() != 0 {
		t.Fatal(s, err)
	}
}

func TestNTPNonRegularConfigurationRefusedWithoutBlocking(t *testing.T) {
	for _, chrony := range []bool{false, true} {
		f := newNTPFixture(t, ntpTimesyncdService)
		logical := ntpManagedTimesyncd
		if chrony {
			f = newNTPFixture(t, ntpChronyService)
			f.chrony(t, "/etc/chrony.conf", "confdir /etc/chrony.d\n")
			logical = "/etc/chrony.conf"
			if err := os.Remove(f.provider.physical(logical)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(f.provider.physical(logical), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := f.provider.Apply(context.Background(), corporateNTP); err == nil || !strings.Contains(err.Error(), "not a regular file") || f.restarts() != 0 {
			t.Fatal(err)
		}
	}
}

func TestNTPFailedRestartRestoresOwnedFileAfterCallerCancellation(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	old := ntpManagedMarker + "[Time]\nNTP=10.10.0.6\n"
	f.write(t, ntpManagedTimesyncd, old)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.restartFails, f.restartError, f.onRestartFail = 1, context.Canceled, cancel
	if _, err := f.provider.Apply(ctx, corporateNTP); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.provider.physical(ntpManagedTimesyncd))
	if err != nil || string(data) != old || f.restarts() != 2 {
		t.Fatal("cancelled apply did not restore previous file and recover service", string(data), err, f.operations)
	}
}

func TestNTPChronyRejectsClockDisabledAndAmbiguousDefaults(t *testing.T) {
	f := newNTPFixture(t, ntpChronyService)
	f.chrony(t, "/etc/chrony.conf", "confdir /etc/chrony.d\n")
	f.write(t, "/proc/101/cmdline", "/usr/sbin/chronyd\x00-f\x00/etc/chrony.conf\x00-x\x00")
	if _, err := f.provider.Apply(context.Background(), corporateNTP); err == nil || !strings.Contains(err.Error(), "-x") {
		t.Fatal(err)
	}
	f.write(t, "/proc/101/cmdline", "/usr/sbin/chronyd\x00")
	f.write(t, "/etc/chrony/chrony.conf", "confdir /etc/chrony.d\n")
	if _, err := f.provider.Apply(context.Background(), corporateNTP); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal(err)
	}
}

func TestNTPStructuredStatusParsing(t *testing.T) {
	for _, malformed := range []string{"", "not properties", "MainPID=1\nMainPID=2\n"} {
		if _, err := ntpProperties([]byte(malformed)); err == nil {
			t.Fatal("malformed properties accepted", malformed)
		}
	}
	for _, tracking := range []string{"", "Reference ID : 10.10.0.5", "a,b,c"} {
		if _, err := ntpParseTracking([]byte(tracking)); err == nil {
			t.Fatal("malformed tracking accepted", tracking)
		}
	}
	if err := ntpCheckSources([]byte("^,*,10.20.0.5,2,6,377,1,0,0,0\n"), corporateNTP.Servers); err == nil {
		t.Fatal("runtime source outside corporate NTP accepted")
	}
}

func TestNTPContextAndFixedCommands(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.provider.Apply(ctx, corporateNTP); !errors.Is(err, context.Canceled) || len(f.operations) != 0 {
		t.Fatal(err, f.operations)
	}
	for op := ntpChronyService; op <= ntpRestartTimesyncd; op++ {
		name, args, err := ntpCommand(op)
		if err != nil || name == "" || len(args) == 0 || strings.Contains(strings.Join(args, " "), "10.10.0.5") {
			t.Fatal(op, name, args, err)
		}
		if op >= ntpRestartChrony && !slices.Contains(args, "--no-ask-password") {
			t.Fatal("restart can invoke interactive authentication")
		}
	}
	if _, _, err := ntpCommand(255); err == nil {
		t.Fatal("arbitrary operation accepted")
	}
	b := ntpOutput{limit: 4}
	if n, err := b.Write([]byte("123456")); n != 6 || err != nil || b.Len() != 4 || !b.exceeded {
		t.Fatal(n, err, b)
	}
}

func TestNTPManagedSymlinkRefused(t *testing.T) {
	f := newNTPFixture(t, ntpTimesyncdService)
	f.write(t, "/etc/admin.conf", "administrator data\n")
	link := f.provider.physical(ntpManagedTimesyncd)
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.provider.physical("/etc/admin.conf"), link); err != nil {
		t.Skip("symlink creation unavailable on test OS:", err)
	}
	_, err := f.provider.Apply(context.Background(), corporateNTP)
	if err == nil || !strings.Contains(err.Error(), "symlink") || f.restarts() != 0 {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.provider.physical("/etc/admin.conf"))
	if string(data) != "administrator data\n" {
		t.Fatal("symlink target changed")
	}
}
