package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/harness"
	"github.com/agentparley/tunnel/internal/policy"
	"github.com/agentparley/tunnel/internal/selfupdate"
	"github.com/agentparley/tunnel/internal/shellrun"
	"github.com/agentparley/tunnel/internal/version"
)

// doctorNetworkTimeout bounds every network probe below (version pointer, API health, egress TLS) — a hung check
// must fail closed rather than leave an operator staring at a stuck `doctor` run.
const doctorNetworkTimeout = 5 * time.Second

// clockSkewFailureThreshold is how far local time may drift from the API's own clock before `doctor` calls it a
// problem — the credentials this daemon presents are time-sensitive (short-lived access tokens), so a clock this
// far off starts producing "token not yet valid"/"expired" failures that have nothing to do with the token itself.
const clockSkewFailureThreshold = 60 * time.Second

type checkOutcome int

const (
	outcomeOK checkOutcome = iota
	outcomeFail
	outcomeSkip
)

// runDoctor is `agentparley doctor`: read-only, runs as any user, never writes to disk or the network beyond the
// GETs/TLS-connects the checks themselves describe. Each check below prints its own line and reports whether it
// passed — main.go's contract elsewhere returns an error and lets main() print it; doctor instead owns its exit
// code directly (os.Exit(1) on any ✗) because printing a second "agentparley: <err>" line after nine check lines
// would look like a tenth, unexplained failure.
func runDoctor(args []string) error {
	flagSet := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := flagSet.String("config", config.DefaultPath, "path to config.yaml")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	tunnelConfig, configErr := config.Load(*configPath)

	anyFailed := false
	note := func(outcome checkOutcome) {
		if outcome == outcomeFail {
			anyFailed = true
		}
	}

	note(doctorVersion(tunnelConfig, configErr))
	note(doctorConfig(*configPath, tunnelConfig, configErr))
	runAsUser, isRunAsUser, runningAsOutcome := doctorRunningAs(tunnelConfig, configErr)
	note(runningAsOutcome)
	note(doctorLoggedIn(runAsUser, isRunAsUser))
	note(doctorService())
	apiOutcome, serverTime, haveServerTime := doctorAPIReachable(tunnelConfig, configErr)
	note(apiOutcome)
	note(doctorEgressReachable(tunnelConfig, configErr))
	note(doctorClock(serverTime, haveServerTime))
	note(doctorProviders(tunnelConfig, configErr, runAsUser, isRunAsUser))

	if anyFailed {
		os.Exit(1)
	}
	return nil
}

// printCheck is the one line format every check below shares: an icon, a left-padded label, and an optional
// detail — the fix hint for ✗, the skip reason for –, or a short confirmation for ✓.
func printCheck(icon, label, detail string) {
	if detail == "" {
		fmt.Printf("%s %s\n", icon, label)
		return
	}
	fmt.Printf("%s %-16s %s\n", icon, label, detail)
}

func doctorVersion(tunnelConfig *config.Config, configErr error) checkOutcome {
	updateServer := config.DefaultUpdateServer
	if configErr == nil {
		updateServer = tunnelConfig.UpdateServer
	}

	httpClient := &http.Client{Timeout: doctorNetworkTimeout}
	latest, err := selfupdate.FetchLatestVersion(httpClient, updateServer)
	if err != nil {
		printCheck("–", "version", fmt.Sprintf("could not reach %s: %v", updateServer, err))
		return outcomeSkip
	}
	if latest == version.Version {
		printCheck("✓", "version", fmt.Sprintf("%s (up to date)", version.Version))
		return outcomeOK
	}
	printCheck("✗", "version", fmt.Sprintf("%s installed, %s available — update available: run `sudo agentparley update`", version.Version, latest))
	return outcomeFail
}

func doctorConfig(configPath string, tunnelConfig *config.Config, configErr error) checkOutcome {
	if configErr != nil {
		printCheck("✗", "config", fmt.Sprintf("%s: %v", configPath, configErr))
		return outcomeFail
	}
	if _, err := shellrun.ResolveUser(tunnelConfig.RunAs); err != nil {
		printCheck("✗", "config", fmt.Sprintf("run_as %q: %v", tunnelConfig.RunAs, err))
		return outcomeFail
	}
	printCheck("✓", "config", configPath)
	return outcomeOK
}

// doctorRunningAs reports whether THIS invocation of doctor runs as the box's own configured run_as user — the
// credentials and providers checks below can only inspect that user's own files, so they degrade to "–" whenever
// this is false rather than silently reporting on the wrong user's state (or failing with a bare permission error).
func doctorRunningAs(tunnelConfig *config.Config, configErr error) (runAsUser *shellrun.User, isRunAsUser bool, outcome checkOutcome) {
	if configErr != nil {
		printCheck("–", "running as", "skipped — config unavailable, see config check")
		return nil, false, outcomeSkip
	}

	resolvedUser, err := shellrun.ResolveUser(tunnelConfig.RunAs)
	if err != nil {
		printCheck("–", "running as", fmt.Sprintf("skipped — could not resolve run_as %q, see config check", tunnelConfig.RunAs))
		return nil, false, outcomeSkip
	}

	if os.Geteuid() == resolvedUser.UID {
		printCheck("✓", "running as", resolvedUser.Username)
		return resolvedUser, true, outcomeOK
	}
	printCheck("✗", "running as", fmt.Sprintf("this run is uid %d, not run_as %q — some checks below need: sudo -u %s agentparley doctor", os.Geteuid(), resolvedUser.Username, resolvedUser.Username))
	return resolvedUser, false, outcomeFail
}

func doctorLoggedIn(runAsUser *shellrun.User, isRunAsUser bool) checkOutcome {
	if runAsUser == nil {
		printCheck("–", "logged in", "skipped — run_as user unknown, see config check")
		return outcomeSkip
	}
	if !isRunAsUser {
		printCheck("–", "logged in", fmt.Sprintf("skipped — run as: sudo -u %s agentparley doctor", runAsUser.Username))
		return outcomeSkip
	}

	credentialsPath := credstore.CredentialsPathFor(runAsUser.UID, runAsUser.Home)
	if _, err := os.Stat(credentialsPath); err != nil {
		if os.IsNotExist(err) {
			printCheck("✗", "logged in", fmt.Sprintf("no credentials for %s — run `sudo -u %s agentparley login`", runAsUser.Username, runAsUser.Username))
			return outcomeFail
		}
		printCheck("✗", "logged in", fmt.Sprintf("checking %s: %v", credentialsPath, err))
		return outcomeFail
	}
	printCheck("✓", "logged in", fmt.Sprintf("credentials present for %s", runAsUser.Username))
	return outcomeOK
}

func doctorService() checkOutcome {
	if _, err := exec.LookPath("systemctl"); err != nil {
		printCheck("–", "service", "skipped — systemctl not found, not a systemd host")
		return outcomeSkip
	}

	enabled := exec.Command("systemctl", "is-enabled", "--quiet", systemdUnitName).Run() == nil
	active := exec.Command("systemctl", "is-active", "--quiet", systemdUnitName).Run() == nil

	if enabled && active {
		printCheck("✓", "service", "enabled and active")
		return outcomeOK
	}
	if !enabled {
		printCheck("✗", "service", "not enabled — run `sudo systemctl enable --now agentparley`; check `journalctl -u agentparley -n 50`")
		return outcomeFail
	}
	printCheck("✗", "service", "enabled but not active — run `sudo systemctl enable --now agentparley`; check `journalctl -u agentparley -n 50`")
	return outcomeFail
}

// doctorAPIReachable also hands back the API's own clock (from the HTTP response's Date header) for doctorClock —
// the two are separate lines in the fixed check order (egress reachability runs between them), but the clock check
// has no independent way to learn the server's time other than the response this check already made.
func doctorAPIReachable(tunnelConfig *config.Config, configErr error) (outcome checkOutcome, serverTime time.Time, haveServerTime bool) {
	if configErr != nil {
		printCheck("–", "api reachable", "skipped — no config")
		return outcomeSkip, time.Time{}, false
	}

	url := strings.TrimSuffix(tunnelConfig.Server.APIBaseURL, "/") + "/health"
	ctx, cancel := context.WithTimeout(context.Background(), doctorNetworkTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		printCheck("✗", "api reachable", fmt.Sprintf("%s: %v", url, err))
		return outcomeFail, time.Time{}, false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		printCheck("✗", "api reachable", fmt.Sprintf("%s: %v — check network/firewall and server.api in config.yaml", url, err))
		return outcomeFail, time.Time{}, false
	}
	defer response.Body.Close()

	serverTime, haveServerTime = time.Time{}, false
	if parsed, err := http.ParseTime(response.Header.Get("Date")); err == nil {
		serverTime, haveServerTime = parsed, true
	}

	if response.StatusCode != http.StatusOK {
		printCheck("✗", "api reachable", fmt.Sprintf("%s returned HTTP %d", url, response.StatusCode))
		return outcomeFail, serverTime, haveServerTime
	}
	printCheck("✓", "api reachable", url)
	return outcomeOK, serverTime, haveServerTime
}

func doctorEgressReachable(tunnelConfig *config.Config, configErr error) checkOutcome {
	if configErr != nil {
		printCheck("–", "egress reachable", "skipped — no config")
		return outcomeSkip
	}

	dialer := &net.Dialer{Timeout: doctorNetworkTimeout}
	connection, err := tls.DialWithDialer(dialer, "tcp", tunnelConfig.Server.EgressAddress, &tls.Config{})
	if err != nil {
		printCheck("✗", "egress reachable", fmt.Sprintf("%s: %v — check network/firewall and server.egress in config.yaml", tunnelConfig.Server.EgressAddress, err))
		return outcomeFail
	}
	connection.Close()
	printCheck("✓", "egress reachable", tunnelConfig.Server.EgressAddress)
	return outcomeOK
}

func doctorClock(serverTime time.Time, haveServerTime bool) checkOutcome {
	if !haveServerTime {
		printCheck("–", "clock", "skipped — no server time available, see api reachable check")
		return outcomeSkip
	}

	skew := time.Since(serverTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > clockSkewFailureThreshold {
		printCheck("✗", "clock", fmt.Sprintf("local clock is %s off from the server — enable NTP: `timedatectl set-ntp true`", skew.Round(time.Second)))
		return outcomeFail
	}
	printCheck("✓", "clock", fmt.Sprintf("within %s of server time", skew.Round(time.Second)))
	return outcomeOK
}

// doctorProviders checks exactly the harnesses this box already knows about — whatever `register` has discovered
// (harnesses.json) plus whatever config.yaml configures explicitly — never running discovery itself (that writes
// to disk; doctor only reads). For each it re-resolves the harness and calls Detect, the same call `register` uses
// to decide ✓/✗, but stops there — it never calls RegisterHarness, so running doctor can never register anything.
func doctorProviders(tunnelConfig *config.Config, configErr error, runAsUser *shellrun.User, isRunAsUser bool) checkOutcome {
	if configErr != nil {
		printCheck("–", "providers", "skipped — no config")
		return outcomeSkip
	}
	if !isRunAsUser {
		printCheck("–", "providers", fmt.Sprintf("skipped — run as: sudo -u %s agentparley doctor", tunnelConfig.RunAs))
		return outcomeSkip
	}

	names, err := harnessesToCheck(tunnelConfig)
	if err != nil {
		printCheck("✗", "providers", fmt.Sprintf("reading harnesses.json: %v", err))
		return outcomeFail
	}
	if len(names) == 0 {
		printCheck("–", "providers", "skipped — none configured or discovered; run `agentparley register`")
		return outcomeSkip
	}

	fmt.Println("  (checking providers, this sends each CLI a tiny test prompt…)")

	harnessPolicy := policy.New(tunnelConfig)
	ctx := context.Background()
	anyFailed := false
	for _, name := range names {
		if ok, reason := harnessPolicy.HarnessDiscoverable(name); !ok {
			printCheck("–", name, reason)
			continue
		}

		resolvedHarness, err := harness.Resolve(name, tunnelConfig)
		if err != nil {
			anyFailed = true
			printCheck("✗", name, err.Error())
			continue
		}
		if err := resolvedHarness.Detect(ctx); err != nil {
			anyFailed = true
			printCheck("✗", name, fmt.Sprintf("%v — re-run `agentparley register`", err))
			continue
		}
		printCheck("✓", name, "ready")
	}

	if anyFailed {
		return outcomeFail
	}
	return outcomeOK
}

// harnessesToCheck is the union of harness names explicitly configured in config.yaml and names `register` has
// already discovered and persisted — the set `doctor` probes, sorted for stable output across runs.
func harnessesToCheck(tunnelConfig *config.Config) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for name := range tunnelConfig.Harnesses {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	discovered, err := harness.DiscoveredNames()
	if err != nil {
		return nil, err
	}
	for _, name := range discovered {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	sort.Strings(names)
	return names, nil
}
