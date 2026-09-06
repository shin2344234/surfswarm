package wifi

import (
	"encoding/json"
	"strings"

	"github.com/shin2344234/surfswarm/internal/protocol"
)

// ParseIWLink parses the output of "iw dev <if> link" (Linux). It returns
// nil when the interface is not associated.
func ParseIWLink(out string) *protocol.WifiInfo {
	info := &protocol.WifiInfo{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Not connected"):
			return nil
		case strings.HasPrefix(line, "Connected to "):
			rest := strings.TrimPrefix(line, "Connected to ")
			if i := strings.Index(rest, " "); i > 0 {
				rest = rest[:i]
			}
			info.BSSID = strings.ToLower(rest)
		case strings.HasPrefix(line, "SSID:"):
			info.SSID = strings.TrimSpace(strings.TrimPrefix(line, "SSID:"))
		case strings.HasPrefix(line, "freq:"):
			if v, ok := leadingNumber(strings.TrimPrefix(line, "freq:")); ok {
				info.FreqMHz = int(v)
				info.Band = bandForFreq(info.FreqMHz)
				info.Channel = channelForFreq(info.FreqMHz)
			}
		case strings.HasPrefix(line, "signal:"):
			if v, ok := leadingNumber(strings.TrimPrefix(line, "signal:")); ok {
				info.SignalDBm = int(v)
			}
		case strings.HasPrefix(line, "tx bitrate:"):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "tx bitrate:"))
			if v, ok := leadingNumber(rest); ok {
				info.TxMbps = v
			}
			info.PHY = phyFromBitrate(rest)
			if w := widthFromBitrate(rest); w > 0 {
				info.WidthMHz = w
			}
		case strings.HasPrefix(line, "rx bitrate:"):
			if v, ok := leadingNumber(strings.TrimPrefix(line, "rx bitrate:")); ok {
				info.RxMbps = v
			}
		}
	}
	if info.BSSID == "" && info.SSID == "" {
		return nil
	}
	return info
}

// phyFromBitrate guesses the generation from iw's rate description, e.g.
// "866.7 MBit/s VHT-MCS 9 80MHz short GI VHT-NSS 2".
func phyFromBitrate(s string) string {
	switch {
	case strings.Contains(s, "EHT"):
		return "11be"
	case strings.Contains(s, "HE-"):
		return "11ax"
	case strings.Contains(s, "VHT"):
		return "11ac"
	case strings.Contains(s, "MCS"):
		return "11n"
	}
	return ""
}

func widthFromBitrate(s string) int {
	for _, w := range []string{"320MHz", "160MHz", "80MHz", "40MHz", "20MHz"} {
		if strings.Contains(s, w) {
			v, _ := leadingNumber(w)
			return int(v)
		}
	}
	return 0
}

// ParseNetsh parses "netsh wlan show interfaces" (Windows). Windows reports
// signal as a percentage, converted here to an approximate dBm. It returns
// nil when no interface is connected.
func ParseNetsh(out string) *protocol.WifiInfo {
	var info *protocol.WifiInfo
	connected := false
	for _, raw := range strings.Split(out, "\n") {
		key, val, ok := kv(raw)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "name":
			// A new interface block starts. Keep the first connected one.
			if info != nil && connected {
				return info
			}
			info = &protocol.WifiInfo{Interface: val}
			connected = false
		case "state":
			connected = strings.EqualFold(val, "connected")
		case "ssid":
			if info != nil {
				info.SSID = val
			}
		case "bssid":
			if info != nil {
				info.BSSID = strings.ToLower(val)
			}
		case "radio type":
			if info != nil {
				info.PHY = strings.TrimPrefix(val, "802.")
			}
		case "band":
			if info != nil {
				switch {
				case strings.HasPrefix(val, "2.4"):
					info.Band = "2.4"
				case strings.HasPrefix(val, "5"):
					info.Band = "5"
				case strings.HasPrefix(val, "6"):
					info.Band = "6"
				}
			}
		case "channel":
			if info != nil {
				if v, ok := leadingNumber(val); ok {
					info.Channel = int(v)
					if info.Band == "" {
						info.Band = bandForChannel(info.Channel)
					}
				}
			}
		case "receive rate (mbps)":
			if info != nil {
				if v, ok := leadingNumber(val); ok {
					info.RxMbps = v
				}
			}
		case "transmit rate (mbps)":
			if info != nil {
				if v, ok := leadingNumber(val); ok {
					info.TxMbps = v
				}
			}
		case "signal":
			if info != nil {
				if v, ok := leadingNumber(val); ok {
					info.SignalDBm = percentToDBm(v)
				}
			}
		}
	}
	if info != nil && connected {
		return info
	}
	return nil
}

// ParseWdutil parses the WIFI section of "wdutil info" (macOS, needs root).
func ParseWdutil(out string) *protocol.WifiInfo {
	info := &protocol.WifiInfo{}
	inWifi := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "—") || strings.HasPrefix(line, "-") {
			continue
		}
		// Section headers are upper-case words on their own line.
		if !strings.Contains(line, ":") && line == strings.ToUpper(line) {
			inWifi = line == "WIFI"
			continue
		}
		if !inWifi {
			continue
		}
		key, val, ok := kv(line)
		if !ok {
			continue
		}
		switch key {
		case "Interface Name":
			info.Interface = val
		case "SSID":
			info.SSID = val
		case "BSSID":
			info.BSSID = strings.ToLower(val)
		case "RSSI":
			if v, ok := leadingNumber(val); ok {
				info.SignalDBm = int(v)
			}
		case "Noise":
			if v, ok := leadingNumber(val); ok {
				info.NoiseDBm = int(v)
			}
		case "Tx Rate":
			if v, ok := leadingNumber(val); ok {
				info.TxMbps = v
			}
		case "PHY Mode":
			info.PHY = val
		case "Channel":
			parseMacChannel(val, info)
		}
	}
	if info.SSID == "" && info.BSSID == "" && info.Channel == 0 {
		return nil
	}
	if info.SSID == "<redacted>" {
		info.SSID = ""
	}
	if info.BSSID == "<redacted>" {
		info.BSSID = ""
	}
	return info
}

// parseMacChannel reads wdutil's "5g44/80" form.
func parseMacChannel(val string, info *protocol.WifiInfo) {
	val = strings.TrimSpace(val)
	switch {
	case strings.HasPrefix(val, "2g"):
		info.Band = "2.4"
		val = val[2:]
	case strings.HasPrefix(val, "5g"):
		info.Band = "5"
		val = val[2:]
	case strings.HasPrefix(val, "6g"):
		info.Band = "6"
		val = val[2:]
	}
	ch, width := val, ""
	if i := strings.Index(val, "/"); i >= 0 {
		ch, width = val[:i], val[i+1:]
	}
	if v, ok := leadingNumber(ch); ok {
		info.Channel = int(v)
		if info.Band == "" {
			info.Band = bandForChannel(info.Channel)
		}
	}
	if v, ok := leadingNumber(width); ok {
		info.WidthMHz = int(v)
	}
}

// ParseIpconfigSummary picks the SSID and BSSID out of "ipconfig getsummary
// <if>" (macOS). Either comes back empty when redacted.
func ParseIpconfigSummary(out string) (ssid, bssid string) {
	for _, raw := range strings.Split(out, "\n") {
		key, val, ok := kv(raw)
		if !ok || val == "" || val == "<redacted>" {
			continue
		}
		switch key {
		case "SSID":
			ssid = val
		case "BSSID":
			bssid = strings.ToLower(val)
		}
	}
	return ssid, bssid
}

// ParseSystemProfiler reads the current network from
// "system_profiler SPAirPortDataType -json" (macOS, any user; SSID and BSSID
// are redacted without location access, the rest is present).
func ParseSystemProfiler(out string) *protocol.WifiInfo {
	var doc struct {
		Data []struct {
			Interfaces []struct {
				Name    string `json:"_name"`
				Current *struct {
					Name    string  `json:"_name"`
					BSSID   string  `json:"spairport_network_bssid"`
					Channel string  `json:"spairport_network_channel"`
					PHY     string  `json:"spairport_network_phymode"`
					Rate    float64 `json:"spairport_network_rate"`
					Signal  string  `json:"spairport_signal_noise"`
				} `json:"spairport_current_network_information"`
			} `json:"spairport_airport_interfaces"`
		} `json:"SPAirPortDataType"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil
	}
	for _, d := range doc.Data {
		for _, ifc := range d.Interfaces {
			cur := ifc.Current
			if cur == nil || cur.Channel == "" {
				continue
			}
			info := &protocol.WifiInfo{Interface: ifc.Name, TxMbps: cur.Rate, PHY: strings.TrimPrefix(cur.PHY, "802.")}
			if cur.Name != "" && cur.Name != "<redacted>" {
				info.SSID = cur.Name
			}
			if cur.BSSID != "" && cur.BSSID != "<redacted>" {
				info.BSSID = strings.ToLower(cur.BSSID)
			}
			// "44 (5GHz, 80MHz)"
			if v, ok := leadingNumber(cur.Channel); ok {
				info.Channel = int(v)
			}
			switch {
			case strings.Contains(cur.Channel, "2GHz"):
				info.Band = "2.4"
			case strings.Contains(cur.Channel, "5GHz"):
				info.Band = "5"
			case strings.Contains(cur.Channel, "6GHz"):
				info.Band = "6"
			}
			if i := strings.Index(cur.Channel, ", "); i >= 0 {
				if v, ok := leadingNumber(cur.Channel[i+2:]); ok {
					info.WidthMHz = int(v)
				}
			}
			// "-52 dBm / -90 dBm"
			parts := strings.Split(cur.Signal, "/")
			if v, ok := leadingNumber(parts[0]); ok {
				info.SignalDBm = int(v)
			}
			if len(parts) > 1 {
				if v, ok := leadingNumber(parts[1]); ok {
					info.NoiseDBm = int(v)
				}
			}
			return info
		}
	}
	return nil
}
