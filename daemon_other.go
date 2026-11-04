//go:build !linux

package main

import "fmt"

var daemonVerbs = map[string]bool{"login": true, "start": true, "status": true, "logout": true, "register": true, "unregister": true, "update": true, "doctor": true, "ws": true}

func runDaemonCommand(verb string, args []string) (bool, error) {
	if !daemonVerbs[verb] {
		return false, nil
	}
	hint := ""
	if verb == "login" || verb == "logout" || verb == "status" {
		hint = fmt.Sprintf(" To sign in from this computer, use 'agentparley auth %s'.", verb)
	}
	return true, fmt.Errorf("'agentparley %s' connects a Linux computer to AgentParley and runs only on Linux.%s", verb, hint)
}
