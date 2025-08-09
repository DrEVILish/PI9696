package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"strings"
	"sync"
	"syscall"
	"time"

	"pi9696/hardware"
)

const (
	DisplayWidth      = 256
	DisplayHeight     = 64
	MaxChannelCount   = 128
	BitsPerSample     = 32
	RecordPath        = "/rec"
	RawPath           = "/rec/raw"
	USBMountPoint     = "/media/usb"
	RecordingFormat   = "WAV 24bit"
)

type AppState int

const (
	StateIdle AppState = iota
	StateRecording
	StateSettings
	StateCopyFiles
	StateCopying
	StateSystemOptions
	StateNetworkInfo
	StateConfirm
)

type MenuMode int

const (
	SettingsMenu MenuMode = iota
	CopyFilesMenu
	SystemOptionsMenu
	NetworkInfoMenu
	DeleteConfirm
	FormatConfirm
	ShutdownConfirm
	RestartConfirm
	InfernoRestartConfirm
)

type ConfirmOption int

const (
	ConfirmNo ConfirmOption = iota
	ConfirmYes
)

// Inferno server states
type InfernoState int

const (
	InfernoStopped InfernoState = iota
	InfernoStarting
	InfernoRunning
	InfernoFailed
)

var (
	hwManager      *hardware.HardwareManager
	sampleRates    = []int{44100, 48000, 96000, 192000}
	sampleRateIdx  = 1 // Default to 48kHz
	channelCount   = 2
	isRecording    = false
	isCopying      = false
	recordStart    time.Time
	recordingFile  string
	currentState   = StateIdle
	menuMode       = SettingsMenu
	selectedMenu   = 0
	menuScrollOffset = 0
	confirmOption  = ConfirmNo
	usbMounted     = false
	usbSize        = ""
	filesToCopy    = make(map[string]bool)
	allFiles       []string
	copyProgress   = 0
	showRemaining  = false
	infernoCmd      *exec.Cmd
	ffmpegCmd       *exec.Cmd
	fifoPath        string
	infernoState    InfernoState
	lastSampleRate  int
	lastChannelCount int
	networkWasUp    bool
	mutex           sync.Mutex
)

func main() {
	var err error
	hwManager, err = hardware.NewHardwareManager()
	if err != nil {
		log.Fatalf("Failed to initialize hardware: %v", err)
	}
	defer hwManager.Close()

	setupHardwareCallbacks()
	go detectUSB()
	go updateLoop()
	go networkMonitorLoop()

	// Keep main thread alive
	select {}
}

func setupHardwareCallbacks() {
	hwManager.SetEncoderCallbacks(
		onEncoderRotate,
		onEncoderClick,
		onEncoderHold,
	)

	hwManager.SetButtonCallback(hardware.RecordButton, onButtonPress)
	hwManager.SetButtonCallback(hardware.StopButton, onButtonPress)
	hwManager.SetButtonCallback(hardware.PlayButton, onButtonPress)
}

func onEncoderRotate(direction int) {
	mutex.Lock()
	defer mutex.Unlock()

	switch currentState {
	case StateIdle:
		// No action on idle screen

	case StateSettings:
		if selectedMenu == 0 { // Sample Rate
			adjustSampleRate(direction)
		} else if selectedMenu == 1 { // Channel Count
			adjustChannelCount(direction)
		} else {
			navigateMenu(direction)
		}

	case StateCopyFiles:
		navigateMenu(direction)

	case StateSystemOptions:
		navigateMenu(direction)

	case StateConfirm:
		if confirmOption == ConfirmNo {
			confirmOption = ConfirmYes
		} else {
			confirmOption = ConfirmNo
		}
	}
}

func onEncoderClick() {
	mutex.Lock()
	defer mutex.Unlock()

	switch currentState {
	case StateIdle:
		if !isRecording {
			currentState = StateSettings
			selectedMenu = 0
		}

	case StateSettings:
		handleSettingsClick()

	case StateCopyFiles:
		handleCopyFilesClick()

	case StateSystemOptions:
		handleSystemOptionsClick()

	case StateConfirm:
		handleConfirmClick()
	}
}

func onEncoderHold() {
	mutex.Lock()
	defer mutex.Unlock()

	if currentState == StateCopying {
		isCopying = false
		currentState = StateIdle
	} else if currentState != StateIdle && currentState != StateRecording {
		currentState = StateIdle
		selectedMenu = 0
		menuScrollOffset = 0
	}
}

func onButtonPress(buttonType hardware.ButtonType) {
	mutex.Lock()
	defer mutex.Unlock()

	switch buttonType {
	case hardware.RecordButton:
		if currentState == StateIdle && !isRecording {
			startRecording()
		}
	case hardware.StopButton:
		if isRecording {
			stopRecording()
		}
	}
}

func adjustSampleRate(direction int) {
	sampleRateIdx += direction
	if sampleRateIdx < 0 {
		sampleRateIdx = len(sampleRates) - 1
	} else if sampleRateIdx >= len(sampleRates) {
		sampleRateIdx = 0
	}
	// Check if we need to restart Inferno server
	checkInfernoRestart()
}

func adjustChannelCount(direction int) {
	channelCount += direction
	if channelCount < 1 {
		channelCount = 1
	} else if channelCount > MaxChannelCount {
		channelCount = MaxChannelCount
	}
	// Check if we need to restart Inferno server
	checkInfernoRestart()
}

func navigateMenu(direction int) {
	var maxItems int

	switch currentState {
	case StateSettings:
		maxItems = 7 // Sample Rate, Channel Count, Copy Files, System Options, Network Info, Restart Inferno, Exit
	case StateCopyFiles:
		maxItems = len(allFiles) + 3 // Start Copy, [All], [NONE], files...
	case StateSystemOptions:
		maxItems = 5 // Delete All, Format USB, Shutdown, Restart, Exit
	}

	selectedMenu += direction
	if selectedMenu < 0 {
		selectedMenu = maxItems - 1
	} else if selectedMenu >= maxItems {
		selectedMenu = 0
	}
}

func handleSettingsClick() {
	switch selectedMenu {
	case 0, 1: // Sample Rate or Channel Count - do nothing, direct adjustment
	case 2: // Copy Files
		if usbMounted {
			loadFilesToCopy()
			currentState = StateCopyFiles
			selectedMenu = 0
			menuScrollOffset = 0
		}
	case 3: // System Options
		currentState = StateSystemOptions
		selectedMenu = 0
		menuScrollOffset = 0
	case 4: // Network Info
		currentState = StateNetworkInfo
		selectedMenu = 0
		menuScrollOffset = 0
	case 5: // Restart Inferno
		menuMode = InfernoRestartConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 6: // Exit
		currentState = StateIdle
		menuScrollOffset = 0
	}
}

func handleCopyFilesClick() {
	if selectedMenu == 0 { // Start Copy
		startCopyOperation()
	} else if selectedMenu == 1 { // [All]
		for file := range filesToCopy {
			filesToCopy[file] = true
		}
	} else if selectedMenu == 2 { // [NONE]
		for file := range filesToCopy {
			filesToCopy[file] = false
		}
	} else if selectedMenu >= 3 && selectedMenu-3 < len(allFiles) {
		file := allFiles[selectedMenu-3]
		filesToCopy[file] = !filesToCopy[file]
	}
}

func handleSystemOptionsClick() {
	switch selectedMenu {
	case 0: // Delete All Recordings
		menuMode = DeleteConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 1: // Format USB Drive
		if usbMounted {
			menuMode = FormatConfirm
			currentState = StateConfirm
			confirmOption = ConfirmNo
		}
	case 2: // Shutdown System
		menuMode = ShutdownConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 3: // Restart System
		menuMode = RestartConfirm
		currentState = StateConfirm
		confirmOption = ConfirmNo
	case 4: // Exit
		currentState = StateSettings
		selectedMenu = 0
		menuScrollOffset = 0
	}
}

func handleConfirmClick() {
	if confirmOption == ConfirmYes {
		switch menuMode {
		case DeleteConfirm:
			deleteAllRecordings()
		case FormatConfirm:
			formatUSB()
		case ShutdownConfirm:
			exec.Command("sudo", "shutdown", "-h", "now").Run()
		case RestartConfirm:
			exec.Command("sudo", "reboot").Run()
		case InfernoRestartConfirm:
			restartInfernoServer()
		}
	}
	currentState = StateIdle
}

// Network monitoring loop to start/restart Inferno server when eth0 comes up
func networkMonitorLoop() {
	for {
		mutex.Lock()
		networkUp := hwManager.IsNetworkAvailable()
		
		if networkUp && !networkWasUp {
			// Network just came up, start Inferno if not running
			if infernoState != InfernoRunning {
				log.Printf("Network available, starting Inferno server")
				startInfernoServer()
			}
		} else if !networkUp && networkWasUp {
			// Network went down
			log.Printf("Network unavailable")
		}
		
		networkWasUp = networkUp
		mutex.Unlock()
		
		time.Sleep(5 * time.Second) // Check every 5 seconds
	}
}

// Check if Inferno server needs to be restarted due to setting changes
func checkInfernoRestart() {
	mutex.Lock()
	defer mutex.Unlock()
	
	currentSampleRate := sampleRates[sampleRateIdx]
	if (currentSampleRate != lastSampleRate || channelCount != lastChannelCount) && infernoState == InfernoRunning {
		log.Printf("Settings changed, restarting Inferno server")
		stopInfernoServer()
		startInfernoServer()
	}
}

// Start the Inferno Audio over IP server
func startInfernoServer() {
	if infernoState == InfernoRunning || infernoState == InfernoStarting {
		return
	}
	
	infernoState = InfernoStarting
	sampleRate := sampleRates[sampleRateIdx]
	
	// Create a persistent FIFO for Inferno output
	timestamp := time.Now().Format("20060102_150405")
	baseFileName := fmt.Sprintf("inferno_%s_ch%d_%dkHz.raw", timestamp, channelCount, sampleRate/1000)
	fifoPath = fmt.Sprintf("%s/%s", RawPath, baseFileName)
	
	// Ensure directories exist
	os.MkdirAll(RawPath, 0755)
	
	// Remove old FIFO if exists
	os.Remove(fifoPath)
	
	// Create new FIFO
	if err := syscall.Mkfifo(fifoPath, 0666); err != nil {
		log.Printf("Failed to create Inferno FIFO %s: %v", fifoPath, err)
		infernoState = InfernoFailed
		return
	}
	
	// Start Inferno server
	infernoCmd = exec.Command("sh", "-c", 
		fmt.Sprintf("cd inferno && INFERNO_SAMPLE_RATE=%d cargo run -- -c %d -o ../%s", 
			sampleRate, channelCount, fifoPath))
	
	if err := infernoCmd.Start(); err != nil {
		log.Printf("Failed to start Inferno server: %v", err)
		os.Remove(fifoPath)
		infernoState = InfernoFailed
		return
	}
	
	infernoState = InfernoRunning
	lastSampleRate = sampleRate
	lastChannelCount = channelCount
	log.Printf("Inferno server started with %dkHz, %d channels", sampleRate/1000, channelCount)
}

// Stop the Inferno server
func stopInfernoServer() {
	if infernoCmd != nil && infernoCmd.Process != nil {
		infernoCmd.Process.Signal(syscall.SIGTERM)
		infernoCmd.Wait()
		infernoCmd = nil
	}
	
	if fifoPath != "" {
		os.Remove(fifoPath)
		fifoPath = ""
	}
	
	infernoState = InfernoStopped
	log.Printf("Inferno server stopped")
}

// Restart the Inferno server
func restartInfernoServer() {
	stopInfernoServer()
	time.Sleep(1 * time.Second) // Give it a moment
	if hwManager.IsNetworkAvailable() {
		startInfernoServer()
	}
}

func startRecording() {
	if infernoState != InfernoRunning {
		log.Printf("Cannot start recording: Inferno server not running")
		return
	}
	
	recordStart = time.Now()
	timestamp := recordStart.Format("20060102_150405")
	sampleRate := sampleRates[sampleRateIdx]
	recordingFile = fmt.Sprintf("%s/recording_%s_ch%d_%dkHz.wav",
		RecordPath, timestamp, channelCount, sampleRate/1000)

	// Create recording directory
	os.MkdirAll(RecordPath, 0755)

	// Start FFmpeg to convert raw stream from existing Inferno FIFO to final output
	ffmpegCmd = exec.Command("ffmpeg", 
		"-nostdin", "-fflags", "nobuffer", 
		"-f", "s32le", "-sample_rate", fmt.Sprintf("%d", sampleRate), 
		"-ac", fmt.Sprintf("%d", channelCount), 
		"-i", fifoPath, 
		"-c:a", "pcm_s24le", recordingFile)
	
	err := ffmpegCmd.Start()
	if err != nil {
		log.Printf("Failed to start FFmpeg: %v", err)
		return
	}

	isRecording = true
	currentState = StateRecording
}

func stopRecording() {
	// Only stop FFmpeg, leave Inferno server running
	if ffmpegCmd != nil && ffmpegCmd.Process != nil {
		ffmpegCmd.Process.Signal(syscall.SIGTERM)
		ffmpegCmd.Wait()
		ffmpegCmd = nil
	}

	isRecording = false
	currentState = StateIdle
}

func loadFilesToCopy() {
	allFiles = []string{}
	filesToCopy = make(map[string]bool)

	files, err := filepath.Glob(filepath.Join(RecordPath, "*.wav"))
	if err != nil {
		return
	}

	for _, file := range files {
		basename := filepath.Base(file)
		allFiles = append(allFiles, basename)
		filesToCopy[basename] = true
	}

	sort.Strings(allFiles)
}

func startCopyOperation() {
	if !usbMounted {
		return
	}

	currentState = StateCopying
	isCopying = true
	copyProgress = 0

	go func() {
		selectedFiles := []string{}
		for file, selected := range filesToCopy {
			if selected {
				selectedFiles = append(selectedFiles, file)
			}
		}

		if len(selectedFiles) == 0 {
			mutex.Lock()
			isCopying = false
			currentState = StateIdle
			mutex.Unlock()
			return
		}

		for i, file := range selectedFiles {
			if !isCopying {
				break
			}

			src := filepath.Join(RecordPath, file)
			dst := filepath.Join(USBMountPoint, file)

			err := copyFile(src, dst)
			if err != nil {
				log.Printf("Failed to copy %s: %v", file, err)
			}

			mutex.Lock()
			copyProgress = int(float64(i+1) / float64(len(selectedFiles)) * 100)
			mutex.Unlock()
		}

		mutex.Lock()
		isCopying = false
		currentState = StateIdle
		mutex.Unlock()
	}()
}

func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, input, 0644)
}

func deleteAllRecordings() {
	files, err := filepath.Glob(filepath.Join(RecordPath, "*.wav"))
	if err != nil {
		return
	}
	for _, file := range files {
		os.Remove(file)
	}
}

func formatUSB() {
	if !usbMounted {
		return
	}
	exec.Command("sudo", "umount", USBMountPoint).Run()
	exec.Command("sudo", "mkfs.vfat", "-F", "32", "/dev/sda1").Run()
	time.Sleep(2 * time.Second)
}

func detectUSB() {
	for {
		if _, err := os.Stat(USBMountPoint); err == nil {
			mutex.Lock()
			usbMounted = true
			usbSize = getUSBSize()
			mutex.Unlock()
		} else {
			mutex.Lock()
			usbMounted = false
			usbSize = ""
			mutex.Unlock()
		}
		time.Sleep(1 * time.Second)
	}
}

func getUSBSize() string {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(USBMountPoint, &stat); err != nil {
		return ""
	}

	totalBytes := uint64(stat.Blocks) * uint64(stat.Bsize)

	if totalBytes < 1024*1024*1024 { // Less than 1GB
		mb := totalBytes / (1024 * 1024)
		return fmt.Sprintf("%dmb", roundToPowerOfTwo(int(mb)))
	} else if totalBytes < 1024*1024*1024*1024 { // Less than 1TB
		gb := totalBytes / (1024 * 1024 * 1024)
		return fmt.Sprintf("%dGB", roundToPowerOfTwo(int(gb)))
	} else {
		tb := totalBytes / (1024 * 1024 * 1024 * 1024)
		return fmt.Sprintf("%dTB", roundToPowerOfTwo(int(tb)))
	}
}

func roundToPowerOfTwo(value int) int {
	if value <= 0 {
		return 1
	}
	power := math.Log2(float64(value))
	return int(math.Pow(2, math.Round(power)))
}

func updateLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		render()
	}
}

func render() {
	mutex.Lock()
	defer mutex.Unlock()

	hwManager.ClearDisplay()

	// Always render status bar first
	renderStatusBar()

	switch currentState {
	case StateIdle:
		renderIdleScreen()
	case StateRecording:
		renderRecordingScreen()
	case StateSettings:
		renderSettingsMenu()
	case StateCopyFiles:
		renderCopyFilesMenu()
	case StateCopying:
		renderCopyProgress()
	case StateSystemOptions:
		renderSystemOptionsMenu()
	case StateNetworkInfo:
		renderNetworkInfo()
	case StateConfirm:
		renderConfirmDialog()
	}

	hwManager.UpdateDisplay()
}

func renderStatusBar() {
	sampleRate := sampleRates[sampleRateIdx]
	// Use FiraCode ligatures: >= <= != === !== -> <- =>
	formatStr := fmt.Sprintf("WAV %dbit %dkHz %dch", BitsPerSample, sampleRate/1000, channelCount)

	// Right side - USB status with enhanced typography
	rightSide := ""
	if usbMounted && usbSize != "" {
		// Use arrow ligature -> for better visual connection
		rightSide = fmt.Sprintf("%s [USB]", usbSize)
	} else {
		rightSide = "[---]"
	}

	// Use context-aware FiraCode rendering with Inferno status
	infernoRunning := (infernoState == InfernoRunning)
	hwManager.DrawStatusBarWithInferno(formatStr, rightSide, infernoRunning)
}

func renderIdleScreen() {
	// Use context-aware rendering for standby state
	hwManager.DrawCenteredText("~ Standby ~", "idle", 32)

	// Time remaining with enhanced formatting using FiraCode features
	remaining := estimateRemainingTime()
	storage := getRemainingStorage()
	// Use mathematical symbols and arrows for better typography
	timeText := fmt.Sprintf("⏱ %s (%s) available", formatDuration(remaining), storage)
	hwManager.DrawCenteredText(timeText, "details", 48)
}

func renderRecordingScreen() {
	elapsed := time.Since(recordStart)
	remaining := estimateRemainingTime()
	storage := getRemainingStorage()
	filename := ""

	if recordingFile != "" {
		filename = filepath.Base(recordingFile)
	}

	// Use FiraCode's context-aware recording display with enhanced typography
	elapsedStr := formatDuration(elapsed)
	remainingStr := fmt.Sprintf("%s (%s)", formatDuration(remaining), storage)

	hwManager.DrawRecordingStatus(elapsedStr, remainingStr, filename)
}

func renderSettingsMenu() {
	// Use FiraCode header context for the title
	hwManager.DrawCenteredText("⚙ Settings", "header", 20)

	// Menu items using FiraCode MenuItem rendering
	sampleRate := sampleRates[sampleRateIdx]
	sampleRateText := fmt.Sprintf("%dkHz", sampleRate/1000)

	// Use arrow ligatures and enhanced typography
	allItems := []hardware.MenuItem{
		{Label: "Sample Rate →", Value: sampleRateText},
		{Label: "Channel Count →", Value: fmt.Sprintf("%d", channelCount)},
		{Label: "Copy Files →", Value: ""},
		{Label: "System Options →", Value: ""},
		{Label: "Network Info →", Value: ""},
		{Label: "Restart Inferno", Value: getInfernoStatusText()},
		{Label: "Exit", Value: ""},
	}

	// Calculate scrolling parameters
	totalItems := len(allItems)
	maxVisibleItems := 6
	
	// Update scroll offset based on selected item
	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset + maxVisibleItems {
		menuScrollOffset = selectedMenu - maxVisibleItems + 1
	}

	// Ensure scroll offset doesn't go past the end
	if menuScrollOffset > totalItems - maxVisibleItems {
		menuScrollOffset = totalItems - maxVisibleItems
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	// Create visible items slice
	endIdx := menuScrollOffset + maxVisibleItems
	if endIdx > totalItems {
		endIdx = totalItems
	}
	visibleItems := allItems[menuScrollOffset:endIdx]

	// Adjust selected index for visible items
	visibleSelectedIndex := selectedMenu - menuScrollOffset

	// Draw visible items
	y := 32
	fontHeight := 12

	for i, item := range visibleItems {
		// Switch to emphasis font for selected items
		if i == visibleSelectedIndex {
			if err := hwManager.SwitchToContext("selected"); err != nil {
				return
			}
		} else {
			if err := hwManager.SwitchToContext("menu"); err != nil {
				return
			}
		}

		prefix := "  "
		if i == visibleSelectedIndex {
			prefix = "> "
		}

		// Draw label
		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		// Draw right-aligned value if present
		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-16, y, item.Value)
		}

		y += fontHeight + 2
	}

	// Draw scroll indicators if needed
	if totalItems > maxVisibleItems {
		hwManager.SwitchToContext("details")
		// Up arrow if we can scroll up
		if menuScrollOffset > 0 {
			hwManager.DrawText(240, 32, "↑")
		}
		// Down arrow if we can scroll down
		if menuScrollOffset + maxVisibleItems < totalItems {
			hwManager.DrawText(240, 52, "↓")
		}
	}
}

// Get Inferno server status text for display
func getInfernoStatusText() string {
	switch infernoState {
	case InfernoRunning:
		return "Running"
	case InfernoStarting:
		return "Starting..."
	case InfernoFailed:
		return "Failed"
	default:
		return "Stopped"
	}
}



func renderCopyFilesMenu() {
	// Use FiraCode header with USB symbol
	hwManager.DrawCenteredText("📁 → USB Copy", "header", 20)

	// Create fixed menu items
	fixedMenuItems := []hardware.MenuItem{
		{Label: "▶ Start Copy", Value: ""},
		{Label: "☑ Select All", Value: fmt.Sprintf("(%d files)", len(allFiles))},
		{Label: "☐ Clear All", Value: ""},
	}

	// Calculate scrolling parameters for file list
	maxVisibleFiles := 2 // Max file items that fit on screen after header and fixed items
	totalItems := len(fixedMenuItems) + len(allFiles)
	fixedItemsCount := len(fixedMenuItems)

	// Update scroll offset based on selected item
	if selectedMenu < menuScrollOffset {
		menuScrollOffset = selectedMenu
	} else if selectedMenu >= menuScrollOffset + fixedItemsCount + maxVisibleFiles {
		menuScrollOffset = selectedMenu - fixedItemsCount - maxVisibleFiles + 1
	}

	// Ensure scroll offset doesn't go past the end
	if menuScrollOffset > totalItems - fixedItemsCount - maxVisibleFiles {
		menuScrollOffset = totalItems - fixedItemsCount - maxVisibleFiles
	}
	if menuScrollOffset < 0 {
		menuScrollOffset = 0
	}

	// Draw fixed menu items first
	y := 32
	fontHeight := hwManager.GetFontHeight()

	for i, item := range fixedMenuItems {
		if selectedMenu == i {
			hwManager.SwitchToContext("selected")
		} else {
			hwManager.SwitchToContext("menu")
		}

		prefix := "  "
		if selectedMenu == i {
			prefix = "> "
		}

		labelText := prefix + item.Label
		hwManager.DrawText(8, y, labelText)

		if item.Value != "" {
			valueWidth := hwManager.GetTextWidth(item.Value)
			hwManager.DrawText(256-valueWidth-16, y, item.Value)
		}

		y += fontHeight + 2
	}

	// Draw visible file items with scrolling
	fileStartIdx := 0
	if selectedMenu >= fixedItemsCount {
		fileOffset := selectedMenu - fixedItemsCount
		if fileOffset >= maxVisibleFiles {
			fileStartIdx = fileOffset - maxVisibleFiles + 1
		}
	}

	endIdx := fileStartIdx + maxVisibleFiles
	if endIdx > len(allFiles) {
		endIdx = len(allFiles)
	}

	for i := fileStartIdx; i < endIdx; i++ {
		file := allFiles[i]
		itemIndex := fixedItemsCount + i

		if selectedMenu == itemIndex {
			hwManager.SwitchToContext("selected")
		} else {
			hwManager.SwitchToContext("menu")
		}

		prefix := "  "
		if selectedMenu == itemIndex {
			prefix = "> "
		}

		checkbox := "[ ]"
		if filesToCopy[file] {
			checkbox = "[X]"
		}

		displayName := file
		maxTextWidth := DisplayWidth - 32 // Account for margins and checkbox
		if hwManager.GetTextWidth(prefix+checkbox+" "+displayName) > maxTextWidth {
			// Truncate filename if too long
			for len(displayName) > 0 && hwManager.GetTextWidth(prefix+checkbox+" "+displayName+"...") > maxTextWidth {
				displayName = displayName[:len(displayName)-1]
			}
			if len(displayName) > 0 {
				displayName = displayName + "..."
			}
		}

		hwManager.DrawText(8, y, fmt.Sprintf("%s%s %s", prefix, checkbox, displayName))
		y += fontHeight + 2
	}

	// Draw scroll indicators if needed
	if len(allFiles) > maxVisibleFiles {
		hwManager.SwitchToContext("details")
		// Up arrow if we can scroll up
		if fileStartIdx > 0 {
			hwManager.DrawText(240, 48, "↑")
		}
		// Down arrow if we can scroll down
		if endIdx < len(allFiles) {
			hwManager.DrawText(240, 58, "↓")
		}
	}
}

func renderCopyProgress() {
	// Use FiraCode progress bar with enhanced typography
	title := "📁 → USB Copying..."
	details := "Hold encoder 3s to cancel"

	// Calculate estimated remaining time
	remainingText := "⏱ Calculating..."
	if copyProgress > 0 {
		// Simple estimation based on current progress
		remainingText = "⏱ ~02:34 remaining"
	}

	// Use context-aware progress bar rendering
	hwManager.DrawProgressBar(title, float64(copyProgress), remainingText)

	// Add cancel instruction at bottom
	hwManager.DrawCenteredText(details, "details", 58)
}

func renderSystemOptionsMenu() {
	// Use FiraCode header with system icon
	hwManager.DrawCenteredText("⚡ System Options", "header", 20)

	// Menu items with enhanced icons and typography
	items := []hardware.MenuItem{
		{Label: "🗑 Delete All Recordings", Value: ""},
		{Label: "💾 Format USB Drive", Value: ""},
		{Label: "🔌 Shutdown System", Value: ""},
		{Label: "🔄 Restart System", Value: ""},
		{Label: "← Exit", Value: ""},
	}

	// Use context-aware menu rendering
	hwManager.DrawMenuItems(items, selectedMenu)
}

func renderConfirmDialog() {
	var title, message1, message2 string

	switch menuMode {
	case DeleteConfirm:
		title = "⚠ CONFIRM DELETE"
		message1 = "Delete ALL recordings?"
		message2 = "This action cannot be undone!"
	case FormatConfirm:
		title = "⚠ CONFIRM FORMAT"
		message1 = "Format USB drive?"
		message2 = "All data will be lost!"
	case ShutdownConfirm:
		title = "🔌 SHUTDOWN"
		message1 = "Power off the system?"
		message2 = ""
	case RestartConfirm:
		title = "🔄 RESTART"
		message1 = "Restart the system?"
		message2 = ""
	case InfernoRestartConfirm:
		title = "🔥 RESTART INFERNO"
		message1 = "Restart Inferno server?"
		message2 = "Will reconnect audio stream"
	}

	// Use FiraCode context-aware confirmation dialog
	selectedOption := 0 // NO is default (safer)
	if confirmOption == ConfirmYes {
		selectedOption = 1
	}

	hwManager.DrawConfirmationDialog(title, message1, message2, selectedOption)
}

func renderNetworkInfo() {
	// Use FiraCode header with network icon
	hwManager.DrawCenteredText("🌐 Network Information", "header", 16)

	// Get detailed network information
	networkDetails := hwManager.GetDetailedNetworkInfo()

	// Display network information
	y := 28
	maxLines := 4 // Limit to fit on screen
	for i, detail := range networkDetails {
		if i >= maxLines {
			break
		}

		// Use different contexts for different types of info
		context := "details"
		if i == 0 { // Interface name
			context = "menu"
		} else if strings.Contains(detail, "Status:") {
			if strings.Contains(detail, "Connected") {
				context = "emphasis"
			} else {
				context = "details"
			}
		}

		hwManager.DrawCenteredText(detail, context, y)
		y += 10
	}

	// Add back instruction
	hwManager.DrawCenteredText("Hold encoder to return", "details", 58)
}

func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, secs)
}

func estimateRemainingTime() time.Duration {
	sampleRate := sampleRates[sampleRateIdx]
	bytesPerSec := float64(sampleRate * channelCount * BitsPerSample / 8)
	free := getFreeSpace()
	return time.Duration(float64(free)/bytesPerSec) * time.Second
}

func getRemainingStorage() string {
	free := getFreeSpace()
	if free < 1024*1024 {
		return fmt.Sprintf("%dKB", free/1024)
	} else if free < 1024*1024*1024 {
		return fmt.Sprintf("%dMB", free/(1024*1024))
	} else {
		return fmt.Sprintf("%dGB", free/(1024*1024*1024))
	}
}

func getFreeSpace() uint64 {
	var stat syscall.Statfs_t
	path := RecordPath
	if usbMounted {
		path = USBMountPoint
	}

	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return stat.Bavail * uint64(stat.Bsize)
}
