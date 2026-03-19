# nina

**English** | [简体中文][readme-zh-link]

Slap your MacBook, it yells back.

> "this is the most amazing thing i've ever seen" — [@kenwheeler](https://x.com/kenwheeler)

> "I just ran sexy mode with my wife sitting next to me...We died laughing" — [@duncanthedev](https://x.com/duncanthedev)

> "peak engineering" — [@tylertaewook](https://x.com/tylertaewook)

Uses either the Apple Silicon accelerometer (IOKit HID) or microphone transients (`--mic`) to detect physical hits and play audio responses.

## Requirements

- macOS
- `sudo` (accelerometer mode only)
- `ffmpeg` (microphone mode only, `--mic`)
- Go 1.26+ (if building from source)

## Install

Download from the [latest release](https://github.com/taigrr/nina/releases/latest).

Or build from source:

```bash
go install github.com/taigrr/nina@latest
```

> **Note:** `go install` places the binary in `$GOBIN` (if set) or `$(go env GOPATH)/bin` (which defaults to `~/go/bin`). Copy it to a system path so `sudo nina` works. For example, with the default Go settings:
>
> ```bash
> sudo cp "$(go env GOPATH)/bin/nina" /usr/local/bin/nina
> ```

## Usage

```bash
# Normal mode — says "ow!" when slapped
sudo nina

# M1-friendly mode — microphone transient detection (no sudo)
nina --mic
nina --mic --mic-device 1   # choose a different input device index
nina --mic --mic-device 1 --strict  # reject most voice/scream triggers

# Sexy mode — escalating responses based on slap frequency
sudo nina --sexy
nina --mic --sexy

# Halo mode — plays Halo death sounds when slapped
sudo nina --halo
nina --mic --halo

# Fast mode — faster polling and shorter cooldown
sudo nina --fast
sudo nina --sexy --fast

# Custom mode — plays your own MP3 files from a directory
sudo nina --custom /path/to/mp3s

# Adjust sensitivity with amplitude threshold (lower = more sensitive)
sudo nina --min-amplitude 0.1   # more sensitive
sudo nina --min-amplitude 0.25  # less sensitive
sudo nina --sexy --min-amplitude 0.2

# Set cooldown period in millisecond (default: 750)
sudo nina --cooldown 600

# Set playback speed multiplier (default: 1.0)
sudo nina --speed 0.7   # slower and deeper
sudo nina --speed 1.5   # faster
sudo nina --sexy --speed 0.6

# If mic mode is too sensitive, increase threshold
nina --mic --min-amplitude 0.08
nina --mic --strict --min-amplitude 0.07 --cooldown 700

# Nina 2.0 companion mode (single-command run)
GOOGLE_API_KEY=your_key_here nina --mic --sus --mic-device 1
```

### Nina 2.0 (Context Companion in `--sus` mode)

- Starts a background vision loop and memory engine automatically when `--sus` is enabled.
- Uses local SQLite memory at `nina_memory.db` (override with `--memory-db /path/to.db`).
- Uses local guardrails + Gemini 1.5 Flash (`GOOGLE_API_KEY`) for context tagging/thoughts.
- If API is unavailable, Nina continues with local classification (no crash).

macOS permissions needed for full Nina 2.0 behavior:

- **Microphone**: for `--mic`.
- **Screen Recording**: for screenshot-based context analysis.
- **Automation/Accessibility (System Events)**: to read active app/window metadata.

Context tags used for sprite routing:

- `mode_focus`
- `mode_chill`
- `mode_game`
- `mode_music`
- `mode_shame`
- `mode_unknown`

### Modes

**Pain mode** (default): Randomly plays from 10 pain/protest audio clips when a slap is detected.

**Sexy mode** (`--sexy`): Tracks slaps within a rolling 5-minute window. The more you slap, the more intense the audio response. 60 levels of escalation. File names do not need numeric ordering; clips are randomized within the current escalation tier and immediate repeats are avoided.

**Halo mode** (`--halo`): Randomly plays from death sound effects from the Halo video game series when a slap is detected.

**Custom mode** (`--custom`): Randomly plays MP3 files from a custom directory you specify.

Randomized modes use a short anti-repeat window: a clip will not replay if it appeared in the previous 3 events (when enough alternatives exist).

**Mic input mode** (`--mic`): Uses microphone spikes to detect taps/slaps. Works without `sudo` and is useful on machines without accelerometer support (for example many M1 models). Use `--mic-device` if you need a non-default mic index.

**Strict mic classifier** (`--strict` with `--mic`): favors short, impact-like transients and suppresses sustained voice energy (for example yelling/screaming).

To list available AVFoundation device indices:

```bash
ffmpeg -f avfoundation -list_devices true -i ""
```

### Detection tuning

Use `--fast` for a more responsive profile with faster polling (4ms vs 10ms), shorter cooldown (350ms vs 750ms), higher sensitivity (0.18 vs 0.05 threshold), and larger sample batch (320 vs 200).

You can still override individual values with `--min-amplitude` and `--cooldown` when needed.

### Sensitivity

Control detection sensitivity with `--min-amplitude` (default: `0.05`):

- Lower values (e.g., 0.05-0.10): Very sensitive, detects light taps
- Medium values (e.g., 0.15-0.30): Balanced sensitivity
- Higher values (e.g., 0.30-0.50): Only strong impacts trigger sounds

In accelerometer mode, this is acceleration amplitude (g-force).
In mic mode, this is normalized transient strength above the ambient noise floor.
In strict mic mode, start around `0.06-0.09` and tune from there.

## Running as a Service

To have nina start automatically at boot, create a launchd plist. Pick your mode:

<details>
<summary>Pain mode (default)</summary>

```bash
sudo tee /Library/LaunchDaemons/com.taigrr.nina.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.taigrr.nina</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/nina</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/nina.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/nina.err</string>
</dict>
</plist>
EOF
```

</details>

<details>
<summary>Sexy mode</summary>

```bash
sudo tee /Library/LaunchDaemons/com.taigrr.nina.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.taigrr.nina</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/nina</string>
        <string>--sexy</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/nina.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/nina.err</string>
</dict>
</plist>
EOF
```

</details>

<details>
<summary>Halo mode</summary>

```bash
sudo tee /Library/LaunchDaemons/com.taigrr.nina.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.taigrr.nina</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/nina</string>
        <string>--halo</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/nina.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/nina.err</string>
</dict>
</plist>
EOF
```

</details>

> **Note:** Update the path to `nina` if you installed it elsewhere (e.g. `~/go/bin/nina`).

Load and start the service:

```bash
sudo launchctl load /Library/LaunchDaemons/com.taigrr.nina.plist
```

Since the plist lives in `/Library/LaunchDaemons` and no `UserName` key is set, launchd runs it as root — no `sudo` needed.

To stop or unload:

```bash
sudo launchctl unload /Library/LaunchDaemons/com.taigrr.nina.plist
```

## How it works

1. Reads raw accelerometer data via IOKit HID, or audio PCM via `ffmpeg` in `--mic` mode
2. Runs impact detection (accelerometer vibration detector, or mic transient detector)
3. When a significant impact is detected, plays an embedded MP3 response
4. **Optional volume scaling** (`--volume-scaling`) — light taps play quietly, hard slaps play at full volume
5. **Optional speed control** (`--speed`) — adjusts playback speed and pitch (0.5 = half speed, 2.0 = double speed)
6. 750ms cooldown between responses to prevent rapid-fire, adjustable with `--cooldown`

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=taigrr/nina&type=date&legend=top-left)](https://www.star-history.com/#taigrr/nina&type=date&legend=top-left)

## Credits

Sensor reading and vibration detection ported from [olvvier/apple-silicon-accelerometer](https://github.com/olvvier/apple-silicon-accelerometer).

## License

MIT

<!-- Links -->
[readme-zh-link]: ./README-zh.md
