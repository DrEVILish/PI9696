package main

import (
	"os"
	"os/exec"
	"strings"
)

// The service advertisement (mdnsTick) does not make <device>.local
// resolve: avahi answers only for the system hostname, so on the test unit
// pi9696.local resolved while pi9696-test.local timed out. mdnsAddrTick
// publishes an address record for <device>.local -> the unit's IPv4 with
// avahi-publish -a -R (-R: no reverse record, which the system hostname
// already owns), re-published when the device name or the address changes.

// mdnsAddrCmd is the live avahi-publish -a child and mdnsAddrKey the
// "<host> <ip>" it publishes; guarded by the app mutex, killed by
// gracefulShutdown.
var mdnsAddrCmd *exec.Cmd
var mdnsAddrKey string

// mdnsSystemHost is the system hostname, which avahi-daemon already
// answers for as <hostname>.local. A var so tests can pin it.
var mdnsSystemHost = func() string {
	h, _ := os.Hostname()
	return strings.ToLower(strings.TrimSuffix(h, ".local"))
}

// mdnsAddrTick keeps <device>.local resolving to ip. Returns whether the
// name resolves (published by us, or the system hostname itself).
func mdnsAddrTick(ip string) bool {
	mutex.Lock()
	defer mutex.Unlock()
	host := sanitizeMDNSHost(deviceName)
	if host == mdnsSystemHost() {
		// avahi-daemon owns this name; publishing it again collides.
		stopMDNSAddrLocked()
		return true
	}
	if ip == "" {
		stopMDNSAddrLocked()
		return false
	}
	key := host + " " + ip
	if key == mdnsAddrKey && mdnsAddrCmd != nil {
		return true
	}
	stopMDNSAddrLocked()
	c := exec.Command("avahi-publish", "-a", "-R", host+".local", ip)
	if err := c.Start(); err != nil {
		logErrorf("mdns: avahi address publish failed: %v", err)
		return false
	}
	mdnsAddrCmd, mdnsAddrKey = c, key
	logInfof("mdns: %s.local -> %s", host, ip)
	go func() {
		c.Wait() // sole Wait()er; stopMDNSAddrLocked only kills
		mutex.Lock()
		if mdnsAddrCmd == c {
			mdnsAddrCmd, mdnsAddrKey = nil, ""
		}
		mutex.Unlock()
	}()
	return true
}

// stopMDNSAddrLocked withdraws the address record. Caller holds the mutex.
func stopMDNSAddrLocked() {
	if mdnsAddrCmd != nil {
		mdnsAddrCmd.Process.Kill()
		mdnsAddrCmd, mdnsAddrKey = nil, ""
	}
}

// mdnsURLHostLocked is the host for links to this unit (the access QR):
// <device>.local while that name resolves, else "" (callers use the IP).
// Caller holds the mutex.
func mdnsURLHostLocked() string {
	host := sanitizeMDNSHost(deviceName)
	if host == mdnsSystemHost() || (mdnsAddrCmd != nil && strings.HasPrefix(mdnsAddrKey, host+" ")) {
		return host + ".local"
	}
	return ""
}
