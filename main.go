// Command agentparley is the on-box daemon: it dials out to the SSH egress service and holds the connection open
// so AgentParley can run commands on this box with no inbound port and no firewall change. Eight verbs, stdlib
// flag (no cobra — eight verbs do not justify the dependency):
//
//	agentparley login        — RFC 8628 device grant, generates this box's Ed25519 identity, enrols
//	agentparley start        — runs the connect loop in the foreground (systemd's ExecStart)
//	agentparley status       — prints whether credentials exist and what the config says
//	agentparley logout       — deregisters and wipes local state
//	agentparley register     — with no argument, scans and registers every available harness (codex,
//	                            claude, ollama, openai-compatible-local); register <harness> does just that
//	                            one. Either way, discovery (finding a CLI installed outside this process's
//	                            own PATH, or a local OpenAI-compatible server on a well-known port) runs
//	                            first — see internal/harness/discovery.go
//	agentparley unregister   — removes this box's provider for one harness
//	agentparley update       — checks for and installs a newer (or rolled-back) release, and restarts the
//	                            systemd unit if it was active; systemd's ExecStartPre runs this (as
//	                            `update --from-service`) as root just before every start
//	agentparley doctor       — read-only diagnosis: version, config, service, connectivity, clock, providers
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentparley/tunnel/internal/client"
	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/harness"
	"github.com/agentparley/tunnel/internal/login"
	"github.com/agentparley/tunnel/internal/policy"
	"github.com/agentparley/tunnel/internal/selfupdate"
	"github.com/agentparley/tunnel/internal/shellrun"
	"github.com/agentparley/tunnel/internal/version"
)

// systemdUnitName is the unit install.sh writes — shared by `update`'s post-install restart and `doctor`'s
// service check.
const systemdUnitName = "agentparley.service"

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
	case "update":
		err = runUpdate(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
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
		fmt.Fprintln(os.Stderr, "agentparley:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentparley <login|start|status|logout|register [harness]|unregister <harness>|update|doctor|ws> [flags]")
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

	fmt.Printf("Enrolled as ssh host %s. Enable the service with:\n  sudo systemctl enable --now agentparley\n", result.Credentials.SSHHostID)
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

	fmt.Println("agentparley", version.Version)

	credentials, err := credstore.New().Load()
	if err != nil {
		return err
	}
	if credentials == nil {
		fmt.Println("not logged in — run 'agentparley login'")
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
	if flagSet.NArg() > 1 {
		return fmt.Errorf("usage: agentparley register [harness]")
	}

	tunnelConfig, credentials, runAsUser, err := loadConfigAndCredentials(*configPath)
	if err != nil {
		return err
	}

	if flagSet.NArg() == 0 {
		return runRegisterScan(tunnelConfig, credentials, runAsUser)
	}
	harnessName := flagSet.Arg(0)
	if ok, reason := policy.New(tunnelConfig).HarnessDiscoverable(harnessName); !ok {
		return fmt.Errorf("%s: %s in config.yaml", harnessName, reason)
	}

	found, hint, err := harness.Discover(harnessName, tunnelConfig, runAsUser)
	if err != nil {
		return fmt.Errorf("discovering %s: %w", harnessName, err)
	}
	if !found {
		if hint == "" {
			hint = "not found on this box"
		}
		return fmt.Errorf("%s: %s", harnessName, hint)
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

// scanHarnessNames is the fixed order `register` (no argument) walks every run — matches the order they're listed
// throughout the README and config.yaml.
var scanHarnessNames = []string{harness.Codex, harness.Claude, harness.Ollama, harness.OpenAICompatibleLocal}

// runRegisterScan is `register` with no argument: discover → resolve → Detect → ListModels → RegisterHarness for
// every harness the policy's allow/deny lists don't exclude, printing one line per harness so an operator can see
// at a glance what this box actually offers without hand-editing config.yaml. It returns a non-zero-exit error only
// when NOTHING registered and at least one harness genuinely failed (as opposed to simply not being installed) —
// a box with no local model providers at all is a normal, exit-0 outcome, not a failure.
func runRegisterScan(tunnelConfig *config.Config, credentials *credstore.Credentials, runAsUser *shellrun.User) error {
	harnessPolicy := policy.New(tunnelConfig)
	httpClient := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()

	registeredCount, failedCount := 0, 0
	for _, harnessName := range scanHarnessNames {
		if ok, reason := harnessPolicy.HarnessDiscoverable(harnessName); !ok {
			fmt.Printf("– %-24s %s\n", harnessName, reason)
			continue
		}

		found, hint, err := harness.Discover(harnessName, tunnelConfig, runAsUser)
		if err != nil {
			failedCount++
			fmt.Printf("✗ %-24s %v\n", harnessName, err)
			continue
		}
		if !found {
			if hint == "" {
				hint = "not found"
			}
			fmt.Printf("– %-24s %s\n", harnessName, hint)
			continue
		}

		resolvedHarness, err := harness.Resolve(harnessName, tunnelConfig)
		if err != nil {
			failedCount++
			fmt.Printf("✗ %-24s %v\n", harnessName, err)
			continue
		}
		if err := resolvedHarness.Detect(ctx); err != nil {
			// ollama carries a built-in default url rather than a discovery step of its own — Detect's own
			// reachability check IS its presence signal, so a miss here reads as "not found" (the ordinary case
			// for a box with no ollama running), keeping a vanilla box in the "nothing found" exit-0 bucket below
			// rather than "installed but not ready".
			if harnessName == harness.Ollama {
				fmt.Printf("– %-24s not found: %v\n", harnessName, err)
				continue
			}
			failedCount++
			fmt.Printf("✗ %-24s installed but not ready: %v\n", harnessName, err)
			continue
		}

		models, err := resolvedHarness.ListModels(ctx)
		if err != nil {
			failedCount++
			fmt.Printf("✗ %-24s %v\n", harnessName, err)
			continue
		}

		if _, err := client.RegisterHarness(httpClient, tunnelConfig.Server.APIBaseURL, credentials, harnessName, models); err != nil {
			failedCount++
			fmt.Printf("✗ %-24s registering: %v\n", harnessName, err)
			continue
		}

		registeredCount++
		fmt.Printf("✓ %-24s registered — %s\n", harnessName, registeredSummary(models))
	}

	if registeredCount == 0 {
		if failedCount > 0 {
			return fmt.Errorf("%d harness(es) failed and none registered", failedCount)
		}
		fmt.Println("no local model providers were found on this box")
	}
	return nil
}

// registeredSummary is the scan's "registered — …" tail: a single model's own label (codex's one ChatGPT-account
// entry), or a plain count once there's more than one (ollama's pulled models, claude's opus/sonnet/haiku).
func registeredSummary(models []harness.Model) string {
	if len(models) == 1 {
		return models[0].Label
	}
	return fmt.Sprintf("%d models", len(models))
}

func runUnregister(args []string) error {
	flagSet := flag.NewFlagSet("unregister", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if flagSet.NArg() != 1 {
		return fmt.Errorf("usage: agentparley unregister <harness>")
	}
	harnessName := flagSet.Arg(0)

	tunnelConfig, credentials, _, err := loadConfigAndCredentials(*configPath)
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
// `sudo agentparley register claude` probe root's ~/.claude and register successfully, while every turn at
// invoke time, run as the daemon's real run_as user, fails with an unexplainable "is_error: true"), then load the
// stored credentials. unregister shares the run_as check for symmetry even though it never touches the harness. The
// returned runAsUser is what register's own discovery (internal/harness.Discover) asks a login shell on behalf of.
func loadConfigAndCredentials(configPath string) (*config.Config, *credstore.Credentials, *shellrun.User, error) {
	tunnelConfig, err := config.Load(configPath)
	if err != nil {
		return nil, nil, nil, err
	}

	runAsUser, err := shellrun.ResolveUser(tunnelConfig.RunAs)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := shellrun.VerifyMatchesProcess(runAsUser); err != nil {
		return nil, nil, nil, err
	}

	credentials, err := credstore.New().Load()
	if err != nil {
		return nil, nil, nil, err
	}
	if credentials == nil {
		return nil, nil, nil, fmt.Errorf("not logged in — run 'agentparley login' first")
	}

	return tunnelConfig, credentials, runAsUser, nil
}

// runUpdate is `agentparley update`: check the published version pointer and, if it differs, download, verify, and
// atomically swap in the new binary — see internal/selfupdate. Two callers need very different failure contracts,
// which is why there are two flags rather than one:
//
//   - A human typing `sudo agentparley update` always updates, config setting or not (typing the command IS the
//     explicit instruction), every failure is reported and the command exits non-zero, and the systemd unit (if
//     installed and currently active) is restarted afterward so the new binary actually takes effect. --no-restart
//     skips only that last step, e.g. ahead of a planned maintenance window.
//   - The systemd unit's own ExecStartPre passes --from-service: the ONE caller that honors `auto_update: false`,
//     and it implies --no-restart (ExecStart is about to (re)start the daemon right after) and CheckFailOpen's
//     log-and-swallow behavior (a broken update check must never block startup).
func runUpdate(args []string) error {
	flagSet := flag.NewFlagSet("update", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	noRestart := flagSet.Bool("no-restart", false, "install the update but don't restart the systemd unit")
	fromService := flagSet.Bool("from-service", false, "internal: invoked by the systemd unit's ExecStartPre — honors auto_update: false, implies --no-restart, and never fails the caller")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root: sudo agentparley update")
	}

	tunnelConfig, err := config.Load(*configPath)
	if err != nil {
		if *fromService {
			log.Printf("update: loading config %s: %v", *configPath, err)
			return nil
		}
		return err
	}

	if *fromService {
		selfupdate.CheckFailOpen(tunnelConfig)
		return nil
	}

	result, err := selfupdate.Check(tunnelConfig.UpdateServer)
	if err != nil {
		return fmt.Errorf("checking for update: %w", err)
	}
	if !result.Updated {
		fmt.Printf("already up to date (%s)\n", result.PreviousVersion)
		return nil
	}
	fmt.Printf("updated %s -> %s\n", result.PreviousVersion, result.LatestVersion)

	if *noRestart {
		return nil
	}
	return restartServiceIfActive()
}

// restartServiceIfActive restarts systemdUnitName only when it's currently active — a box that never enabled the
// service (a manual/dev checkout) or has it stopped on purpose shouldn't have `update` start it.
func restartServiceIfActive() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", systemdUnitName).Run(); err != nil {
		return nil // not installed, or installed but not active — nothing to restart
	}

	fmt.Printf("restarting %s...\n", systemdUnitName)
	restartCmd := exec.Command("systemctl", "restart", systemdUnitName)
	restartCmd.Stdout = os.Stdout
	restartCmd.Stderr = os.Stderr
	if err := restartCmd.Run(); err != nil {
		return fmt.Errorf("restarting %s: %w", systemdUnitName, err)
	}
	fmt.Printf("restarted %s\n", systemdUnitName)
	return nil
}
