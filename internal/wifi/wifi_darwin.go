package wifi

import "surfswarm/internal/protocol"

// current prefers wdutil, which reports everything but needs root (the
// installed daemon runs as root). Without root, macOS redacts SSID and
// BSSID, so fall back to system_profiler, which still gives channel,
// signal, and rate.
func current() (*protocol.WifiInfo, error) {
	if out, err := run("wdutil", "info"); err == nil {
		if info := ParseWdutil(out); info != nil && info.SSID != "" {
			return info, nil
		}
	}
	out, err := run("system_profiler", "SPAirPortDataType", "-json")
	if err != nil {
		return nil, err
	}
	return ParseSystemProfiler(out), nil
}
