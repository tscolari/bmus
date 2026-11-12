//go:build !linux

package bridge

import "net"

// checkPeer is a no-op off Linux, where SO_PEERCRED isn't available; the
// socket's 0600 mode and the 0700 home keep other users out.
func checkPeer(net.Conn) error { return nil }
