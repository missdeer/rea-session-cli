package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"rea-session-cli/internal/state"
	"rea-session-cli/internal/textutil"
)

// StartConfig is what the CLI needs to launch a new daemon.
type StartConfig struct {
	ReaCommand  []string
	IdleTimeout time.Duration
	Version     string
}

// Ensure returns a live endpoint, starting a detached daemon when needed.
func Ensure(ctx context.Context, cfg StartConfig) (*state.Endpoint, error) {
	if len(cfg.ReaCommand) == 0 {
		return nil, fmt.Errorf("no REA command configured")
	}
	if _, err := exec.LookPath(cfg.ReaCommand[0]); err != nil {
		return nil, fmt.Errorf("REA command %q not found: %w", cfg.ReaCommand[0], err)
	}

	if ep, err := state.Load(); err == nil {
		if alive(ctx, ep) {
			return ep, nil
		}
		_ = state.Remove()
	}

	// A stale failure record from an earlier run must not fail this attempt.
	_ = state.ClearError()
	if err := withSpawnLock(func() error { return spawn(cfg) }); err != nil {
		return nil, err
	}

	// npx may need to fetch the package on first use, so allow a cold start.
	const readyTimeout = 90 * time.Second
	deadline := time.Now().Add(readyTimeout)
	for {
		// A daemon that dies before publishing leaves the precise reason here.
		if message, err := state.LoadError(); err == nil && message != "" {
			return nil, errors.New(message)
		}
		if ep, err := state.Load(); err == nil && alive(ctx, ep) {
			return ep, nil
		}
		if time.Now().After(deadline) {
			logPath, _ := state.LogPath()
			tail, _ := textutil.TailLines(logPath, 10)
			message := fmt.Sprintf("daemon did not become ready within %s; see %s", readyTimeout, logPath)
			if tail != "" {
				message += "\n--- daemon.log ---\n" + tail
			}
			return nil, errors.New(message)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Ping reports daemon health without touching the MCP session.
func Ping(ctx context.Context, ep *state.Endpoint) (*Response, error) {
	return Do(ctx, ep, Request{Op: OpPing})
}

// Stop asks the daemon to exit.
func Stop(ctx context.Context, ep *state.Endpoint) (*Response, error) {
	return Do(ctx, ep, Request{Op: OpStop})
}

// Do performs one request/response exchange over loopback TCP.
func Do(ctx context.Context, ep *state.Endpoint, req Request) (*Response, error) {
	if req.Token == "" {
		req.Token = ep.Token
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", ep.Addr)
	if err != nil {
		return nil, fmt.Errorf("daemon unreachable at %s: %w", ep.Addr, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func alive(ctx context.Context, ep *state.Endpoint) bool {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := Ping(pingCtx, ep)
	return err == nil
}

func spawn(cfg StartConfig) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, err := state.LogPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	idle := cfg.IdleTimeout
	if idle <= 0 {
		idle = 20 * time.Minute
	}

	args := []string{"daemon", "run", "--idle", idle.String(), "--version", version}
	for i, arg := range cfg.ReaCommand {
		if i == 0 {
			args = append(args, "--rea-cmd", arg)
		} else {
			args = append(args, "--rea-arg", arg)
		}
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), "REA_SESSION_DAEMON=1")
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	return cmd.Process.Release()
}

// withSpawnLock makes concurrent CLI invocations race benignly: one spawns,
// the rest fall through to polling for the published endpoint.
func withSpawnLock(fn func() error) error {
	path, err := state.LockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// A crashed spawn can leave the lock behind. Take it over once it is
		// clearly stale; otherwise another process is already starting.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
			f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
		if err != nil {
			return nil
		}
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(path)
	}()
	return fn()
}
