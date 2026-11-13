// Package cli implements the rea-session command surface. The CLI never talks
// to REA directly: it forwards one request to the daemon, which owns the
// long-lived MCP connection and the provider session.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"rea-session-cli/internal/daemon"
	"rea-session-cli/internal/mcp"
	"rea-session-cli/internal/state"
	"rea-session-cli/internal/textutil"
)

// Options are process-level defaults supplied by main.
type Options struct {
	Version     string
	ReaCommand  []string
	IdleTimeout time.Duration
}

type app struct {
	out        io.Writer
	err        io.Writer
	version    string
	reaCommand []string
	idle       time.Duration
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Run dispatches one CLI invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer, opts Options) int {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 20 * time.Minute
	}

	fs := flag.NewFlagSet("rea-session", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reaCmd := fs.String("rea-cmd", "", "executable that starts the REA MCP server")
	var reaArgs stringList
	fs.Var(&reaArgs, "rea-arg", "argument passed to --rea-cmd (repeatable)")
	idle := fs.Duration("idle", opts.IdleTimeout, "daemon idle timeout")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	a := &app{
		out:        stdout,
		err:        stderr,
		version:    opts.Version,
		reaCommand: opts.ReaCommand,
		idle:       *idle,
	}
	if *reaCmd != "" {
		a.reaCommand = append([]string{*reaCmd}, reaArgs...)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		usage(stdout)
		return 0
	}
	switch rest[0] {
	case "daemon":
		return a.daemonCmd(rest[1:])
	case "tools":
		return a.toolsCmd(rest[1:])
	case "schema", "describe":
		return a.schemaCmd(rest[1:])
	case "call":
		return a.callCmd(rest[1:])
	case "open":
		return a.openCmd(rest[1:])
	case "close":
		return a.closeCmd(rest[1:])
	case "status":
		return a.statusCmd()
	case "doctor":
		return a.doctorCmd()
	case "version", "--version", "-v":
		fmt.Fprintf(a.out, "rea-session %s\n", a.version)
		return 0
	case "help", "-h", "--help":
		usage(a.out)
		return 0
	default:
		fmt.Fprintf(a.err, "rea-session: unknown command %q\n\n", rest[0])
		usage(a.err)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `rea-session - keep one REA MCP session warm behind a small CLI

Usage:
  rea-session [--rea-cmd EXE] [--rea-arg ARG]... [--idle 20m] <command> [args]

Commands:
  daemon start|stop|status   Manage the background daemon
  tools [--query Q] [--limit N] [--all] [--json]
                             List tool names (schemas stay out of context)
  schema <tool>              Print one tool's input schema
  call <tool> [--json JSON|@FILE|-] [--timeout 10m] [--out FILE] [--raw] [--pretty]
                             Invoke one tool through the warm session
  open <path> [--provider P] [--snapshot PATH]
                             Shortcut for open_binary
  close [--snapshot PATH] [--overwrite]
                             Shortcut for close_binary
  status                     Daemon status
  doctor                     Runtime, daemon, and log diagnostics
  version                    Print version

Environment:
  REA_SESSION_REA_CMD   command line used to start the MCP server
                        (default: "rea mcp", else "npx -y rea-agents mcp")
  REA_SESSION_HOME      daemon state directory

Exit codes:
  0 success   1 tool/isError   2 usage   3 daemon or protocol failure
`)
}

func defaultReaCommand() []string {
	if v := os.Getenv("REA_SESSION_REA_CMD"); v != "" {
		return strings.Fields(v)
	}
	if p, err := exec.LookPath("rea"); err == nil {
		return []string{p, "mcp"}
	}
	return []string{"npx", "-y", "rea-agents", "mcp"}
}

func (a *app) rea() []string {
	if len(a.reaCommand) > 0 {
		return a.reaCommand
	}
	return defaultReaCommand()
}

func (a *app) ensure(ctx context.Context) (*state.Endpoint, error) {
	return daemon.Ensure(ctx, daemon.StartConfig{
		ReaCommand:  a.rea(),
		IdleTimeout: a.idle,
		Version:     a.version,
	})
}

// ---------------------------------------------------------------- daemon

func (a *app) daemonCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.err, "rea-session daemon: expected start, stop, status, or run")
		return 2
	}
	switch args[0] {
	case "run":
		return a.daemonRun(args[1:])
	case "start":
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		ep, err := a.ensure(ctx)
		if err != nil {
			a.fail(err)
			return 3
		}
		resp, err := daemon.Ping(ctx, ep)
		if err != nil {
			a.fail(err)
			return 3
		}
		a.printStatus(resp.Server)
		return 0
	case "stop":
		return a.daemonStop()
	case "status":
		return a.statusCmd()
	default:
		fmt.Fprintf(a.err, "rea-session daemon: unknown subcommand %q\n", args[0])
		return 2
	}
}

func (a *app) daemonRun(args []string) int {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(a.err)
	exe := fs.String("rea-cmd", "", "MCP server executable")
	var reaArgs stringList
	fs.Var(&reaArgs, "rea-arg", "MCP server argument (repeatable)")
	idle := fs.Duration("idle", a.idle, "idle timeout")
	version := fs.String("version", a.version, "reported version")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	command := a.rea()
	if *exe != "" {
		command = append([]string{*exe}, reaArgs...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := daemon.Run(ctx, daemon.Config{
		ReaCommand:  command,
		IdleTimeout: *idle,
		Version:     *version,
		Logger:      newLogger(a.err),
	}); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

func (a *app) daemonStop() int {
	ep, err := state.Load()
	if err != nil {
		a.printJSON(map[string]any{"running": false, "home": homeOrEmpty()}, true)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := daemon.Stop(ctx, ep)
	if err != nil {
		_ = state.Remove()
		a.printJSON(map[string]any{"running": false, "stale": true, "home": homeOrEmpty()}, true)
		return 0
	}
	a.printJSON(map[string]any{
		"running": false,
		"stopped": true,
		"pid":     resp.Server.PID,
		"home":    homeOrEmpty(),
	}, true)
	return 0
}

func (a *app) statusCmd() int {
	ep, err := state.Load()
	if err != nil {
		a.printJSON(map[string]any{"running": false, "home": homeOrEmpty()}, true)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := daemon.Ping(ctx, ep)
	if err != nil {
		a.printJSON(map[string]any{"running": false, "stale": true, "addr": ep.Addr, "home": homeOrEmpty()}, true)
		return 0
	}
	a.printStatus(resp.Server)
	return 0
}

func (a *app) doctorCmd() int {
	command := a.rea()
	_, lookErr := exec.LookPath(command[0])
	view := map[string]any{
		"home":             homeOrEmpty(),
		"rea_command":      command,
		"executable_found": lookErr == nil,
		"running":          false,
	}
	if lookErr != nil {
		view["executable_error"] = lookErr.Error()
	}

	if ep, err := state.Load(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if resp, err := daemon.Ping(ctx, ep); err == nil {
			view["running"] = true
			view["daemon"] = resp.Server
		} else {
			view["stale_endpoint"] = true
		}
	}

	if logPath, err := state.LogPath(); err == nil {
		tail, tailErr := textutil.TailLines(logPath, 20)
		switch {
		case tailErr != nil:
			view["log_error"] = tailErr.Error()
		case tail != "":
			view["log_tail"] = tail
		}
	}

	a.printJSON(view, true)
	return 0
}

// ---------------------------------------------------------------- tools

func (a *app) toolsCmd(args []string) int {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	fs.SetOutput(a.err)
	query := fs.String("query", "", "case-insensitive words; all must match name or description")
	limit := fs.Int("limit", 50, "maximum rows when --all is absent")
	all := fs.Bool("all", false, "show every match")
	asJSON := fs.Bool("json", false, "emit full tool objects, including schemas")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ep, err := a.ensure(ctx)
	if err != nil {
		a.fail(err)
		return 3
	}
	resp, err := daemon.Do(ctx, ep, daemon.Request{Op: daemon.OpTools})
	if err != nil {
		a.fail(err)
		return 3
	}
	if !resp.OK {
		a.failf("%s: %s", resp.Code, resp.Error)
		return 3
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(resp.Result, &tools); err != nil {
		a.fail(err)
		return 3
	}
	matches := filterTools(tools, *query)
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	total := len(matches)
	if !*all && len(matches) > *limit {
		matches = matches[:*limit]
	}

	if *asJSON {
		a.printJSON(matches, true)
		return 0
	}
	for _, tool := range matches {
		fmt.Fprintf(a.out, "%-38s %s\n", tool.Name, oneLine(tool.Description, 110))
	}
	if len(matches) < total {
		fmt.Fprintf(a.err, "# %d of %d tools; raise --limit, use --all, or narrow --query\n", len(matches), total)
	}
	return 0
}

func filterTools(tools []mcp.Tool, query string) []mcp.Tool {
	words := strings.Fields(strings.ToLower(query))
	matches := make([]mcp.Tool, 0, len(tools))
	for _, tool := range tools {
		hay := strings.ToLower(tool.Name + " " + tool.Description)
		ok := true
		for _, word := range words {
			if !strings.Contains(hay, word) {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, tool)
		}
	}
	return matches
}

func (a *app) schemaCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.err, "rea-session schema: expected exactly one tool name")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ep, err := a.ensure(ctx)
	if err != nil {
		a.fail(err)
		return 3
	}
	resp, err := daemon.Do(ctx, ep, daemon.Request{Op: daemon.OpSchema, Tool: args[0]})
	if err != nil {
		a.fail(err)
		return 3
	}
	if !resp.OK {
		if resp.Code == daemon.CodeNotFound {
			a.failf("%s", resp.Error)
			return 2
		}
		a.failf("%s: %s", resp.Code, resp.Error)
		return 3
	}
	a.printJSON(json.RawMessage(resp.Result), true)
	return 0
}

// ---------------------------------------------------------------- call

type invokeOpts struct {
	timeout time.Duration
	outFile string
	raw     bool
	pretty  bool
}

func (a *app) callCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.err, "rea-session call: expected a tool name")
		return 2
	}
	name := args[0]
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(a.err)
	argJSON := fs.String("json", "", `arguments as JSON, @file, or - for stdin (default {})`)
	outFile := fs.String("out", "", "write the tool payload here and print a receipt")
	timeout := fs.Duration("timeout", 10*time.Minute, "tool timeout")
	raw := fs.Bool("raw", false, "print the text content instead of JSON")
	pretty := fs.Bool("pretty", false, "indent JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	argsRaw, err := readArgJSON(*argJSON)
	if err != nil {
		a.fail(err)
		return 2
	}
	return a.invoke(name, argsRaw, invokeOpts{
		timeout: *timeout,
		outFile: *outFile,
		raw:     *raw,
		pretty:  *pretty,
	})
}

func (a *app) openCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.err, "rea-session open: expected a target path")
		return 2
	}
	path := args[0]
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.SetOutput(a.err)
	provider := fs.String("provider", "", "provider id")
	snapshot := fs.String("snapshot", "", "snapshot path to import")
	timeout := fs.Duration("timeout", 10*time.Minute, "tool timeout")
	pretty := fs.Bool("pretty", false, "indent JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		a.fail(err)
		return 2
	}
	arguments := map[string]any{"path": abs}
	if *provider != "" {
		arguments["provider_id"] = *provider
	}
	if *snapshot != "" {
		snapAbs, err := filepath.Abs(*snapshot)
		if err != nil {
			a.fail(err)
			return 2
		}
		arguments["snapshot_path"] = snapAbs
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		a.fail(err)
		return 2
	}
	return a.invoke("open_binary", raw, invokeOpts{timeout: *timeout, pretty: *pretty})
}

func (a *app) closeCmd(args []string) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.SetOutput(a.err)
	snapshot := fs.String("snapshot", "", "snapshot path to save before closing")
	overwrite := fs.Bool("overwrite", false, "overwrite an existing snapshot")
	timeout := fs.Duration("timeout", 10*time.Minute, "tool timeout")
	pretty := fs.Bool("pretty", false, "indent JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	arguments := map[string]any{"overwrite": *overwrite}
	if *snapshot != "" {
		abs, err := filepath.Abs(*snapshot)
		if err != nil {
			a.fail(err)
			return 2
		}
		arguments["snapshot_path"] = abs
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		a.fail(err)
		return 2
	}
	return a.invoke("close_binary", raw, invokeOpts{timeout: *timeout, pretty: *pretty})
}

func (a *app) invoke(name string, args json.RawMessage, opts invokeOpts) int {
	if opts.timeout <= 0 {
		opts.timeout = 10 * time.Minute
	}
	// The socket deadline must outlast the daemon's own tool deadline.
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout+30*time.Second)
	defer cancel()

	ep, err := a.ensure(ctx)
	if err != nil {
		a.fail(err)
		return 3
	}
	resp, err := daemon.Do(ctx, ep, daemon.Request{
		Op:        daemon.OpCall,
		Tool:      name,
		Arguments: args,
		TimeoutMS: opts.timeout.Milliseconds(),
	})
	if err != nil {
		a.fail(err)
		return 3
	}
	if !resp.OK {
		a.failf("%s: %s", resp.Code, resp.Error)
		return 3
	}

	var result mcp.CallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		a.fail(err)
		return 3
	}
	payload, text := selectPayload(result)

	if opts.outFile != "" {
		data := payload
		if len(data) == 0 {
			data = []byte(text)
		}
		if err := os.WriteFile(opts.outFile, data, 0o600); err != nil {
			a.fail(err)
			return 3
		}
		a.printJSON(map[string]any{
			"ok":       !result.IsError,
			"out":      opts.outFile,
			"bytes":    len(data),
			"is_error": result.IsError,
		}, false)
		if result.IsError {
			return 1
		}
		return 0
	}

	switch {
	case opts.raw:
		if text != "" {
			fmt.Fprintln(a.out, text)
		} else if len(payload) > 0 {
			fmt.Fprintln(a.out, string(payload))
		}
	case len(payload) > 0:
		a.printJSON(json.RawMessage(payload), opts.pretty)
	case text != "":
		fmt.Fprintln(a.out, text)
	}

	if result.IsError {
		return 1
	}
	return 0
}

func selectPayload(result mcp.CallResult) (payload []byte, text string) {
	for _, content := range result.Content {
		if content.Type == "text" && content.Text != "" {
			text = content.Text
			break
		}
	}
	if len(result.StructuredContent) > 0 {
		payload = result.StructuredContent
	}
	if len(payload) == 0 && text != "" && json.Valid([]byte(text)) {
		payload = []byte(text)
	}
	return payload, text
}

func readArgJSON(value string) (json.RawMessage, error) {
	var raw []byte
	switch {
	case value == "":
		return json.RawMessage(`{}`), nil
	case value == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		raw = data
	case strings.HasPrefix(value, "@"):
		data, err := os.ReadFile(value[1:])
		if err != nil {
			return nil, err
		}
		raw = data
	default:
		raw = []byte(value)
	}
	if !json.Valid(raw) {
		return nil, errors.New("arguments are not valid JSON")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("arguments must be a JSON object")
	}
	return json.RawMessage(trimmed), nil
}

// ---------------------------------------------------------------- output

func (a *app) printStatus(status daemon.ServerStatus) {
	view := map[string]any{
		"running":        status.Running,
		"pid":            status.PID,
		"addr":           status.Addr,
		"rea":            status.ReaCommand,
		"protocol":       status.ProtocolVersion,
		"server":         status.ServerName + "/" + status.ServerVersion,
		"tools":          status.Tools,
		"tools_loaded":   status.ToolsLoaded,
		"uptime_seconds": status.UptimeSeconds,
	}
	if status.ServerName == "" {
		view["server"] = ""
	}
	a.printJSON(view, true)
}

func (a *app) printJSON(value any, pretty bool) {
	encoder := json.NewEncoder(a.out)
	encoder.SetEscapeHTML(false)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	_ = encoder.Encode(value)
}

func (a *app) fail(err error) {
	fmt.Fprintf(a.err, "rea-session: %v\n", err)
}

func (a *app) failf(format string, args ...any) {
	fmt.Fprintf(a.err, "rea-session: "+format+"\n", args...)
}

func homeOrEmpty() string {
	home, err := state.Home()
	if err != nil {
		return ""
	}
	return home
}

func oneLine(s string, limit int) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if limit > 0 && len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}

func newLogger(w io.Writer) *log.Logger {
	return log.New(w, "rea-session: ", log.LstdFlags)
}
