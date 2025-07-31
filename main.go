package main

import (
	"fmt"
	"os/exec"
	"time"
	"os"
	"io/ioutil"
	"strings"
	"strconv"
	"sync"
)

const (
	DisplayWidth      = 256
	DisplayHeight     = 64
	MaxChannelCount   = 128
	BitsPerSample     = 32
	RecordPath        = "/rec"
	USBMountPoint     = "/media/usb"
	MinSampleRate     = 48000
	MaxSampleRate     = 96000
)

var (
	sampleRate    = MinSampleRate
	channelCount  = 2
	isRecording   = false
	isCopying     = false
	recordStart   time.Time
	menuVisible   = false
	selectedMenu  = 0
	usbMounted    = false
	filesToCopy   = make(map[string]bool)
	allFiles      []string
	copyProgress  = 0
	mutex         sync.Mutex
)

func main() {
	// Hardware Init (GPIO, Display, Encoder, Buttons)
	initHardware()
	go detectUSB()
	for {
		render()
		time.Sleep(100 * time.Millisecond)
	}
}

func detectUSB() {
	for {
		if _, err := os.Stat(USBMountPoint); err == nil {
			mutex.Lock()
			usbMounted = true
			mutex.Unlock()
		} else {
			mutex.Lock()
			usbMounted = false
			mutex.Unlock()
		}
		time.Sleep(1 * time.Second)
	}
}

func render() {
	clearDisplay()
	mutex.Lock()
	if isCopying {
		renderCopyProgress()
	} else if isRecording {
		renderRecordingStatus()
	} else if menuVisible {
		renderMenu()
	} else {
		renderIdleStatus()
	}
	mutex.Unlock()
	drawDisplay()
}

func renderRecordingStatus() {
	elapsed := time.Since(recordStart)
	remaining := estimateRemainingTime()
	drawText(0, 0, fmt.Sprintf("%s            %s (%s)",
		formatDuration(elapsed), formatDuration(remaining), getRemainingStorage()))
}

func renderIdleStatus() {
	drawText(0, 0, fmt.Sprintf("%s            %s (%s)",
		formatDuration(0), formatDuration(estimateRemainingTime()), getRemainingStorage()))
}

func renderMenu() {
	items := []string{
		"Sample Rate",
		"Channel Count",
		"Copy Files",
		"Delete All Recordings",
		"Shutdown",
		"Restart",
		"Exit Menu",
	}
	for i, item := range items {
		prefix := "  "
		if i == selectedMenu {
			prefix = "> "
		}
		drawText(160, 8*i, fmt.Sprintf("%s%s", prefix, item))
	}
}

func renderCopyProgress() {
	bar := strings.Repeat("#", copyProgress/2) + strings.Repeat(" ", 50-copyProgress/2)
	drawText(0, 24, fmt.Sprintf("[%s] %d%%", bar, copyProgress))
	drawText(0, 40, "Hold button 3s to cancel...")
}

func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, (seconds/60)%60, seconds%60)
}

func estimateRemainingTime() time.Duration {
	bytesPerSec := float64(sampleRate * channelCount * BitsPerSample / 8)
	free := getFreeSpace()
	return time.Duration(float64(free)/bytesPerSec) * time.Second
}

func getRemainingStorage() string {
	free := getFreeSpace()
	return fmt.Sprintf("%dMB", free/(1024*1024))
}

func getFreeSpace() uint64 {
	var stat syscall.Statfs_t
	if usbMounted {
		syscall.Statfs(USBMountPoint, &stat)
	} else {
		syscall.Statfs(RecordPath, &stat)
	}
	return stat.Bavail * uint64(stat.Bsize)
}

func clearDisplay() {}
func drawDisplay() {}
func drawText(x, y int, text string) {}
func initHardware() {}
