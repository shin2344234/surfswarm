package wifi

import "surfswarm/internal/protocol"

// current uses "netsh wlan show interfaces", which works for any user.
func current() (*protocol.WifiInfo, error) {
	out, err := run("netsh", "wlan", "show", "interfaces")
	if err != nil {
		return nil, err
	}
	return ParseNetsh(out), nil
}
