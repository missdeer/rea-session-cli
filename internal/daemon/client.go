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

// ReadyPolicy controls how long Ensure waits after the daemon answers.
type ReadyPolicy int

const (
	// RequireRunning returns as soon as the control socket answers, even while
	// the MCP handshake is still in progress.
	RequireRunning ReadyPolicy = iota
	// RequireReady waits until the MCP handshake finished.
	RequireReady
)

// Ensure returns a live endpoint, starting a detached daemon when needed.
func Ensure(ctx context.Context, cfg StartConfig, policy ReadyPolicy) (*state.Endpoint, error) {
	if len(cfg.ReaCommand) == 0 {
		return nil, fmt.Errorf("no REA command configured")
	}
	if _, err := exec.LookPath(cfg.ReaCommand[0]); err != nil {
		return nil, fmt.Errorf("REA command %q not found: %w", cfg.ReaCommand[0], err)
	}

	spawnNeeded := true
	if ep, err := state.Load(); err == nil {
		responded, ok, inspectErr := inspectEndpoint(ctx, ep, policy)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if ok {
			return ep, nil
		}
		if responded {
			// Alive but still starting: wait for it instead of spawning again.
			spawnNeeded = false
		} else {
			_ = state.Remove()
		}
	}

	if spawnNeeded {
		// A stale failure record from an earlier run must not fail this attempt.
		_ = state.ClearError()
		if err := withSpawnLock(func() error { return spawn(cfg) }); err != nil {
			return nil, err
		}
	}

	// npx may need to fetch the package on first use, so allow a cold start.
	// RequireRunning only waits for the control socket, which is published
	// before the handshake.
	readyTimeout := 20 * time.Second
	if policy == RequireReady {
		readyTimeout = 5 * time.Minute
	}
	deadline := time.Now().Add(readyTimeout)
	for {
		if ep, err := state.Load(); err == nil {
			_, ok, inspectErr := inspectEndpoint(ctx, ep, policy)
			if inspectErr != nil {
				return nil, inspectErr
			}
			if ok {
				return ep, nil
			}
		} else if message, err := state.LoadError(); err == nil && message != "" {
			// The daemon died before publishing its endpoint.
			return nil, errors.New(message)
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

// inspectEndpoint reports whether the daemon answered and whether it satisfies
// the requested readiness policy. An error means initialization permanently
// failed and the caller should stop waiting.
func inspectEndpoint(ctx context.Context, ep *state.Endpoint, policy ReadyPolicy) (responded, ok bool, err error) {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, pingErr := Ping(pingCtx, ep)
	if pingErr != nil {
		return false, false, nil
	}
	switch resp.Server.Phase {
	case "failed":
		message := resp.Server.InitError
		if message == "" {
			message = "MCP initialization failed"
		}
		return true, false, errors.New(message)
	case "", "ready":
		return true, true, nil
	default: // "starting"
		return true, policy == RequireRunning, nil
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
	cmd.Env = daemonEnv()
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	return cmd.Process.Release()
}

// daemonEnv builds the daemon environment. An explicit REA_IDA_MCP_CONFIG
// always wins; otherwise the conventional <home>/ida-mcp.json is injected so
// `rea-session open --provider ida` works without shell exports. The selected
// analysis provider is never forced here: `--provider ida` (or
// REA_ANALYSIS_PROVIDER) still decides whether IDA is used.
func daemonEnv() []string {
	env := append(os.Environ(), "REA_SESSION_DAEMON=1")
	if os.Getenv("REA_IDA_MCP_CONFIG") != "" {
		return env
	}
	path, err := state.IDARegistrationPath()
	if err != nil {
		return env
	}
	if _, err := os.Stat(path); err != nil {
		return env
	}
	return append(env, "REA_IDA_MCP_CONFIG="+path)
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
