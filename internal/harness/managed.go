package harness

import (
	"log"

	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/managedcli"
)

// manifestSource is where a CLI harness reads the pinned CLI builds from: the daemon's refreshing pin list when it
// has one, and otherwise (register, doctor) the persisted signed copy or the one embedded in this binary.
func manifestSource(pins *managedcli.Refresher) func() managedcli.Manifest {
	if pins != nil {
		return pins.Current
	}
	standalone := managedcli.NewRefresher(credstore.StateDir(), log.Default())
	return standalone.Current
}
