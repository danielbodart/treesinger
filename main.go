// Command treesinger is a central authentication broker for Hytale dedicated
// servers. Subcommands of one binary:
//
//	treesinger serve    run the mint-on-request broker.
//	treesinger login    the one-time device login: through a running broker's
//	                    admin socket, else directly into the store.
//	treesinger exec     mint a session, then become a game server with it.
//	treesinger health   probe a running broker, for a container HEALTHCHECK.
//	treesinger version  what this build calls itself.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/danielbodart/treesinger/internal/broker"
)

const usage = `usage: treesinger <command>

  serve                                                  run the broker
  login [--socket PATH]                                  log in (once)
  exec [--socket PATH | --url URL] -- COMMAND [ARGS...]  run COMMAND with a session
  health [--socket PATH | --url URL]                     probe a running broker
  version                                                print the version`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	command := ""
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	ctx := context.Background()
	switch command {
	case "serve":
		if len(args) > 0 {
			return failed(&broker.UsageError{Usage: "serve"})
		}
		c, err := wire()
		if err != nil {
			return failed(err)
		}
		return failed(broker.Serve(c))
	case "login":
		return failed(broker.Login(ctx, args, broker.DefaultAdminSocket, os.Stdout, wire))
	case "exec":
		return failed(broker.Exec(ctx, args, broker.Environ(), os.Stderr, syscall.Exec))
	case "health":
		return failed(broker.Health(args, broker.Environ()))
	case "version", "--version":
		fmt.Println(broker.Version)
		return 0
	case "", "help", "--help", "-h":
		fmt.Println(usage)
		return 0
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

// wire is the production container, configured from the environment. Only
// the commands that need it build it, so `exec` and `health` run where there
// is no store to point at.
func wire() (*broker.Container, error) {
	c := &broker.Container{}
	return c, c.Wire()
}

// failed is the exit code for a command's result, saying why on stderr: 2 for
// a command line that does not parse, else 1.
func failed(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	if usage := (*broker.UsageError)(nil); errors.As(err, &usage) {
		return 2
	}
	return 1
}
