package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useFakeAvahiPublish installs an avahi-publish that records its argv and
// then becomes a long-lived child (exec, so a kill leaves nothing behind).
func useFakeAvahiPublish(t *testing.T) string {
	t.Helper()
	args := filepath.Join(t.TempDir(), "args")
	fakeExecutable(t, "avahi-publish", "#!/bin/sh\necho \"$@\" >> "+args+"\nexec sleep 300\n")
	mutex.Lock()
	origDev, origHost := deviceName, mdnsSystemHost
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		stopMDNSAddrLocked()
		deviceName, mdnsSystemHost = origDev, origHost
		mutex.Unlock()
	})
	mutex.Lock()
	mdnsSystemHost = func() string { return "pi9696" }
	mutex.Unlock()
	return args
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(data) > 0 || time.Now().After(deadline) {
			if len(data) == 0 {
				return nil
			}
			return lines
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// <device>.local gets an address record, re-published only when the name
// or the address changes, and the QR host follows it.
func TestMDNSAddrPublishesTheDeviceName(t *testing.T) {
	args := useFakeAvahiPublish(t)
	mutex.Lock()
	deviceName = "PI9696-test"
	mutex.Unlock()

	if !mdnsAddrTick("192.0.2.69") {
		t.Fatal("address not published")
	}
	if got := readArgs(t, args); len(got) != 1 || got[0] != "-a -R pi9696-test.local 192.0.2.69" {
		t.Fatalf("avahi-publish args = %q", got)
	}
	mutex.Lock()
	host := mdnsURLHostLocked()
	mutex.Unlock()
	if host != "pi9696-test.local" {
		t.Errorf("QR host = %q, want pi9696-test.local", host)
	}
	mdnsAddrTick("192.0.2.69") // unchanged: no second publish
	mdnsAddrTick("192.0.2.70") // new address: re-published
	time.Sleep(100 * time.Millisecond)
	if got := readArgs(t, args); len(got) != 2 || got[1] != "-a -R pi9696-test.local 192.0.2.70" {
		t.Fatalf("after an address change, args = %q", got)
	}
}

// The system hostname is avahi-daemon's own: never published again, and
// the QR uses it as is. Without avahi the QR falls back to the IP.
func TestMDNSAddrSystemHostAndFallback(t *testing.T) {
	args := useFakeAvahiPublish(t)
	mutex.Lock()
	deviceName = "PI9696"
	mutex.Unlock()
	if !mdnsAddrTick("192.0.2.69") {
		t.Error("the system hostname should count as resolving")
	}
	if got := readArgs(t, args); got != nil {
		t.Errorf("published the system hostname again: %q", got)
	}
	mutex.Lock()
	host := mdnsURLHostLocked()
	deviceName = "Other"
	mutex.Unlock()
	if host != "pi9696.local" {
		t.Errorf("QR host = %q, want pi9696.local", host)
	}
	t.Setenv("PATH", t.TempDir()) // no avahi
	if mdnsAddrTick("192.0.2.69") {
		t.Error("reported resolving without avahi")
	}
	mutex.Lock()
	host = mdnsURLHostLocked()
	mutex.Unlock()
	if host != "" {
		t.Errorf("QR host = %q without avahi, want the IP fallback", host)
	}
}
