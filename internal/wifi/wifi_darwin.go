package wifi

import "surfswarm/internal/protocol"

// current prefers wdutil, which reports signal, channel, and rate for any
// caller and the SSID and BSSID only for root with location access; the
// installed daemon runs as root. Names that come back redacted are filled
// from ipconfig when it will say, and the reading is kept either way.
// system_profiler is the fallback when wdutil is unavailable; it is slow
// but works for any user.
func current() (*protocol.WifiInfo, error) {
	if out, err := run("wdutil", "info"); err == nil {
		if info := ParseWdutil(out); info != nil {
			if info.SSID == "" || info.BSSID == "" {
				ifc := info.Interface
				if ifc == "" {
					ifc = "en0"
				}
				if sum, err := run("ipconfig", "getsummary", ifc); err == nil {
					ssid, bssid := ParseIpconfigSummary(sum)
					if info.SSID == "" {
						info.SSID = ssid
					}
					if info.BSSID == "" {
						info.BSSID = bssid
					}
				}
			}
			return info, nil
		}
	}
	out, err := run("system_profiler", "SPAirPortDataType", "-json")
	if err != nil {
		return nil, err
	}
	return ParseSystemProfiler(out), nil
}
