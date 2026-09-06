//go:build !linux && !darwin && !windows

package wifi

import "github.com/shin2344234/surfswarm/internal/protocol"

func current() (*protocol.WifiInfo, error) { return nil, nil }
