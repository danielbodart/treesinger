package broker

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Inherited is a listening socket systemd opened for `serve`, by the name its
// socket unit gave it (FileDescriptorName=, else "unknown").
type Inherited struct {
	Name     string
	Listener net.Listener
}

// AdminName is the descriptor name that marks the admin socket. Only a socket
// systemd hands over under this name is ever served the admin handler.
const AdminName = "admin"

// listenFdsStart is the first descriptor systemd passes: the ones after
// stdin, stdout and stderr.
const listenFdsStart = 3

// Activated is the listeners systemd passed, by its socket-activation
// protocol (sd_listen_fds_with_names), written out here rather than taken as a
// dependency: LISTEN_FDS descriptors from fd 3, named by the colon-separated
// LISTEN_FDNAMES, meant only for the process whose pid is LISTEN_PID. Anything
// else, a variable inherited by a child for one, is not an activation, and
// nil is returned. The pid and the descriptors come in as arguments, so a
// test hands in listeners of its own; file is os.NewFile in production.
func Activated(env Env, pid int, file func(fd uintptr, name string) *os.File) ([]Inherited, error) {
	if env["LISTEN_PID"] == "" && env["LISTEN_FDS"] == "" {
		return nil, nil
	}
	target, err := strconv.Atoi(env["LISTEN_PID"])
	if err != nil {
		return nil, fmt.Errorf("LISTEN_PID must be a pid, not %q", env["LISTEN_PID"])
	}
	if target != pid {
		return nil, nil
	}
	count, err := strconv.Atoi(env["LISTEN_FDS"])
	if err != nil || count < 0 {
		return nil, fmt.Errorf("LISTEN_FDS must be a count of descriptors, not %q", env["LISTEN_FDS"])
	}
	names := make([]string, count)
	for i := range names {
		names[i] = "unknown"
	}
	if value := env["LISTEN_FDNAMES"]; value != "" {
		if names = strings.Split(value, ":"); len(names) != count {
			return nil, fmt.Errorf("LISTEN_FDNAMES names %d descriptors but LISTEN_FDS passes %d", len(names), count)
		}
	}
	var inherited []Inherited
	for i, name := range names {
		listener, err := listenerOn(file(uintptr(listenFdsStart+i), name))
		if err != nil {
			closeAll(inherited)
			return nil, fmt.Errorf("descriptor %d (%s) from systemd: %w", listenFdsStart+i, name, err)
		}
		inherited = append(inherited, Inherited{Name: name, Listener: listener})
	}
	return inherited, nil
}

// listenerOn is the listener over a passed descriptor. net.FileListener
// works on a duplicate (close-on-exec, so a child never inherits it), and the
// original is closed. A socket that is not listening is refused: Accept=yes
// would pass a connection, which this does not serve.
func listenerOn(file *os.File) (net.Listener, error) {
	if file == nil {
		return nil, fmt.Errorf("no such descriptor")
	}
	defer file.Close()
	conn, err := file.SyscallConn()
	if err != nil {
		return nil, err
	}
	accepting, sockErr := 0, error(nil)
	if err := conn.Control(func(fd uintptr) {
		accepting, sockErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	}); err != nil {
		return nil, err
	}
	if sockErr != nil {
		return nil, fmt.Errorf("not a socket: %w", sockErr)
	}
	if accepting == 0 {
		return nil, fmt.Errorf("not a listening socket (is the socket unit Accept=yes?)")
	}
	return net.FileListener(file)
}

func closeAll(inherited []Inherited) {
	for _, each := range inherited {
		each.Listener.Close()
	}
}

// listenEnv is the socket-activation variables, which `serve` clears once
// read, so that nothing it might start mistakes them for its own.
var listenEnv = []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"}

// InheritedListeners is Activated for this process, its variables then
// cleared from the environment.
func InheritedListeners() ([]Inherited, error) {
	inherited, err := Activated(Environ(), os.Getpid(), os.NewFile)
	for _, name := range listenEnv {
		os.Unsetenv(name)
	}
	return inherited, err
}

// Notify is how `serve` tells a supervisor it is listening.
type Notify func(env Env) error

// SystemdNotify is readiness for systemd `Type=notify`: sd_notify's "READY=1",
// sent as one datagram to $NOTIFY_SOCKET (a path, or an abstract socket when it
// starts with '@'). Sent by the main process itself, so it needs no helper and
// no NotifyAccess=all. Without NOTIFY_SOCKET (Docker, a shell) it does
// nothing.
func SystemdNotify(env Env) error {
	path := env["NOTIFY_SOCKET"]
	if path == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	return nil
}
