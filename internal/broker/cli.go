package broker

import (
	"net/http"
	"slices"
	"strings"
)

// UsageError is a command line that does not parse. main shows the usage it
// carries and exits 2, the conventional status for a misused command, and the
// command itself runs no further.
type UsageError struct{ Usage string }

func (e *UsageError) Error() string { return "usage: treesinger " + e.Usage }

func usageError(usage string) error { return &UsageError{Usage: usage} }

// commandLine is a parsed command line: `--name VALUE` options, then, after
// an optional `--`, the rest.
type commandLine struct {
	values   map[string]string
	dashDash bool
	rest     []string
}

// parseCommandLine reads `--name VALUE` (or `--name=VALUE`) options, only
// those named in allowed, each at most once and never empty, up to a `--`,
// which it consumes, or the end. Anything else is a usage error: there are no
// positional arguments before `--`, so a mistyped option is never taken for
// one.
func parseCommandLine(usage string, args []string, allowed ...string) (commandLine, error) {
	parsed := commandLine{values: map[string]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			parsed.dashDash, parsed.rest = true, args[i+1:]
			return parsed, nil
		}
		name, value, inline := strings.Cut(arg, "=")
		if !slices.Contains(allowed, name) {
			return commandLine{}, usageError(usage)
		}
		if _, repeated := parsed.values[name]; repeated {
			return commandLine{}, usageError(usage)
		}
		if !inline {
			if i+1 >= len(args) {
				return commandLine{}, usageError(usage)
			}
			i++
			value = args[i]
		}
		if value == "" {
			return commandLine{}, usageError(usage)
		}
		parsed.values[name] = value
	}
	return parsed, nil
}

// parseOptionsOnly is parseCommandLine for a command that takes nothing but
// its options.
func parseOptionsOnly(usage string, args []string, allowed ...string) (commandLine, error) {
	parsed, err := parseCommandLine(usage, args, allowed...)
	if err == nil && (parsed.dashDash || len(parsed.rest) > 0) {
		return commandLine{}, usageError(usage)
	}
	return parsed, err
}

// broker is where a client command finds the broker: `--socket PATH` or
// `--url URL`, never both, as a client and the base URL to use with it. With
// neither it is the socket at defaultSocket, or "" when the command has a
// default of its own.
func (c commandLine) broker(usage, defaultSocket string) (client *http.Client, base, where string, err error) {
	socket, url := c.values["--socket"], c.values["--url"]
	if socket != "" && url != "" {
		return nil, "", "", usageError(usage)
	}
	if url != "" {
		return &http.Client{}, strings.TrimRight(url, "/"), url, nil
	}
	if socket == "" {
		socket = defaultSocket
	}
	if socket == "" {
		return nil, "", "", nil
	}
	return &http.Client{Transport: unixTransport(socket)}, socketURL, socket, nil
}
