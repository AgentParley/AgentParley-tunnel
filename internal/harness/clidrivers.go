package harness

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/agentparley/tunnel/internal/claudepty"
	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/codexrpc"
	"github.com/agentparley/tunnel/internal/managedcli"
)

// CLIDriver starts one CLI's sessions and owns the processes that outlive a single conversation.
type CLIDriver interface {
	Factory(ctx context.Context, model string, request *cliround.Request, budgets cliround.Budgets) (cliround.Runner, *cliround.Result, error)
	OneShot(ctx context.Context, model string, request *cliround.Request) (*cliround.Result, error)
	Shutdown()
}

// cliDefinition is everything the CLI family keeps per CLI: its wire name, how many warm sessions a box holds by
// default, and how its driver is built.
type cliDefinition struct {
	name           string
	defaultMaxWarm int
	newDriver      func(manifest func() managedcli.Manifest, logger *log.Logger) CLIDriver
}

var cliDefinitions = []cliDefinition{
	{name: Claude, defaultMaxWarm: 8, newDriver: func(manifest func() managedcli.Manifest, logger *log.Logger) CLIDriver {
		return claudepty.NewDriver(manifest, logger)
	}},
	{name: Codex, defaultMaxWarm: 32, newDriver: func(manifest func() managedcli.Manifest, logger *log.Logger) CLIDriver {
		return codexrpc.NewDriver(manifest, logger)
	}},
}

// IsCLI reports whether harnessName is one of the CLIs the daemon runs itself.
func IsCLI(harnessName string) bool {
	for _, definition := range cliDefinitions {
		if definition.name == harnessName {
			return true
		}
	}
	return false
}

// DefaultMaxWarmSessions is each CLI's warm-session cap when config does not set one.
func DefaultMaxWarmSessions() map[string]int {
	caps := map[string]int{}
	for _, definition := range cliDefinitions {
		caps[definition.name] = definition.defaultMaxWarm
	}
	return caps
}

// CLIDrivers is the daemon's one driver per CLI, built once and closed once.
type CLIDrivers struct {
	byName map[string]CLIDriver
}

func NewCLIDrivers(pins *managedcli.Refresher, logger *log.Logger) *CLIDrivers {
	manifest := manifestSource(pins)
	drivers := &CLIDrivers{byName: map[string]CLIDriver{}}
	for _, definition := range cliDefinitions {
		drivers.byName[definition.name] = definition.newDriver(manifest, logger)
	}
	return drivers
}

// For returns the named CLI's driver, or nil when there is none (the receiver is nil in register and doctor).
func (c *CLIDrivers) For(harnessName string) CLIDriver {
	if c == nil {
		return nil
	}
	return c.byName[harnessName]
}

// Close stops every driver's long-lived processes.
func (c *CLIDrivers) Close() {
	for _, driver := range c.byName {
		driver.Shutdown()
	}
}

// CLIDeps is what the CLI family needs from the running daemon. The zero value (register, doctor) can detect a CLI and
// list its models but cannot run a conversation round.
type CLIDeps struct {
	Sessions *cliround.Manager
	Pins     *managedcli.Refresher
	Drivers  *CLIDrivers
}

// runCLIRound runs one round through the session manager and returns the encoded result. The manager keeps the usage
// it carried into the result until the encoding has succeeded.
func runCLIRound(ctx context.Context, sessions *cliround.Manager, driver CLIDriver, harnessName, model string, request *cliround.Request) (InvokeOutcome, error) {
	if sessions == nil || driver == nil {
		return InvokeOutcome{}, errors.New("this process has no CLI session manager")
	}
	var encoded []byte
	err := sessions.Round(ctx, harnessName, model, request, driver.Factory, driver.OneShot, func(result *cliround.Result) error {
		if result.Calls == nil {
			result.Calls = []cliround.Call{}
		}
		if result.Events == nil {
			result.Events = []cliround.Event{}
		}
		var marshalErr error
		encoded, marshalErr = json.Marshal(result)
		return marshalErr
	})
	if err != nil {
		return InvokeOutcome{}, err
	}
	return InvokeOutcome{Payload: string(encoded)}, nil
}
