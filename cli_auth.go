package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"time"

	"github.com/agentparley/tunnel/internal/apiclient"
	"github.com/agentparley/tunnel/internal/cliconfig"
	"github.com/agentparley/tunnel/internal/devicegrant"
	"github.com/agentparley/tunnel/internal/version"
)

const authUsage = "usage: agentparley auth <login|logout|status>"

func runAuth(args []string) error {
	if len(args) == 0 {
		return errors.New(authUsage)
	}
	switch args[0] {
	case "login":
		return runAuthLogin(args[1:])
	case "logout":
		return runAuthLogout(args[1:])
	case "status":
		return runAuthStatus(args[1:])
	}
	return errors.New(authUsage)
}

type authTokenResponse struct {
	APIKey *struct {
		Secret             string    `json:"secret"`
		ID                 string    `json:"id"`
		Name               string    `json:"name"`
		Prefix             string    `json:"prefix"`
		Scopes             []string  `json:"scopes"`
		MonthlySpendCapUsd float64   `json:"monthlySpendCapUsd"`
		ExpiresAt          time.Time `json:"expiresAt"`
		ManageURL          string    `json:"manageUrl"`
	} `json:"apiKey"`
}

func runAuthLogin(args []string) error {
	flagSet := flag.NewFlagSet("auth login", flag.ExitOnError)
	serverFlag := flagSet.String("server", "", "AgentParley API base URL")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	saved, err := cliconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Note:", err)
	}
	server := *serverFlag
	if server == "" {
		server = cliconfig.Server(saved)
	}

	if os.Getenv("AGENTPARLEY_API_KEY") != "" {
		fmt.Fprintln(os.Stderr, "Note: AGENTPARLEY_API_KEY is set and is used instead of this sign-in while it stays set.")
	}
	if saved != nil {
		fmt.Fprintf(os.Stderr, "Already signed in with %q (%s…). Signing in again makes a new key; the old one keeps working until it expires on %s or you revoke it at %s.\n",
			saved.KeyName, saved.KeyPrefix, saved.ExpiresAt.Local().Format(dateLayout), saved.ManageURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	username := ""
	if current, err := user.Current(); err == nil {
		username = current.Username
	}
	httpClient := &http.Client{Timeout: 15 * time.Second}
	code, err := devicegrant.Issue(ctx, httpClient, server, devicegrant.LocalRequest(devicegrant.PurposeCLI, username))
	if err != nil {
		return err
	}

	fmt.Printf("To sign in, open this page and approve the code %s:\n\n  %s\n\nWaiting for approval… (the code works for %d minutes)\n",
		code.UserCode, code.VerificationURIComplete, max(int(time.Until(code.ExpiresAt).Round(time.Minute)/time.Minute), 1))

	var token authTokenResponse
	err = devicegrant.Poll(ctx, httpClient, server, code, &token)
	switch {
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled")
	case errors.Is(err, devicegrant.ErrExpired):
		return errors.New("the code expired. Run 'agentparley auth login' again")
	case errors.Is(err, devicegrant.ErrDenied):
		return errors.New("the sign-in was refused in the browser")
	case errors.Is(err, devicegrant.ErrAlreadyUsed):
		return errors.New("that code was already used. Run 'agentparley auth login' again")
	case err != nil:
		return err
	}
	if token.APIKey == nil {
		return errors.New("the server did not return a key — is it up to date?")
	}

	key := token.APIKey
	if err := cliconfig.Save(&cliconfig.SignIn{
		Server:    server,
		APIKey:    key.Secret,
		KeyID:     key.ID,
		KeyName:   key.Name,
		KeyPrefix: key.Prefix,
		ExpiresAt: key.ExpiresAt,
		ManageURL: key.ManageURL,
	}); err != nil {
		return err
	}

	fmt.Printf("Signed in. This computer's key is %q (%s…): it may spend up to $%.2f a month and stops working on %s.\n",
		key.Name, key.Prefix, key.MonthlySpendCapUsd, key.ExpiresAt.Local().Format(dateLayout))
	fmt.Printf("Manage or revoke it at %s\n", key.ManageURL)
	return nil
}

func runAuthLogout(args []string) error {
	flagSet := flag.NewFlagSet("auth logout", flag.ExitOnError)
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	saved, err := cliconfig.Load()
	if err != nil {
		return err
	}
	if saved == nil {
		fmt.Println("Not signed in.")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	revokeErr := apiclient.New(cliconfig.Server(saved), saved.APIKey).DeleteSelf(ctx)

	if err := cliconfig.Delete(); err != nil {
		return err
	}

	var apiErr *apiclient.Error
	switch {
	case revokeErr == nil, errors.As(revokeErr, &apiErr) && apiErr.Status == http.StatusUnauthorized:
		fmt.Printf("Signed out: the key %q (%s…) was deleted and removed from this computer.\n", saved.KeyName, saved.KeyPrefix)
	default:
		fmt.Printf("Signed out: removed the key from this computer, but could not delete it on the server (%v).\n", revokeErr)
		fmt.Printf("The key %q (%s…) still works until %s; revoke it now at %s\n",
			saved.KeyName, saved.KeyPrefix, saved.ExpiresAt.Local().Format(dateLayout), saved.ManageURL)
	}
	return nil
}

func runAuthStatus(args []string) error {
	flagSet := flag.NewFlagSet("auth status", flag.ExitOnError)
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	fmt.Println("agentparley", version.Version)
	saved, err := cliconfig.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Note:", err)
	}
	if environmentKey := os.Getenv("AGENTPARLEY_API_KEY"); environmentKey != "" {
		fmt.Printf("using the key in AGENTPARLEY_API_KEY (%s…) against %s\n", firstChars(environmentKey, 17), cliconfig.Server(saved))
		return nil
	}
	if saved == nil {
		fmt.Println("not signed in — run 'agentparley auth login'")
		return nil
	}

	expiry := saved.ExpiresAt.Local().Format(dateLayout)
	if time.Now().After(saved.ExpiresAt) {
		expiry += " (expired)"
	}
	path, _ := cliconfig.Path()
	fmt.Printf("signed in with %q (%s…)\nserver: %s\nexpires: %s\nstored in: %s\nmanage: %s\n",
		saved.KeyName, saved.KeyPrefix, cliconfig.Server(saved), expiry, path, saved.ManageURL)
	return nil
}

func firstChars(value string, count int) string {
	if len(value) <= count {
		return value
	}
	return value[:count]
}
