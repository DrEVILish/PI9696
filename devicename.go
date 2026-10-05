package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Renaming the unit from a network audio controller.
//
// The controller renames the inferno device (ARC 0x1001). The in-process
// inferno can't call back into the app through ALSA, so the fork hands the
// request over as a one-line file at INFERNO_NAME_REQUEST_PATH (fork
// a67a337): the new name, or an empty line for "reset to factory name".
// The app adopts it as its own device name - the same as a rename from the
// WebUI: persisted, shown everywhere, and pushed back out by restarting
// inferno with the new INFERNO_NAME (deferred while a take records or
// plays). Owner decision 2026-10-05: a controller rename updates and
// persists the pi9696 device name.

// infernoNameRequestPath is where the fork writes a controller's rename
// request. A var so tests can point it at a temp dir.
var infernoNameRequestPath = "/var/lib/pi9696/inferno-name-request"

// defaultDeviceName is the unit's factory name, which a controller's
// "reset name" restores.
const defaultDeviceName = "PI9696"

// nameRequestPoll is how often the request file is checked.
const nameRequestPoll = time.Second

// applyControllerRename consumes a pending rename request, if any, and
// reports the name it adopted ("" when there was nothing to do).
func applyControllerRename() string {
	data, err := os.ReadFile(infernoNameRequestPath)
	if err != nil {
		return ""
	}
	// Consume first: a request that cannot be applied must not be retried
	// every second forever.
	if err := os.Remove(infernoNameRequestPath); err != nil {
		logWarnf("controller rename: cannot remove %s: %v", infernoNameRequestPath, err)
	}
	name := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if name == "" {
		name = defaultDeviceName
	}
	// The fork already applied the controllers' rules; this is the app's own
	// (stricter set in the other direction: no spaces/underscores arrive).
	if !isValidDeviceName(name) {
		logWarnf("controller rename: ignoring invalid name %q", name)
		return ""
	}
	mutex.Lock()
	if deviceName == name {
		mutex.Unlock()
		return ""
	}
	old := deviceName
	deviceName = name
	persistConfig()
	showWebNotice("Renamed to " + name + " from a network controller")
	mutex.Unlock()
	logInfof("Device name changed from %q to %q by a network controller", old, name)
	// Re-advertise under the new name (deferred while busy, like a WebUI
	// rename).
	restartInfernoServer()
	return name
}

// nameRequestLoop watches for controller rename requests.
func nameRequestLoop() {
	ticker := time.NewTicker(nameRequestPoll)
	defer ticker.Stop()
	for range ticker.C {
		applyControllerRename()
	}
}

// prepareNameRequestPath makes sure the fork can write the request file.
func prepareNameRequestPath() {
	if err := os.MkdirAll(filepath.Dir(infernoNameRequestPath), 0o755); err != nil {
		logWarnf("controller rename: cannot create %s: %v", filepath.Dir(infernoNameRequestPath), err)
	}
}
