// Command treesinger is a central authentication broker for Hytale dedicated
// servers. Subcommands of one binary, sharing one container:
//
//	treesinger login    the one-time device login; writes the refresh token.
//	treesinger serve    run the mint-on-request broker.
//	treesinger health   probe a running broker, for a container HEALTHCHECK.
//	treesinger version  what this build calls itself.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/danielbodart/treesinger/internal/broker"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "login", "serve", "health":
	case "version", "--version":
		fmt.Println(broker.Version)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: treesinger <login|serve|health|version>")
		if command != "" {
			return 1
		}
		return 0
	}

	c := &broker.Container{}
	if err := c.Wire(); err != nil {
		return failed(err)
	}
	switch command {
	case "login":
		return failed(login(c))
	case "serve":
		return failed(broker.Serve(c))
	default:
		return broker.Health(*c.Config)
	}
}

// failed is the exit code for a command's result, saying why on stderr.
func failed(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

// login is §1–2: run the device flow, print the code, poll, persist the
// refresh token.
func login(c *broker.Container) error {
	ctx := context.Background()
	auth, err := c.OAuth.DeviceAuth(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nTo authorise Treesinger, visit:\n\n  %s\n", auth.VerificationURIComplete)
	fmt.Printf("\nor go to %s and enter code: %s\n\n", auth.VerificationURI, auth.UserCode)
	fmt.Println("Waiting for approval…")
	if _, err := c.OAuth.PollToken(ctx, auth.DeviceCode, time.Duration(auth.Interval)*time.Second); err != nil {
		return err
	}
	fmt.Println("✓ Logged in. Refresh token stored. You can now `treesinger serve`.")
	return nil
}
