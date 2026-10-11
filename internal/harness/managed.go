package harness

import (
	"fmt"
	"log"

	"github.com/agentparley/tunnel/internal/config"
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

// resolveCLIModels applies the CLI family's "replaced wholesale" config rule: an empty overrides list means the
// built-in defaults; a non-empty list REPLACES them entirely (never merges), and every override's
// ContextWindowTokens must be > 0 — this package never emits an unknown window. The server family merges per model
// instead (see serverhelpers.go).
func resolveCLIModels(defaults []Model, overrides []config.HarnessModelConfig, harnessName string) ([]Model, error) {
	if len(overrides) == 0 {
		return defaults, nil
	}

	models := make([]Model, 0, len(overrides))
	for _, override := range overrides {
		if override.ContextWindowTokens <= 0 {
			return nil, fmt.Errorf("harnesses.%s.models[].context_window_tokens is required and must be > 0 for model %q", harnessName, override.ID)
		}
		label := override.Label
		if label == "" {
			label = override.ID
		}
		models = append(models, Model{ID: override.ID, Label: label, ContextWindowTokens: override.ContextWindowTokens})
	}
	return models, nil
}
