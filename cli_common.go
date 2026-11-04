package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/apiclient"
	"github.com/agentparley/tunnel/internal/cliconfig"
)

const dateLayout = "2 Jan 2006"

func signedInClient() (*apiclient.Client, *cliconfig.Active, error) {
	active, err := cliconfig.Resolve()
	if err != nil {
		return nil, nil, err
	}
	return apiclient.New(active.Server, active.APIKey), active, nil
}

func resolveAgent(agents []apiclient.Agent, ref string) (apiclient.Agent, error) {
	for _, agent := range agents {
		if agent.ID == ref {
			return agent, nil
		}
	}
	for _, agent := range agents {
		if strings.EqualFold(agent.Slug, ref) {
			return agent, nil
		}
	}
	var named []apiclient.Agent
	for _, agent := range agents {
		if strings.EqualFold(agent.Name, ref) {
			named = append(named, agent)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
		return apiclient.Agent{}, fmt.Errorf("no agent called %q — run 'agentparley agents'", ref)
	}
	slugs := make([]string, len(named))
	for i, agent := range named {
		slugs[i] = agent.Slug
	}
	return apiclient.Agent{}, fmt.Errorf("%d agents are called %q; use a slug: %s", len(named), ref, strings.Join(slugs, ", "))
}

// explainAPIError turns an API refusal into the sentence a person can act on; other errors pass through.
func explainAPIError(err error, active *cliconfig.Active) error {
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Status {
	case 401:
		switch {
		case active.FromEnvironment:
			return errors.New("AGENTPARLEY_API_KEY was refused (unknown, revoked or expired)")
		case active.Saved != nil && time.Now().After(active.Saved.ExpiresAt):
			return fmt.Errorf("your sign-in expired on %s. Run 'agentparley auth login'", active.Saved.ExpiresAt.Local().Format(dateLayout))
		default:
			return errors.New("this computer's key was revoked. Run 'agentparley auth login'")
		}
	case 402:
		if apiErr.Code == "api_key_spend_cap_reached" && active.Saved != nil && active.Saved.ManageURL != "" {
			return fmt.Errorf("%s Raise the limit at %s", apiErr.Message, active.Saved.ManageURL)
		}
		return errors.New(apiErr.Message)
	case 403:
		if apiErr.Code == "api_scope_missing" {
			return errors.New(apiErr.Message)
		}
	}
	return apiErr
}
