package client

import (
	"crypto/tls"
	"fmt"
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// dialCredentials returns the transport credentials for the egress connection. Server verification is ALWAYS on —
// insecure plaintext exists ONLY for a literal localhost target, and connecting anywhere else with allowInsecure
// set is refused outright rather than silently falling back to TLS (a caller who typo'd a real hostname into
// --insecure should get a hard error, not a quietly-secure connection that masks the mistake).
func dialCredentials(egressAddress string, allowInsecure bool) (credentials.TransportCredentials, error) {
	if !allowInsecure {
		return credentials.NewTLS(&tls.Config{}), nil // nil RootCAs → the system trust store
	}

	if !isLocalhost(hostOnly(egressAddress)) {
		return nil, fmt.Errorf("--insecure is only permitted against localhost; refusing to connect to %q without TLS", egressAddress)
	}
	return insecure.NewCredentials(), nil
}

func hostOnly(hostPort string) string {
	if portSeparatorIndex := strings.LastIndex(hostPort, ":"); portSeparatorIndex >= 0 {
		return hostPort[:portSeparatorIndex]
	}
	return hostPort
}

func isLocalhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
