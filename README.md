# rea-session

A small Go CLI that keeps one REA MCP session warm behind a local daemon.
The model sees **one shell tool**, not hundreds of tool schemas.

```
agent ──Bash──> rea-session CLI ──loopback TCP──> daemon ──stdio JSON-RPC──> rea mcp
                                                   │
                                                   └─ holds the provider session
                                                      (Ghidra / IDA / JADX / Frida / ...)
```

## Why

An MCP server advertises every tool definition to the client. REA v6.9 exposes
roughly 140 tools; with the default `full` catalog profile their schemas are
part of the context window before any work starts.

This project keeps the same REA runtime but changes where the tool surface
lives:

| Concern | MCP | rea-session |
| --- | --- | --- |
| Tool schemas in context | every advertised tool | none; fetched on demand |
| Provider session | warm inside the server | warm inside the daemon |
| Tool discovery | `tools/list` | `rea-session tools --query ...` |
| Schema lookup | client-side | `rea-session schema <tool>` |
| Large results | client result budget | `--out FILE` + receipt |

The daemon is not a reimplementation of REA. It is an MCP client that owns the
stdio connection so the expensive parts — provider startup, analysis import,
debugger attach, investigation history — survive between CLI invocations.

## Requirements

- Go 1.23+ to build (no third-party modules)
- A REA runtime: `rea` on `PATH`, or Node/npx for `npx -y rea-agents mcp`

## Build

```bash
go build -o bin/rea-session ./cmd/rea-session
```

Windows:

```bash
go build -o bin/rea-session.exe ./cmd/rea-session
```

## Usage

```bash
# Discover without pulling schemas into context
rea-session tools --query native --limit 20

# Load one schema when you are about to call it
rea-session schema procedure_pseudo_code

# Open a target once; the daemon keeps the provider warm
rea-session open /absolute/path/to/program --provider ghidra

# Call anything by name
rea-session call binary_session
rea-session call procedure_pseudo_code --json '{"name":"main"}'

# Keep a large payload out of the transcript
rea-session call get_evidence_bundle --json '{}' --out /tmp/evidence.json

# Inspect the daemon
rea-session status
rea-session doctor
rea-session daemon stop
```

The daemon starts on first use and exits after 20 minutes idle (configurable
with `--idle`). Every command auto-starts it.

`daemon run` is the foreground process; `daemon start` spawns it detached and
returns as soon as the control socket is published, before the MCP handshake
finishes. During a cold `npx` start, `status` reports `"phase": "starting"`
and a real tool command waits for `"phase": "ready"`.

### Selecting the REA runtime

```bash
# default: "rea mcp" when present, otherwise "npx -y rea-agents mcp"
rea-session --rea-cmd rea --rea-arg mcp tools
rea-session --rea-cmd node --rea-arg /path/to/rea/dist/mcp/main.js tools
```

The daemon keeps the command it was started with. Stop it before switching:

```bash
rea-session daemon stop && rea-session --rea-cmd ... status
```

### Environment

| Variable | Effect |
| --- | --- |
| `REA_SESSION_REA_CMD` | Command line used to start the MCP server |
| `REA_SESSION_HOME` | State/log directory (default: user cache dir) |
| `REA_MCP_TOOL_CATALOG_PROFILE` | Passed through to REA; see below |

### IDA provider

REA reaches IDA through an upstream `ida-pro-mcp` database supervisor. Generate
its registration on a new machine:

```bash
python scripts/setup_ida.py
```

The script resolves the IDA installation (`--ida`, `IDADIR`, the persisted
`ida-config.json`, PATH, then a shallow scan), resolves `idalib-mcp`, rejects
builds that do not advertise `--stdio` and `--max-workers`, and writes the
registration to `<REA_SESSION_HOME>/ida-mcp.json`
(`%LOCALAPPDATA%\rea-session\ida-mcp.json` by default).

A daemon started without `REA_IDA_MCP_CONFIG` picks that file up
automatically, so no shell exports are required:

```bash
rea-session open /path/to/target --provider ida
```

An explicit `REA_IDA_MCP_CONFIG` always wins. The script never forces the
analysis provider; `--provider ida`, `REA_ANALYSIS_PROVIDER`, or
`open_binary.provider_id` still decides whether IDA is used.

The upstream must serve REA's supervisor profile (`idb_open`, `idb_list`,
`idb_close`). The enhancement fork that exposes only `idalib_*` is rejected:

```bash
python -m pip install --force-reinstall \
  "git+https://github.com/mrexodia/ida-pro-mcp@c133c3853faa111a9b00ee615c013b720d0c4acd"
```

## Design notes

### 1. The daemon owns the session, not the socket

Each CLI invocation opens a loopback connection, sends one request, and exits.
The MCP process and its provider session stay alive in the daemon. All tool
calls are serialized with one mutex, matching REA's admitted-operation model.

### 2. The tool catalog is never printed by default

`tools` prints `name` plus one truncated line of description. `--json` is the
only way to emit full tool objects, and the skill instructs agents not to use
it. `schema <tool>` is the progressive-disclosure step before an unfamiliar
call.

### 3. Output discipline

- `call` prints `structuredContent` when present, otherwise the text content.
- `--out FILE` writes the payload and prints a small receipt instead.
- `--raw` prints only the text representation.
- Exit code `1` means the tool returned `isError`; `3` means the daemon or
  protocol failed.

### 4. Hot/cold catalogs still work

REA's `REA_MCP_TOOL_CATALOG_PROFILE=hot-cold` reduces the server's advertised
set. `rea-session tools` then lists only the loaded definitions; call
`discover_tools` through `rea-session call` to load more, then re-run `tools`.
The default `full` profile is recommended for this CLI because the schemas
never reach the model anyway.

## Limitations

- No MCP progress notifications; long provider startups appear as a blocking
  command. Use `--timeout` and check `daemon.log`.
- Per-tool client approval hints (`readOnlyHint`, `destructiveHint`) are lost.
  Dangerous operations still require their explicit REA arguments.
- `untrusted_content` result metadata is not surfaced; treat all tool output
  as target-derived data.
- The daemon is per-user and local-only (loopback + bearer token).

## Status

Prototype. The transport, discovery and session lifecycle are implemented and
covered by an end-to-end mock (`testdata/mock-mcp.mjs`).
