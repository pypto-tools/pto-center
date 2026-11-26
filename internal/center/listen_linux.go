package center

import (
	"context"
	"net"
	"syscall"
)

func listenOnDevice(address, device string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var bind error
		err := c.Control(func(fd uintptr) {
			bind = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
		})
		if err != nil {
			return err
		}
		return bind
	}}
	return lc.Listen(context.Background(), "tcp", address)
}
