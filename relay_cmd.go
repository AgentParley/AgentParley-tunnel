package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/relay"
	"github.com/agentparley/tunnel/internal/shellrun"
)

const relayTakesEffect = "Takes effect on the next connection; no restart needed."

func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this changes /etc/agentparley/config.yaml; run it with sudo")
	}
	return nil
}

func runAllow(args []string) error {
	return runAllowChange("allow", args, relay.Allow, "Allowed relays to %s.", "%s was already allowed.")
}

func runDisallow(args []string) error {
	return runAllowChange("disallow", args, relay.Disallow, "No longer allowing relays to %s.", "%s was not in the allow list.")
}

func runAllowChange(name string, args []string, change func(path, entry string) (bool, error), changedText, unchangedText string) error {
	flagSet := flag.NewFlagSet(name, flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if flagSet.NArg() != 1 {
		return fmt.Errorf("usage: agentparley %s <host:port>", name)
	}
	if err := requireRoot(); err != nil {
		return err
	}
	entry := flagSet.Arg(0)
	changed, err := change(*configPath, entry)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf(unchangedText+"\n", entry)
		return nil
	}
	fmt.Printf(changedText+" "+relayTakesEffect+"\n", entry)
	return nil
}

func runProgram(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agentparley program add <name> [--env KEY=VALUE]... -- <command> [args...] | program remove <name>")
	}
	switch args[0] {
	case "add":
		return runProgramAdd(args[1:])
	case "remove":
		return runProgramRemove(args[1:])
	}
	return fmt.Errorf("unknown program command %q (add or remove)", args[0])
}

func runProgramRemove(args []string) error {
	flagSet := flag.NewFlagSet("program remove", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if flagSet.NArg() != 1 {
		return fmt.Errorf("usage: agentparley program remove <name>")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	name := flagSet.Arg(0)
	changed, err := relay.RemoveProgram(*configPath, name)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("There is no program named %s.\n", name)
		return nil
	}
	fmt.Printf("Removed program %s. %s\n", name, relayTakesEffect)
	return nil
}

func runProgramAdd(args []string) error {
	configPath := config.DefaultPath
	env := map[string]string{}
	var name string
	var command []string

	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			command = args[index+1:]
			index = len(args)
		case argument == "--env" || argument == "--config":
			if index+1 >= len(args) {
				return fmt.Errorf("%s needs a value", argument)
			}
			index++
			if argument == "--config" {
				configPath = args[index]
			} else if err := addEnvPair(env, args[index]); err != nil {
				return err
			}
		case strings.HasPrefix(argument, "--env="):
			if err := addEnvPair(env, strings.TrimPrefix(argument, "--env=")); err != nil {
				return err
			}
		case strings.HasPrefix(argument, "--config="):
			configPath = strings.TrimPrefix(argument, "--config=")
		case strings.HasPrefix(argument, "-"):
			return fmt.Errorf("unknown flag %q", argument)
		case name == "":
			name = argument
		default:
			return fmt.Errorf("put the program's command after --")
		}
	}
	if name == "" || len(command) == 0 {
		return fmt.Errorf("usage: agentparley program add <name> [--env KEY=VALUE]... -- <command> [args...]")
	}
	if err := config.ValidateProgramName(name); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}

	tunnelConfig, err := config.Load(configPath)
	if err != nil {
		return err
	}
	home := ""
	if runAsUser, err := shellrun.ResolveUser(tunnelConfig.RunAs); err == nil {
		home = runAsUser.Home
	}

	if !strings.Contains(command[0], "/") {
		resolved, err := relay.ResolveCommand(command[0], env["PATH"], os.Getenv("PATH"), home)
		if err != nil {
			return err
		}
		command[0] = resolved
	}
	if _, hasPath := env["PATH"]; !hasPath {
		env["PATH"] = relay.DefaultProgramPath(command[0])
	}

	program := config.RelayProgram{Command: command, Env: env}
	if _, err := relay.AddProgram(configPath, name, program); err != nil {
		return err
	}
	fmt.Printf("Saved program %s: %s\n%s\n", name, strings.Join(command, " "), relayTakesEffect)
	return nil
}

func addEnvPair(env map[string]string, pair string) error {
	key, value, found := strings.Cut(pair, "=")
	if !found || key == "" {
		return fmt.Errorf("--env needs KEY=VALUE, got %q", pair)
	}
	env[key] = value
	return nil
}

// printRelayStatus lists what this computer lets AgentParley relay to; env values are never printed.
func printRelayStatus(tunnelConfig *config.Config) {
	allow := tunnelConfig.Relay.Allow
	if len(allow) == 0 {
		fmt.Println("relay allow: (none)")
	}
	for _, entry := range allow {
		fmt.Printf("relay allow: %s\n", entry)
	}
	names := make([]string, 0, len(tunnelConfig.Relay.Programs))
	for name := range tunnelConfig.Relay.Programs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("relay programs: (none)")
	}
	for _, name := range names {
		program := tunnelConfig.Relay.Programs[name]
		keys := make([]string, 0, len(program.Env))
		for key := range program.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Printf("relay program %s: %s (env keys: %s)\n", name, strings.Join(program.Command, " "), strings.Join(keys, ", "))
	}
}
