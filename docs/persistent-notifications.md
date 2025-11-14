# Persistent Notifications with Dunst

## Overview

Hyprvoice now uses **Dunst's replacement mechanism** to provide persistent, non-intrusive notifications during recording and transcription.

Instead of spawning multiple notification popups, hyprvoice maintains a single notification that updates in-place as the workflow progresses.

---

## How It Works

### Dunst Replace ID

Dunst supports **notification replacement** via the `-r` (replace) flag with `notify-send`. By using a consistent replace ID, subsequent notifications replace the previous one instead of creating new popups.

### Notification Flow

```
🎤 Recording...        (Persistent - ID: 9999)
       ↓ (same notification replaced)
⏳ Transcribing...     (Persistent - ID: 9999)
       ↓ (same notification replaced)
✅ Complete            (Persistent - ID: 9999)
```

---

## Implementation Details

### Replace ID Constant

```go
const (
    RecordingNotificationID = 9999 // Persistent notification ID
)
```

### notify-send Command

```bash
notify-send \
    -a "Hyprvoice" \
    -r 9999 \          # Replace ID (Dunst-specific)
    -t 0 \             # Timeout 0 = persistent until replaced
    "Hyprvoice" \
    "🎤 Recording..."
```

### Timeout Behavior

- `-t 0`: Notification persists indefinitely until replaced
- Dunst will keep the notification visible until:
  1. A new notification with the same replace ID arrives
  2. User manually dismisses it
  3. Dunst is restarted

---

## Notification States

| State | Icon | Message | Persistent |
|-------|------|---------|------------|
| **Recording** | 🎤 | Recording... | ✅ Yes |
| **Transcribing** | ⏳ | Transcribing... | ✅ Yes |
| **Complete** | ✅ | Complete | ✅ Yes |
| **Error** | ❌ | (error message) | ❌ No (critical urgency) |
| **Aborted** | ⚠️ | (abort message) | ❌ No |

---

## Dunst Configuration

### Recommended Settings

Add to `~/.config/dunst/dunstrc`:

```ini
[experimental]
# Enable per-monitor DPI (optional)
per_monitor_dpi = true

[global]
# Allow notification replacement
enable_recursive_icon_lookup = true
icon_theme = "Adwaita"

# Notification positioning
origin = top-right
offset = 10x50

# Timeout settings
timeout = 5
# idle_threshold = 120  # Hide after 2min of idle (optional)

[urgency_low]
background = "#1e1e2e"
foreground = "#cdd6f4"
timeout = 5

[urgency_normal]
background = "#1e1e2e"
foreground = "#cdd6f4"
timeout = 5

[urgency_critical]
background = "#1e1e2e"
foreground = "#f38ba8"
timeout = 0  # Critical notifications persist

# Hyprvoice persistent notifications
[hyprvoice_persistent]
appname = "Hyprvoice"
timeout = 0  # Override timeout for Hyprvoice
```

### Testing Dunst Replace Mechanism

```bash
# Send initial notification
notify-send -r 9999 -t 0 "Test" "Message 1"

# Replace it (same notification updates)
notify-send -r 9999 -t 0 "Test" "Message 2"

# Replace again
notify-send -r 9999 -t 0 "Test" "Message 3"

# Should only see ONE notification that updates in-place
```

---

## Benefits

### 1. **Non-Intrusive**
- Single notification instead of multiple popups
- No notification spam during workflow
- Cleaner notification center

### 2. **Always Visible Status**
- Notification stays visible during recording
- Clear visual feedback of current state
- No need to check terminal/logs

### 3. **Smooth Transitions**
- Notification morphs from Recording → Transcribing → Complete
- No jarring popup/dismiss cycles
- Better UX for voice workflows

---

## Compatibility

### Dunst (Full Support) ✅
- Replace ID: **Fully supported**
- Persistent timeout: **Fully supported**
- Recommended notification daemon

### Mako (Partial Support) ⚠️
- Replace ID: **Not supported** (uses different mechanism)
- Will create multiple notifications
- Consider using `makoctl dismiss --all` in workflow

### libnotify-notify-osd (No Support) ❌
- Replace ID: **Not supported**
- Persistent timeout: **Limited**
- Will spawn multiple notifications

---

## Alternatives for Non-Dunst Users

If you're not using Dunst, you can:

### Option 1: Disable Notifications
```toml
# ~/.config/hyprvoice/config.toml
[notifications]
enabled = false
type = "none"
```

### Option 2: Use Log Mode
```toml
[notifications]
type = "log"
```

### Option 3: Custom Notification Daemon

Implement your own notification handler:

```go
// Custom notifier example
type CustomNotifier struct {
    currentID int
}

func (c *CustomNotifier) Notify(title, message string) {
    // Your custom notification logic
    // e.g., update a status bar, desktop widget, etc.
}
```

---

## Debugging

### Check Dunst Version
```bash
dunst --version
```

Requires Dunst **v1.5.0+** for best replace ID support.

### Monitor Dunst Logs
```bash
# Terminal 1: Start dunst with logging
dunst -verbosity debug

# Terminal 2: Test notifications
notify-send -r 9999 -t 0 "Test" "Message 1"
notify-send -r 9999 -t 0 "Test" "Message 2"
```

### Verify Replace Behavior

You should see log output like:
```
DEBUG: Replacing notification with id 9999
DEBUG: Notification 9999 updated
```

---

## Technical Details

### Code Changes

**Before** (multiple notifications):
```go
func (Desktop) RecordingStarted() {
    exec.Command("notify-send", "Hyprvoice", "Recording Started").Run()
}

func (Desktop) Transcribing() {
    exec.Command("notify-send", "Hyprvoice", "Transcribing...").Run()
}
// Result: 2 separate notifications
```

**After** (single persistent notification):
```go
const RecordingNotificationID = 9999

func (d Desktop) RecordingStarted() {
    d.PersistentNotify("Hyprvoice", "🎤 Recording...", RecordingNotificationID)
}

func (d Desktop) Transcribing() {
    d.PersistentNotify("Hyprvoice", "⏳ Transcribing...", RecordingNotificationID)
}

func (Desktop) PersistentNotify(title, message string, replaceID int) {
    exec.Command("notify-send",
        "-a", "Hyprvoice",
        "-r", fmt.Sprintf("%d", replaceID),
        "-t", "0",
        title,
        message,
    ).Run()
}
// Result: 1 notification that morphs
```

### Why Replace ID 9999?

- High number (less likely to conflict)
- Easy to remember
- Not used by system notifications
- Consistent across all hyprvoice notifications

---

## Future Enhancements

Potential improvements:

1. **Notification Actions**
   ```bash
   notify-send -r 9999 \
       -a "Hyprvoice" \
       -t 0 \
       --action="cancel=Cancel Recording" \
       "Recording..."
   ```

2. **Progress Indicators**
   ```bash
   notify-send -r 9999 \
       -a "Hyprvoice" \
       -t 0 \
       -h int:value:75 \  # Progress bar
       "Transcribing..."
   ```

3. **Desktop Entry Integration**
   - Custom icon from desktop file
   - Better categorization
   - System tray integration

---

## Related Documentation

- [Dunst Documentation](https://dunst-project.org/documentation/)
- [freedesktop.org Notification Spec](https://specifications.freedesktop.org/notification-spec/latest/)
- [hyprvoice Configuration](../README.md#notifications)

---

*Last Updated: November 2025*
