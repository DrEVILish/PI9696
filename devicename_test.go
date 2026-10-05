package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A controller's rename arrives as the fork's one-line request file and is
// adopted as the unit's own name (persisted, like a WebUI rename); an
// empty line is a reset to the factory name; the file is consumed either
// way, and an invalid name changes nothing.
func TestControllerRenameAdoptsRequestedName(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t) // restores deviceName
	quiesceInfernoWorker(t)
	orig := infernoNameRequestPath
	infernoNameRequestPath = filepath.Join(t.TempDir(), "name-request")
	t.Cleanup(func() {
		infernoNameRequestPath = orig
		quiesceInfernoWorker(t) // the restart the rename queued
	})
	request := func(content string) string {
		t.Helper()
		if err := os.WriteFile(infernoNameRequestPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got := applyControllerRename()
		if _, err := os.Stat(infernoNameRequestPath); !os.IsNotExist(err) {
			t.Fatalf("request %q was not consumed", content)
		}
		return got
	}
	name := func() string {
		mutex.Lock()
		defer mutex.Unlock()
		return deviceName
	}
	mutex.Lock()
	deviceName = "PI9696"
	mutex.Unlock()

	if got := request("Stage-Left\n"); got != "Stage-Left" || name() != "Stage-Left" {
		t.Fatalf("rename: adopted %q, device name %q", got, name())
	}
	loadPersistedConfig()
	if name() != "Stage-Left" {
		t.Fatalf("the controller's name was not persisted: %q", name())
	}
	if got := request("Stage-Left\n"); got != "" {
		t.Errorf("same name again should be a no-op, adopted %q", got)
	}
	if got := request("bad name!\n"); got != "" || name() != "Stage-Left" {
		t.Errorf("invalid name: adopted %q, device name %q", got, name())
	}
	if got := request("\n"); got != defaultDeviceName || name() != defaultDeviceName {
		t.Errorf("reset: adopted %q, device name %q", got, name())
	}
	if got := applyControllerRename(); got != "" {
		t.Errorf("no request pending, adopted %q", got)
	}
}
