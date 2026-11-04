package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"text/tabwriter"

	"github.com/agentparley/tunnel/internal/apiclient"
)

func runAgents(args []string) error {
	flagSet := flag.NewFlagSet("agents", flag.ExitOnError)
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, active, err := signedInClient()
	if err != nil {
		return err
	}
	agents, err := client.ListAgents(ctx)
	if err != nil {
		return explainAPIError(err, active)
	}
	if len(agents) == 0 {
		fmt.Println("No agents yet.")
		return nil
	}

	table := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tSLUG\tID")
	for _, agent := range agents {
		name := agent.Name
		if !agent.IsEnabled {
			name += "  (off)"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\n", name, agent.Slug, agent.ID)
	}
	return table.Flush()
}

func runChats(args []string) error {
	flagSet := flag.NewFlagSet("chats", flag.ExitOnError)
	agentRef := flagSet.String("agent", "", "only chats with this agent (id, slug or name)")
	limit := flagSet.Int("limit", 20, "how many chats to list (1-100)")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, active, err := signedInClient()
	if err != nil {
		return err
	}
	agents, err := client.ListAgents(ctx)
	if err != nil {
		return explainAPIError(err, active)
	}
	agentID := ""
	if *agentRef != "" {
		agent, err := resolveAgent(agents, *agentRef)
		if err != nil {
			return err
		}
		agentID = agent.ID
	}
	chats, err := client.ListChats(ctx, agentID, *limit)
	if err != nil {
		return explainAPIError(err, active)
	}
	if len(chats) == 0 {
		fmt.Println("No chats from this computer yet. Start one with: agentparley chat <agent> \"<message>\"")
		return nil
	}

	agentNames := map[string]string{}
	for _, agent := range agents {
		agentNames[agent.ID] = agent.Name
	}
	table := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tAGENT\tTITLE\tLAST ACTIVITY")
	for _, chat := range chats {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", chat.ID, nameOrID(agentNames, chat), titleOrDash(chat), chat.LastActivityAt.Local().Format("2006-01-02 15:04"))
	}
	return table.Flush()
}

func nameOrID(agentNames map[string]string, chat apiclient.ChatSummary) string {
	if name, ok := agentNames[chat.AgentID]; ok {
		return name
	}
	return chat.AgentID
}

func titleOrDash(chat apiclient.ChatSummary) string {
	if chat.Title == nil || *chat.Title == "" {
		return "-"
	}
	return *chat.Title
}
