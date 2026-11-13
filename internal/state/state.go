package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Endpoint is the daemon's discovery record. It contains the loopback address
// and the bearer token required for every request.
type Endpoint struct {
	Addr      string    `json:"addr"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// Home resolves the per-user state directory. REA_SESSION_HOME overrides it,
// which keeps tests and parallel checkouts isolated.
func Home() (string, error) {
	if v := os.Getenv("REA_SESSION_HOME"); v != "" {
		return v, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "rea-session"), nil
}

// EndpointPath is the discovery file path.
func EndpointPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "endpoint.json"), nil
}

// IDARegistrationPath is the conventional IDA MCP registration location. A
// daemon started without REA_IDA_MCP_CONFIG picks this file up when it exists.
func IDARegistrationPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "ida-mcp.json"), nil
}

// LogPath is where a detached daemon writes its stderr.
func LogPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "daemon.log"), nil
}

// LockPath serializes concurrent daemon spawns.
func LockPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "daemon.lock"), nil
}

// Load reads the endpoint record. A missing file is reported as os.ErrNotExist.
func Load() (*Endpoint, error) {
	path, err := EndpointPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ep Endpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		return nil, fmt.Errorf("state: parse endpoint: %w", err)
	}
	if ep.Addr == "" || ep.Token == "" {
		return nil, errors.New("state: endpoint record is incomplete")
	}
	return &ep, nil
}

// Save writes the endpoint atomically with owner-only permissions.
func Save(ep *Endpoint) error {
	path, err := EndpointPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove deletes the discovery record if present.
func Remove() error {
	path, err := EndpointPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrorPath is where a daemon that failed before publishing its endpoint
// records the reason for the waiting CLI.
func ErrorPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "startup-error.json"), nil
}

type startupError struct {
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

// SaveError records a startup failure for the next CLI invocation.
func SaveError(message string) error {
	path, err := ErrorPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(startupError{Error: message, At: time.Now()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// LoadError returns the recorded startup failure, if any.
func LoadError() (string, error) {
	path, err := ErrorPath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var record startupError
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", err
	}
	return record.Error, nil
}

// ClearError removes a previous startup failure.
func ClearError() error {
	path, err := ErrorPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// NewToken returns a 256-bit hex bearer token.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
