package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/apiclient"
	"github.com/agentparley/tunnel/internal/cliconfig"
)

const chatUsage = `usage: agentparley chat [--new | --chat <id>] <agent> "<message>"`

func runChat(args []string) error {
	flagSet := flag.NewFlagSet("chat", flag.ContinueOnError)
	forceNew := flagSet.Bool("new", false, "start a new chat instead of continuing the newest one")
	chatFlag := flagSet.String("chat", "", "continue this chat id")
	if err := flagSet.Parse(args); err != nil {
		return errors.New(chatUsage)
	}
	if flagSet.NArg() != 2 || (*forceNew && *chatFlag != "") {
		return errors.New(chatUsage)
	}
	agentRef, message := flagSet.Arg(0), flagSet.Arg(1)
	if strings.TrimSpace(message) == "" {
		return errors.New("the message is empty")
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
	agent, err := resolveAgent(agents, agentRef)
	if err != nil {
		return err
	}

	chatID := *chatFlag
	if chatID == "" && !*forceNew {
		newest, err := client.ListChats(ctx, agent.ID, 1)
		if err != nil {
			return explainAPIError(err, active)
		}
		if len(newest) == 1 {
			chatID = newest[0].ID
		}
	}

	var cursor *string
	if chatID == "" {
		chat, err := client.StartChat(ctx, agent.ID, message, newIdempotencyKey())
		if err != nil {
			return explainAPIError(err, active)
		}
		chatID = chat.ID
	} else {
		cursor, err = client.SendMessage(ctx, chatID, message)
		var apiErr *apiclient.Error
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == 404:
			return fmt.Errorf("chat %s not found (a computer can only reach the chats it started)", chatID)
		case errors.As(err, &apiErr):
			return explainAPIError(err, active)
		case err != nil:
			return errors.New("could not reach AgentParley; the message may or may not have been sent — check with 'agentparley chats'")
		}
	}

	fmt.Fprintf(os.Stderr, "Sent to %s. Waiting for the reply… (Ctrl-C stops waiting; the agent keeps working)\n", agent.Name)
	return printReply(ctx, client, active, chatID, cursor)
}

func printReply(ctx context.Context, client *apiclient.Client, active *cliconfig.Active, chatID string, cursor *string) error {
	for {
		page, err := client.Messages(ctx, chatID, cursor, 200)
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "Stopped waiting. The chat is %s; see it with 'agentparley chats'.\n", chatID)
			return nil
		}
		if err != nil {
			return explainAPIError(err, active)
		}

		for _, line := range page.Messages {
			switch line.Kind {
			case "agent":
				fmt.Println(line.Text)
				fmt.Println()
			case "user":
			default:
				fmt.Fprintf(os.Stderr, "[%s] %s\n", line.Kind, line.Text)
			}
		}
		if page.NextAfter != nil {
			cursor = page.NextAfter
		}
		if page.HasMore {
			continue
		}
		switch page.State {
		case "failed":
			return fmt.Errorf("the chat failed; open it in the console to see why (chat %s)", chatID)
		case "idle":
			return nil
		}

		timer := time.NewTimer(time.Duration(max(page.NextPollIntervalMs, 500)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func newIdempotencyKey() string {
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	return hex.EncodeToString(key)
}
