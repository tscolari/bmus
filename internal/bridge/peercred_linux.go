package bridge

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer refuses a connection from any uid but the bridge's own effective
// uid, read with SO_PEERCRED. The socket's 0600 mode and the 0700 home
// already keep other users out; this is defence in depth.
func checkPeer(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var (
		cred *unix.Ucred
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("read peer credentials: %w", cerr)
	}
	if int(cred.Uid) != os.Geteuid() {
		return fmt.Errorf("peer uid %d (pid %d) is not the bridge's uid %d", cred.Uid, cred.Pid, os.Geteuid())
	}
	return nil
}
