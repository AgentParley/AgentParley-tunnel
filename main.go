// Command agentparley-tunnel is the on-box daemon: it dials out to the SSH egress service and holds the
// connection open so AgentParley can run commands on this box with no inbound port and no firewall change. Seven
// verbs, stdlib flag (no cobra — seven verbs do not justify the dependency):
//
//	agentparley-tunnel login        — RFC 8628 device grant, generates this box's Ed25519 identity, enrols
//	agentparley-tunnel start        — runs the connect loop in the foreground (systemd's ExecStart)
//	agentparley-tunnel status       — prints whether credentials exist and what the config says
//	agentparley-tunnel logout       — deregisters and wipes local state
//	agentparley-tunnel register     — detects a local model harness and registers its models with the platform
//	agentparley-tunnel unregister   — removes this box's provider for one harness
//	agentparley-tunnel self-update  — checks for and installs a newer (or rolled-back) release (systemd's
//	                                  ExecStartPre, runs as root just before every start)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentparley/tunnel/internal/client"
	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/harness"
	"github.com/agentparley/tunnel/internal/login"
	"github.com/agentparley/tunnel/internal/selfupdate"
	"github.com/agentparley/tunnel/internal/shellrun"
	"github.com/agentparley/tunnel/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "login":
		err = runLogin(os.Args[2:])
	case "start":
		err = runStart(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "logout":
		err = runLogout(os.Args[2:])
	case "register":
		err = runRegister(os.Args[2:])
	case "unregister":
		err = runUnregister(os.Args[2:])
	case "self-update":
		err = runSelfUpdate(os.Args[2:])
	case "ws":
		err = runWs(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "agentparley-tunnel:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentparley-tunnel <login|start|status|logout|register|unregister|self-update|ws> [flags]")
}

func runLogin(args []string) error {
	flagSet := flag.NewFlagSet("login", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	tunnelConfig, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("login requires a config file (see install.sh): %w", err)
	}

	result, err := login.Run(tunnelConfig.Server.APIBaseURL, tunnelConfig.RunAs, func(prompt login.Prompt) {
		fmt.Printf("\nOpen %s and enter this code: %s\n\n", prompt.VerificationURI, prompt.UserCode)
		fmt.Println("Waiting for approval...")
	})
	if err != nil {
		return err
	}

	if err := credstore.New().Save(result.Credentials); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}

	fmt.Printf("Enrolled as ssh host %s. Enable the service with:\n  sudo systemctl enable --now agentparley-tunnel\n", result.Credentials.SSHHostID)
	return nil
}

func runStart(args []string) error {
	flagSet := flag.NewFlagSet("start", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	insecure := flagSet.Bool("insecure", false, "skip TLS verification — localhost only, refused for any other host")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	tunnelConfig, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	runAsUser, err := shellrun.ResolveUser(tunnelConfig.RunAs)
	if err != nil {
		return err
	}
	if err := shellrun.VerifyMatchesProcess(runAsUser); err != nil {
		return err
	}

	credentialStore := credstore.New()
	daemon := client.New(tunnelConfig.Server.APIBaseURL, tunnelConfig.Server.EgressAddress, *insecure, credentialStore, tunnelConfig, runAsUser)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return daemon.Run(ctx)
}

func runStatus(args []string) error {
	flagSet := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	fmt.Println("agentparley-tunnel", version.Version)

	credentials, err := credstore.New().Load()
	if err != nil {
		return err
	}
	if credentials == nil {
		fmt.Println("not logged in — run 'agentparley-tunnel login'")
		return nil
	}
	fmt.Printf("ssh host id: %s\n", credentials.SSHHostID)
	fmt.Printf("machine id:  %s\n", credentials.MachineID)

	tunnelConfig, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("config: %v\n", err)
		return nil
	}
	fmt.Printf("api:         %s\n", tunnelConfig.Server.APIBaseURL)
	fmt.Printf("egress:      %s\n", tunnelConfig.Server.EgressAddress)
	fmt.Printf("run_as:      %s\n", tunnelConfig.RunAs)
	fmt.Printf("enabled:     %v\n", tunnelConfig.Enabled)
	fmt.Printf("read_only:   %v\n", tunnelConfig.ReadOnly)
	return nil
}

func runLogout(args []string) error {
	flagSet := flag.NewFlagSet("logout", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	store := credstore.New()
	credentials, err := store.Load()
	if err != nil {
		return err
	}
	if credentials == nil {
		fmt.Println("not logged in")
		return nil
	}

	// A box that can't reach us must still be able to disarm itself, so the local wipe below always runs — a failed
	// revoke call is reported, not fatal to logout.
	tunnelConfig, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("could not load config, skipping server-side revoke: %v\n", err)
	} else if err := client.Revoke(&http.Client{Timeout: 10 * time.Second}, tunnelConfig.Server.APIBaseURL, credentials); err != nil {
		fmt.Printf("server-side revoke failed (this install may still be valid server-side — delete it from the portal if needed): %v\n", err)
	} else {
		fmt.Println("revoked server-side")
	}

	if err := store.Delete(); err != nil {
		return fmt.Errorf("wiping local credentials: %w", err)
	}
	fmt.Println("logged out — local credentials and key removed")
	return nil
}

func runRegister(args []string) error {
	flagSet := flag.NewFlagSet("register", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if flagSet.NArg() != 1 {
		return fmt.Errorf("usage: agentparley-tunnel register <harness>")
	}
	harnessName := flagSet.Arg(0)

	tunnelConfig, credentials, err := loadConfigAndCredentials(*configPath)
	if err != nil {
		return err
	}

	resolvedHarness, err := harness.Resolve(harnessName, tunnelConfig)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := resolvedHarness.Detect(ctx); err != nil {
		return fmt.Errorf("%s is not ready to register: %w", harnessName, err)
	}

	models, err := resolvedHarness.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("listing %s's models: %w", harnessName, err)
	}

	registerHarnessResult, err := client.RegisterHarness(&http.Client{Timeout: 30 * time.Second}, tunnelConfig.Server.APIBaseURL, credentials, harnessName, models)
	if err != nil {
		return err
	}

	fmt.Printf("Registered %s: %d model(s) now available in the AgentParley console.\n", harnessName, registerHarnessResult.ModelCount)
	return nil
}

func runUnregister(args []string) error {
	flagSet := flag.NewFlagSet("unregister", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if flagSet.NArg() != 1 {
		return fmt.Errorf("usage: agentparley-tunnel unregister <harness>")
	}
	harnessName := flagSet.Arg(0)

	tunnelConfig, credentials, err := loadConfigAndCredentials(*configPath)
	if err != nil {
		return err
	}

	if err := client.UnregisterHarness(&http.Client{Timeout: 30 * time.Second}, tunnelConfig.Server.APIBaseURL, credentials, harnessName); err != nil {
		return err
	}

	fmt.Printf("Unregistered %s.\n", harnessName)
	return nil
}

// loadConfigAndCredentials is the shared prologue for register/unregister: load the config, prove the DAEMON's own
// identity can drive a harness (run_as bounds a CLI harness's read-and-quote exposure to exactly that OS user's
// files, exactly like runStart does before it ever starts serving operations — skipping this would let
// `sudo agentparley-tunnel register claude` probe root's ~/.claude and register successfully, while every turn at
// invoke time, run as the daemon's real run_as user, fails with an unexplainable "is_error: true"), then load the
// stored credentials. unregister shares the run_as check for symmetry even though it never touches the harness.
func loadConfigAndCredentials(configPath string) (*config.Config, *credstore.Credentials, error) {
	tunnelConfig, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}

	runAsUser, err := shellrun.ResolveUser(tunnelConfig.RunAs)
	if err != nil {
		return nil, nil, err
	}
	if err := shellrun.VerifyMatchesProcess(runAsUser); err != nil {
		return nil, nil, err
	}

	credentials, err := credstore.New().Load()
	if err != nil {
		return nil, nil, err
	}
	if credentials == nil {
		return nil, nil, fmt.Errorf("not logged in — run 'agentparley-tunnel login' first")
	}

	return tunnelConfig, credentials, nil
}

// runSelfUpdate is systemd's ExecStartPre step, run as root just before every daemon start. A broken config must
// not block startup any more than a failed update would, so a config load failure logs and returns nil rather
// than propagating — the caller (systemd, via the unit's `-` prefix) starts the daemon regardless either way, but
// this keeps the exit code 0 on its own terms instead of relying on that prefix.
func runSelfUpdate(args []string) error {
	flagSet := flag.NewFlagSet("self-update", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	tunnelConfig, err := config.Load(*configPath)
	if err != nil {
		log.Printf("self-update: loading config %s: %v", *configPath, err)
		return nil
	}

	return selfupdate.Run(tunnelConfig)
}
