package main

// Dev-only harness for visually checking display rendering via PI9696_SIM,
// without physical hardware. Not part of the shipped app - useful when
// iterating on layout on a dev machine (e.g. x86) that has no OLED/SPI.
// Mirrors the render logic in main.go's renderXxx functions so screenshots
// reflect what the real app actually draws; keep it in sync if you change
// layout there. Run: go run ./cmd/simcheck, then check /tmp/pi9696_shots/.

import (
	"fmt"
	"log"
	"os"

	"pi9696/hardware"
)

func main() {
	os.Setenv("PI9696_SIM", "1")

	hm, err := hardware.NewHardwareManager()
	if err != nil {
		log.Fatalf("init failed: %v", err)
	}
	defer hm.Close()

	shot := func(name string, draw func()) {
		hm.ClearDisplay()
		draw()
		if err := hm.UpdateDisplay(); err != nil {
			log.Printf("update failed for %s: %v", name, err)
			return
		}
		src := os.Getenv("PI9696_SIM_OUT")
		if src == "" {
			src = "/tmp/pi9696_sim_frame.png"
		}
		dst := fmt.Sprintf("/tmp/pi9696_shots/%s.png", name)
		data, _ := os.ReadFile(src)
		os.WriteFile(dst, data, 0644)
		fmt.Println("wrote", dst)
	}

	os.MkdirAll("/tmp/pi9696_shots", 0755)

	statusBar := func() {
		hm.DrawStatusBarWithInferno("WAV 24bit 48kHz 2ch", "4GB [USB]", true)
	}

	shot("idle", func() {
		statusBar()
		hm.DrawCenteredText("~ Standby ~", "idle", 32)
		hm.DrawCenteredText("02:45:12 (456MB) available", "details", 48)
	})

	shot("recording", func() {
		statusBar()
		hm.DrawRecordingStatus("00:05:23", "01:39:49 (387MB)", "Peak: -18.1dB  RMS: -21.1dB")
	})

	shot("playing", func() {
		statusBar()
		hm.DrawPlaybackStatus("00:01:47", "recording_20240131_143022_ch2_48kHz.wav")
	})

	shot("settings_menu", func() {
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Sample Rate →", Value: "48kHz"},
			{Label: "Channel Count →", Value: "2"},
			{Label: "Tag →", Value: "None"},
			{Label: "Prefix →", Value: "Default"},
			{Label: "← Back", Value: ""},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 0 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-16, y, item.Value)
			}
			y += fh
		}
		hm.SwitchToContext("details")
		hm.DrawText(240, 61, "↓")
	})

	shot("logging_menu", func() {
		// Logging submenu picker: active level (Info here) marked with ●,
		// cursor on Debug, as a direct-select list - mirrors renderLoggingMenu.
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Error", Value: " "},
			{Label: "Warn", Value: " "},
			{Label: "Info", Value: "●"},
			{Label: "Debug", Value: " "},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 3 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-16, y, item.Value)
			}
			y += fh
		}
		hm.SwitchToContext("details")
		hm.DrawText(240, 61, "↓")
	})

	shot("settings_menu_scrolled", func() {
		// selectedMenu=10 ("Exit") forces menuScrollOffset=7, showing
		// Restart Inferno / WiFi / Exit with the up arrow visible - checks the
		// up arrow at (240,22) doesn't collide with a right-aligned value.
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Restart Inferno", Value: "Running"},
			{Label: "WiFi →", Value: "off"},
			{Label: "Exit", Value: ""},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 2 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-16, y, item.Value)
			}
			y += fh
		}
		hm.SwitchToContext("details")
		hm.DrawText(240, 22, "↑")
	})

	shot("settings_menu_offset1", func() {
		// Metering (which carries a right-aligned "NdB" value) lands as the
		// top visible row with the up arrow showing - the exact collision this
		// scrollable Settings menu can reach, where a value-bearing row ends
		// up right under the arrow.
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Metering →", Value: "60dB"},
			{Label: "Display →", Value: "70%"},
			{Label: "Logging →", Value: "Error"},
			{Label: "Copy Files →", Value: ""},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 1 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-32, y, item.Value)
			}
			y += fh
		}
		hm.SwitchToContext("details")
		hm.DrawText(240, 22, "↑")
	})

	shot("settings_menu_display_top", func() {
		// The realistic scroll window that starts the current (11-row)
		// Settings list at row 2: Display (value "70%") on top - the row that
		// must not collide with the up arrow - then Logging, Copy Files,
		// System Options. Cursor sits on Logging.
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Display →", Value: "70%"},
			{Label: "Logging →", Value: "Error"},
			{Label: "Copy Files →", Value: ""},
			{Label: "System Options →", Value: ""},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 1 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-32, y, item.Value)
			}
			y += fh
		}
		hm.SwitchToContext("details")
		hm.DrawText(240, 22, "↑")
	})

	shot("display_menu", func() {
		// Display submenu: Brightness row being edited (» prefix) with its
		// right-aligned live 62%, Auto Dim below, then Back - the two
		// press-to-edit rows plus the return row.
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Brightness →", Value: "62%"},
			{Label: "Auto Dim →", Value: "On"},
			{Label: "← Back", Value: ""},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 0 {
				ctx = "selected"
				prefix = "» "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-32, y, item.Value)
			}
			y += fh
		}
	})

	shot("system_options", func() {
		statusBar()
		items := []hardware.MenuItem{
			{Label: "Delete All Recordings", Value: ""},
			{Label: "Format USB Drive", Value: ""},
			{Label: "Shutdown System", Value: ""},
			{Label: "Restart System", Value: ""},
		}
		y := 22
		fh := 13
		for i, item := range items {
			ctx := "menu"
			prefix := "  "
			if i == 1 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			y += fh
		}
		hm.SwitchToContext("details")
		hm.DrawText(240, 61, "↓")
	})

	shot("confirm_delete", func() {
		statusBar()
		hm.DrawConfirmationDialog("CONFIRM DELETE", "Delete ALL recordings?", "This action cannot be undone!", 0)
	})

	shot("copy_progress", func() {
		statusBar()
		hm.DrawProgressBar("Copying to USB...", 50, "~02:34 remaining - hold 3s to cancel")
	})

	shot("copy_files_menu", func() {
		statusBar()
		fixed := []hardware.MenuItem{
			{Label: "▶ Start Copy", Value: ""},
			{Label: "☑ Select All", Value: "(2 files)"},
			{Label: "☐ Clear All", Value: ""},
		}
		y := 22
		fh := hm.GetFontHeight()
		for i, item := range fixed {
			ctx := "menu"
			prefix := "  "
			if i == 0 {
				ctx = "selected"
				prefix = "> "
			}
			hm.SwitchToContext(ctx)
			hm.DrawText(8, y, prefix+item.Label)
			if item.Value != "" {
				w := hm.GetTextWidth(item.Value)
				hm.DrawText(256-w-16, y, item.Value)
			}
			y += fh
		}
		hm.SwitchToContext("menu")
		hm.DrawText(8, y, "  [X] recording_20240131_143022_ch2_48kHz.wav")
		hm.SwitchToContext("details")
		hm.DrawText(240, 63, "↓")
	})

	shot("remote_info", func() {
		statusBar()
		lines := []struct {
			text string
			ctx  string
		}{
			{"Remote Access", "menu"},
			{"http://192.168.10.162:8080", "details"},
			{"Token: 7F3K QMPX", "details"},
		}
		y := 22
		for _, l := range lines {
			hm.DrawCenteredText(l.text, l.ctx, y)
			y += 11
		}
		hm.DrawCenteredText("Hold encoder to return", "details", 58)
	})

	shot("network_info", func() {
		statusBar()
		details := []struct {
			text string
			ctx  string
		}{
			{"Interface: eth0", "menu"},
			{"Status: Connected", "selected"},
			{"IP Address: 192.168.10.162", "details"},
		}
		y := 22
		for _, d := range details {
			hm.DrawCenteredText(d.text, d.ctx, y)
			y += 11
		}
		hm.DrawCenteredText("Hold encoder to return", "details", 58)
	})

	fmt.Println("done")
}
