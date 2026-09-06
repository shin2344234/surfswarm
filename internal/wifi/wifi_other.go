//go:build !linux && !darwin && !windows

package wifi

import "surfswarm/internal/protocol"

func current() (*protocol.WifiInfo, error) { return nil, nil }
