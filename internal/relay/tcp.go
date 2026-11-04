package relay

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"
)

const dialTimeout = 10 * time.Second

// DialTCP connects to host:port; the failure carries the wire code the egress shows the user.
func DialTCP(ctx context.Context, host string, port int) (net.Conn, *Error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		return conn, nil
	}
	var netErr net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return nil, &Error{CodeConnectRefused, "connection refused"}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return nil, &Error{CodeConnectTimedOut, "timed out connecting"}
	default:
		return nil, &Error{CodeConnectFailed, err.Error()}
	}
}
