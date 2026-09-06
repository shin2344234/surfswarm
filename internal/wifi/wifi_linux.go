package wifi

import (
	"os"

	"surfswarm/internal/protocol"
)

// current uses iw, which ships with Raspberry Pi OS and most distributions.
// It works without root for the fields we need.
func current() (*protocol.WifiInfo, error) {
	for _, ifc := range wirelessInterfaces() {
		out, err := run("iw", "dev", ifc, "link")
		if err != nil {
			return nil, err
		}
		if info := ParseIWLink(out); info != nil {
			info.Interface = ifc
			return info, nil
		}
	}
	return nil, nil
}

// wirelessInterfaces lists interfaces that have a wireless directory in
// sysfs, falling back to the usual names.
func wirelessInterfaces() []string {
	var out []string
	entries, err := os.ReadDir("/sys/class/net")
	if err == nil {
		for _, e := range entries {
			if _, err := os.Stat("/sys/class/net/" + e.Name() + "/wireless"); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	if len(out) == 0 {
		out = []string{"wlan0", "wlp2s0", "wlp3s0"}
	}
	return out
}
