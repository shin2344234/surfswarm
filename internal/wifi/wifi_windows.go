package wifi

import "github.com/shin2344234/surfswarm/internal/protocol"

// current uses "netsh wlan show interfaces", which works for any user.
func current() (*protocol.WifiInfo, error) {
	out, err := run("netsh", "wlan", "show", "interfaces")
	if err != nil {
		return nil, err
	}
	return ParseNetsh(out), nil
}
