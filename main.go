// Command agentparley is two programs in one binary.
//
// Computer connection (Linux only): the on-box daemon dials out to the SSH egress service and holds the connection
// open so AgentParley can run commands on this box with no inbound port.
//
//	agentparley login        — RFC 8628 device grant, generates this box's Ed25519 identity, enrols
//	agentparley start        — runs the connect loop in the foreground (systemd's ExecStart)
//	agentparley status       — prints whether credentials exist and what the config says
//	agentparley logout       — deregisters and wipes local state
//	agentparley register     — registers every available harness, or just the named one
//	agentparley unregister   — removes this box's provider for one harness
//	agentparley update       — checks for and installs a newer (or rolled-back) release
//	agentparley doctor       — read-only diagnosis: version, config, service, connectivity, clock, providers
//
// Command-line use (any OS): sign a person in from a terminal and talk to their agents through the API.
//
//	agentparley auth login   — device sign-in; saves an API key in the user's config folder
//	agentparley auth logout  — deletes the key on the server, then the saved copy
//	agentparley auth status  — shows the saved sign-in
//	agentparley agents       — lists the agents
//	agentparley chat         — sends a message to an agent and prints the reply
//	agentparley chats        — lists the chats this computer started
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "auth":
		err = runAuth(os.Args[2:])
	case "chat":
		err = runChat(os.Args[2:])
	case "agents":
		err = runAgents(os.Args[2:])
	case "chats":
		err = runChats(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		var ok bool
		if ok, err = runDaemonCommand(os.Args[1], os.Args[2:]); !ok {
			usage()
			os.Exit(2)
		}
	}

	if err != nil {
		var reported *reportedError
		if !errors.As(err, &reported) {
			fmt.Fprintln(os.Stderr, "agentparley:", err)
		}
		os.Exit(1)
	}
}

// reportedError is a failure the command has already printed in full; main only sets the exit code.
type reportedError struct{}

func (*reportedError) Error() string {
	return "failed"
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: agentparley <auth login|auth logout|auth status|chat <agent> "<message>"|agents|chats> [flags]`)
	fmt.Fprintln(os.Stderr, "       agentparley <login|start|status|logout|register [harness]|unregister <harness>|update|doctor|ws> [flags]   (Linux: connect this computer)")
}
