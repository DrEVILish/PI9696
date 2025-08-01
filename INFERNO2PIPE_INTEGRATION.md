# Inferno2Pipe Recording Integration

This document describes how the PI9696 project integrates with inferno2pipe for audio recording functionality.

## Overview

The PI9696 project uses inferno2pipe's `save_to_file` command instead of directly calling ffmpeg for audio recording. This provides better integration with specialized audio hardware and processing pipelines.

## Command Structure

### Standard 48kHz Recording
For recordings at the default 48kHz sample rate:
```bash
./save_to_file <channels>
```

Where `<channels>` is the number of audio channels (1-8).

### Custom Sample Rate Recording
For recordings at sample rates other than 48kHz:
```bash
sample_rate=<rate> ./save_to_file <channels>
```

Where:
- `<rate>` is the desired sample rate (e.g., 44100, 96000)
- `<channels>` is the number of audio channels (1-8)

## Implementation Details

### Recording Process

1. **Start Recording** (`startRecording()` function):
   - Determines current sample rate and channel count settings
   - Builds appropriate command based on sample rate
   - Starts inferno2pipe process using `exec.Command()`
   - Sets recording state to active

2. **Stop Recording** (`stopRecording()` function):
   - Sends SIGTERM signal to inferno2pipe process
   - Waits for process to terminate gracefully
   - Resets recording state to idle

### Code Implementation

```go
// Build inferno2pipe command
var cmdName string
var args []string

if sampleRate != 48000 {
    // For non-48kHz sample rates, prefix with sample_rate=X
    cmdName = "sh"
    args = []string{
        "-c",
        fmt.Sprintf("sample_rate=%d ./save_to_file %d", sampleRate, channelCount),
    }
} else {
    // For 48kHz, use direct command
    cmdName = "./save_to_file"
    args = []string{fmt.Sprintf("%d", channelCount)}
}

infernoPipeCmd = exec.Command(cmdName, args...)
```

## Supported Configurations

### Sample Rates
The system supports various sample rates including:
- 44.1 kHz (CD quality)
- 48 kHz (professional standard)
- 88.2 kHz (high resolution)
- 96 kHz (high resolution)
- 176.4 kHz (ultra high resolution)
- 192 kHz (ultra high resolution)

### Channel Configurations
Supports 1-8 channels:
- 1 channel (mono)
- 2 channels (stereo)
- 4 channels (quad)
- 6 channels (5.1 surround)
- 8 channels (7.1 surround)

## Requirements

### Dependencies
- **inferno2pipe** - Must be installed and configured
- **save_to_file** - Executable must be present in project directory
- **ALSA** - Audio system support
- **Appropriate permissions** - Access to audio hardware

### File System
- **Recording directory** (`/rec`) must exist and be writable
- **USB mount point** (`/media/usb`) for file transfers
- **Sufficient storage** for audio files

## File Naming Convention

Generated audio files follow this pattern:
```
recording_YYYYMMDD_HHMMSS_chX_YYkHz.wav
```

Where:
- `YYYYMMDD_HHMMSS` - Timestamp when recording started
- `X` - Number of channels
- `YY` - Sample rate in kHz

Example: `recording_20241201_143052_ch2_48kHz.wav`

## Error Handling

### Common Issues

1. **inferno2pipe not found**
   - Ensure `./save_to_file` is executable and in project directory
   - Check PATH and working directory settings

2. **Permission denied**
   - Verify audio device permissions
   - Check file system write permissions

3. **Invalid sample rate**
   - Ensure sample rate is supported by hardware
   - Verify inferno2pipe configuration

### Debug Information

Recording errors are logged with details:
```go
log.Printf("Failed to start recording with inferno2pipe: %v", err)
```

## Integration Points

### User Interface
- **Encoder rotation** - Adjusts sample rate and channel count
- **Button press** - Starts/stops recording
- **Status display** - Shows current recording parameters
- **Menu system** - Allows configuration changes

### Hardware Interface
- **Display updates** - Real-time recording status and parameters
- **LED indicators** - Recording state feedback
- **Audio hardware** - Direct integration via inferno2pipe

## Configuration Variables

Key variables in the recording system:

```go
var (
    sampleRates    = []int{44100, 48000, 88200, 96000, 176400, 192000}
    sampleRateIdx  = 1  // Default to 48kHz
    channelCount   = 2  // Default to stereo
    infernoPipeCmd *exec.Cmd
)
```

## Advantages over FFmpeg

1. **Specialized audio processing** - Optimized for high-quality recording
2. **Lower latency** - Direct hardware integration
3. **Better resource usage** - Efficient audio pipeline
4. **Hardware-specific optimizations** - Tailored for audio interfaces
5. **Simplified configuration** - Channel and sample rate via parameters

## Future Enhancements

Potential improvements:
- **Real-time monitoring** - Audio level meters during recording
- **Multiple file formats** - Support for FLAC, DSD, etc.
- **Automatic gain control** - Dynamic level adjustment
- **Record scheduling** - Timed recording sessions
- **Remote control** - Network-based recording control

## Troubleshooting

### Verification Steps

1. **Check inferno2pipe installation**:
   ```bash
   ls -la ./save_to_file
   ```

2. **Test basic recording**:
   ```bash
   ./save_to_file 2  # Test 2-channel recording
   ```

3. **Verify audio devices**:
   ```bash
   arecord -l  # List available recording devices
   ```

4. **Check permissions**:
   ```bash
   groups $USER  # Should include 'audio' group
   ```

### Log Analysis

Monitor logs for recording events:
```bash
tail -f /var/log/pi9696/pi9696.log
```

Look for inferno2pipe process start/stop events and any error messages.

## Support

For issues specific to:
- **inferno2pipe functionality** - Consult inferno2pipe documentation
- **PI9696 integration** - Check project issues and documentation
- **Audio hardware** - Verify ALSA configuration and device compatibility