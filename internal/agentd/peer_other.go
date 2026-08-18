//go:build !linux

package agentd

import "net"

const peerCredSupported = false

func peerUID(net.Conn) (uint32, bool) { return 0, false }
