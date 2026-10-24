package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"github.com/danielbodart/gokit/httpx"
)

// DefaultAdminSocket is where treesinger-admin.socket listens: outside
// /run/treesinger on purpose, since that whole directory is bind-mounted into
// the game servers that mint through it.
const DefaultAdminSocket = "/run/treesinger-admin.sock"

const loginUsage = "login [--socket PATH]"

// Login is `treesinger login [--socket PATH]`, the one-time device login.
// Where a broker runs under systemd it logs in through the broker's admin
// socket, so the running broker takes the new account at once. Where there is
// no such socket (Docker) it runs the flow here, writing the store itself, and
// says which it is doing. That is a second writer beside any broker running on
// the same store, which its design rules out (see FileTokenStore): a refresh in
// flight there can put the old tokens back, and that broker keeps the old
// account's profiles. So it is safe only for a first login, before the broker
// holds any tokens, or with the broker stopped (docs/docker.md says so); it
// cannot tell which from here.
//
// A socket that is there but cannot be used (not root, the broker not
// answering) is an error, never a reason to fall back to that second writer.
// So is a --socket that is not there: naming one says a broker is expected.
// local wires the container for logging in here, called only then, since
// only then is the store's configuration needed.
func Login(ctx context.Context, args []string, defaultSocket string, out io.Writer, local func() (*Container, error)) error {
	line, err := parseOptionsOnly(loginUsage, args, "--socket")
	if err != nil {
		return err
	}
	socket, given := line.values["--socket"]
	if !given {
		socket = defaultSocket
		_, err := os.Stat(socket)
		if errors.Is(err, fs.ErrNotExist) {
			c, err := local()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "No broker admin socket at %s, so logging in directly to %s.\n", socket, c.Config.StorePath)
			return localLogin(ctx, c.OAuth, out)
		}
		if err != nil {
			return fmt.Errorf("cannot look for the broker's admin socket: %w", err)
		}
	}
	fmt.Fprintf(out, "Logging in through the broker at %s.\n", socket)
	return remoteLogin(ctx, &http.Client{Transport: httpx.UnixSocket(socket)}, socket, out)
}

// remoteLogin asks the broker at the far end of client to log in, and shows
// the operator its events as they come.
func remoteLogin(ctx context.Context, client *http.Client, socket string, out io.Writer) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, socketURL+"/v1/login", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		hint := ""
		if errors.Is(err, fs.ErrPermission) {
			hint = " (it is root's: sudo treesinger login)"
		}
		return fmt.Errorf("cannot use the broker's admin socket %s%s: %w", socket, hint, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return errors.New("the broker is already running a login; finish or abandon that one first")
	}
	if !succeeded(response) {
		data, _ := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
		return fmt.Errorf("the broker refused the login: %s (%s)", errorCode(data), response.Status)
	}
	lines := bufio.NewScanner(response.Body)
	for lines.Scan() {
		var event loginEvent
		if err := json.Unmarshal(lines.Bytes(), &event); err != nil {
			return fmt.Errorf("the broker sent an unreadable login event %q: %w", lines.Text(), err)
		}
		switch {
		case event.Error != "":
			return fmt.Errorf("login failed: %s", event.Error)
		case event.OK:
			fmt.Fprintln(out, "✓ Logged in. Refresh token stored, and the broker is using it now.")
			return nil
		case event.VerificationURIComplete != "":
			showPrompt(out, event.VerificationURIComplete, event.VerificationURI, event.UserCode)
		default:
			return fmt.Errorf("the broker sent an unexpected login event %q", lines.Text())
		}
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("the login stream broke off: %w", err)
	}
	return errors.New("the broker ended the login without saying whether it worked")
}

// showPrompt is what the operator does to approve a login.
func showPrompt(out io.Writer, complete, uri, code string) {
	fmt.Fprintf(out, "\nTo authorise Treesinger, visit:\n\n  %s\n", complete)
	fmt.Fprintf(out, "\nor go to %s and enter code: %s\n\n", uri, code)
	fmt.Fprintln(out, "Waiting for approval…")
}

// localLogin runs the device flow here, writing the store itself.
func localLogin(ctx context.Context, oauth *OAuthClient, out io.Writer) error {
	prompt := func(auth DeviceAuth) {
		showPrompt(out, auth.VerificationURIComplete, auth.VerificationURI, auth.UserCode)
	}
	if err := oauth.Login(ctx, prompt); err != nil {
		return err
	}
	fmt.Fprintln(out, "✓ Logged in. Refresh token stored. You can now `treesinger serve`.")
	return nil
}
