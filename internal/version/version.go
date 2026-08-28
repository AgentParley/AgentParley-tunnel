// Package version names the daemon's reported AgentVersion — sent on every Hello and on enrolment, and shown by
// `agentparley-tunnel status`. Release builds overwrite Version at link time:
//
//	-ldflags "-X github.com/agentparley/tunnel/internal/version.Version=<v>"
//
// so it must stay a var, not a const.
package version

var Version = "0.0.0-dev"
