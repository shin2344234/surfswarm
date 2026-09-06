// Package wifi reads the wireless link an agent is on: SSID, BSSID, band,
// channel, signal, and negotiated rates. It shells out to the platform's
// own tool (iw on Linux, wdutil or system_profiler on macOS, netsh on
// Windows) and parses the text, so there is nothing to install on the
// device beyond what ships with the OS.
package wifi

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"surfswarm/internal/protocol"
)

// Current returns the active wireless link, or nil when the device has no
// connected Wi-Fi interface or the platform tool is unavailable.
func Current() (*protocol.WifiInfo, error) {
	return current()
}

// run executes a command with a short timeout and returns its stdout.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// kv splits "Key : Value" lines the way netsh and wdutil print them.
func kv(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// leadingNumber parses the number at the start of s, ignoring what follows
// ("-52 dBm", "866.7 MBit/s", "92%").
func leadingNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] == '-' || s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	return v, err == nil
}

// bandForChannel maps a channel number to a band when the frequency is
// unknown. Channels overlap between 5 GHz and 6 GHz, so callers that know
// the frequency should use bandForFreq instead.
func bandForChannel(ch int) string {
	switch {
	case ch >= 1 && ch <= 14:
		return "2.4"
	case ch >= 32 && ch <= 177:
		return "5"
	}
	return ""
}

func bandForFreq(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return "2.4"
	case mhz >= 5000 && mhz < 5900:
		return "5"
	case mhz >= 5925 && mhz < 7200:
		return "6"
	}
	return ""
}

func channelForFreq(mhz int) int {
	switch {
	case mhz == 2484:
		return 14
	case mhz >= 2412 && mhz <= 2472:
		return (mhz - 2407) / 5
	case mhz >= 5000 && mhz < 5900:
		return (mhz - 5000) / 5
	case mhz >= 5955 && mhz < 7200:
		return (mhz - 5950) / 5
	}
	return 0
}

// percentToDBm approximates Windows' signal quality percentage as dBm using
// the common linear mapping: 100% is -50 dBm, 0% is -100 dBm.
func percentToDBm(pct float64) int {
	if pct <= 0 {
		return -100
	}
	if pct >= 100 {
		return -50
	}
	return int(pct/2 - 100)
}
