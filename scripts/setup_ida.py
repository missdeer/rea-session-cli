#!/usr/bin/env python3
"""Generate and validate an REA IDA MCP registration on a fresh machine.

Usage:
    python scripts/setup_ida.py                 # auto-detect IDA and idalib-mcp
    python scripts/setup_ida.py --ida "D:\\IDAPro9.3\\ida93\\portable_win"
    python scripts/setup_ida.py --print-only    # print JSON, do not write
    python scripts/setup_ida.py --shell powershell

Detection order for the IDA installation:
    1. --ida (directory containing idalib.dll, or ida.exe)
    2. IDADIR
    3. persisted ida-config.json (Paths.ida-install-dir)
    4. ida64.exe / ida.exe on PATH
    5. shallow scan of Program Files, LOCALAPPDATA, the user directory and
       drive roots, entering only directories whose names contain "ida"

Detection order for idalib-mcp:
    1. --idalib-mcp
    2. idalib-mcp / idalib-mcp.exe on PATH
    3. the active interpreter's scripts directory

The resolved executable is rejected unless it advertises --stdio and
--max-workers, which is how REA's database-supervisor profile is identified.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import sysconfig
import tempfile
from pathlib import Path

PINNED_UPSTREAM = (
    "git+https://github.com/mrexodia/ida-pro-mcp@"
    "c133c3853faa111a9b00ee615c013b720d0c4acd"
)
SKIP_DIRS = {
    "windows",
    "winnt",
    "temp",
    "tmp",
    "installers",
    "installer",
    "$recycle.bin",
    "system volume information",
    "node_modules",
    ".git",
}


def idalib_name() -> str:
    if sys.platform == "win32":
        return "idalib.dll"
    if sys.platform == "darwin":
        return "libidalib.dylib"
    return "libidalib.so"


def is_ida_dir(path: Path) -> bool:
    return path.is_dir() and (path / idalib_name()).is_file()


def normalize_ida_path(value: str) -> Path:
    path = Path(value).expanduser()
    if path.is_file():
        path = path.parent
    return path


def ida_user_config() -> Path:
    if os.name == "nt":
        base = Path(os.environ.get("APPDATA", "")) / "Hex-Rays" / "IDA Pro"
    else:
        base = Path(os.environ.get("IDAUSR", "~/.idapro")).expanduser()
    return base / "ida-config.json"


def from_persisted_config() -> Path | None:
    config = ida_user_config()
    try:
        data = json.loads(config.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    value = data.get("Paths", {}).get("ida-install-dir") or ""
    if not value:
        return None
    candidate = normalize_ida_path(value)
    return candidate if is_ida_dir(candidate) else None


def from_path() -> Path | None:
    for name in ("ida64.exe", "ida.exe", "ida64", "ida"):
        found = shutil.which(name)
        if found:
            candidate = Path(found).parent
            if is_ida_dir(candidate):
                return candidate
    return None


def scan_roots(max_depth: int = 5) -> Path | None:
    roots: list[Path] = []
    if os.name == "nt":
        for env in ("ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"):
            if os.environ.get(env):
                roots.append(Path(os.environ[env]))
        roots.append(Path.home())
        for drive in "CDEFGHIJKLMNOPQRSTUVWXYZ":
            root = Path(f"{drive}:\\")
            if root.exists():
                roots.append(root)
    else:
        roots.extend([Path("/opt"), Path("/usr/local"), Path.home()])

    seen: set[Path] = set()
    for root in roots:
        if not root.exists():
            continue
        for dirpath, dirnames, _ in os.walk(root):
            current = Path(dirpath)
            try:
                depth = len(current.relative_to(root).parts)
            except ValueError:
                dirnames[:] = []
                continue
            if is_ida_dir(current) and current not in seen:
                seen.add(current)
                return current
            if depth >= max_depth:
                dirnames[:] = []
                continue
            dirnames[:] = [
                name
                for name in dirnames
                if name.lower() not in SKIP_DIRS
                and (depth == 0 or "ida" in name.lower())
            ]
    return None


def resolve_ida(explicit: str | None) -> Path:
    attempts: list[tuple[str, Path | None]] = []
    if explicit:
        candidate = normalize_ida_path(explicit)
        attempts.append(("--ida", candidate if is_ida_dir(candidate) else None))
    if os.environ.get("IDADIR"):
        candidate = normalize_ida_path(os.environ["IDADIR"])
        attempts.append(("IDADIR", candidate if is_ida_dir(candidate) else None))
    attempts.append(("ida-config.json", from_persisted_config()))
    attempts.append(("PATH", from_path()))
    attempts.append(("filesystem scan", scan_roots()))

    for source, candidate in attempts:
        if candidate is not None:
            return candidate

    raise SystemExit(
        "IDA Pro was not found. Pass --ida with the directory that contains "
        f"{idalib_name()} (or ida.exe), or set IDADIR."
    )


def resolve_idalib_mcp(explicit: str | None) -> Path:
    candidates: list[Path] = []
    if explicit:
        candidates.append(Path(explicit).expanduser())
    for name in ("idalib-mcp.exe", "idalib-mcp"):
        found = shutil.which(name)
        if found:
            candidates.append(Path(found))
    scripts = Path(sysconfig.get_path("scripts"))
    for name in ("idalib-mcp.exe", "idalib-mcp"):
        candidates.append(scripts / name)

    for candidate in candidates:
        if candidate.is_file():
            return candidate
    raise SystemExit(
        "idalib-mcp was not found. Install the REA-compatible upstream first:\n"
        f"  {sys.executable} -m pip install {PINNED_UPSTREAM}\n"
        "or pass --idalib-mcp with its path."
    )


def validate_upstream(exe: Path) -> None:
    try:
        proc = subprocess.run(
            [str(exe), "--help"],
            capture_output=True,
            text=True,
            timeout=60,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise SystemExit(f"cannot run {exe}: {exc}") from exc

    output = (proc.stdout or "") + (proc.stderr or "")
    missing = [flag for flag in ("--stdio", "--max-workers") if flag not in output]
    if missing:
        raise SystemExit(
            f"{exe} is not the database-supervisor build "
            f"(missing {', '.join(missing)}).\n"
            "REA headless mode requires the upstream supervisor. Install it:\n"
            f"  {sys.executable} -m pip install --force-reinstall {PINNED_UPSTREAM}\n"
            "Then re-run this script. The enhancement fork's idalib_* catalog is "
            "not accepted by REA."
        )


def default_registration_path() -> Path:
    if os.name == "nt":
        base = Path(os.environ.get("LOCALAPPDATA", tempfile.gettempdir()))
    else:
        base = Path(os.environ.get("XDG_CACHE_HOME", "~/.cache")).expanduser()
    return base / "rea-session" / "ida-mcp.json"


def build_registration(ida_dir: Path, exe: Path, max_workers: int, timeout_ms: int) -> dict:
    return {
        "command": str(exe),
        "args": ["--stdio", "--max-workers", str(max_workers)],
        "mode": "headless",
        "env": {"IDADIR": str(ida_dir)},
        "timeoutMs": timeout_ms,
    }


def write_registration(path: Path, registration: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    raw = json.dumps(registration, indent=2, ensure_ascii=False) + "\n"
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(raw, encoding="utf-8")
    os.replace(tmp, path)


def print_env(path: Path, shell: str) -> None:
    if shell == "none":
        return
    if shell == "powershell":
        print(f"$env:REA_IDA_MCP_CONFIG = '{path}'")
        print("$env:REA_ANALYSIS_PROVIDER = 'ida'")
    elif shell == "cmd":
        print(f'set REA_IDA_MCP_CONFIG={path}')
        print("set REA_ANALYSIS_PROVIDER=ida")
    else:
        print(f"export REA_IDA_MCP_CONFIG='{path}'")
        print("export REA_ANALYSIS_PROVIDER='ida'")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ida", help="IDA installation directory or ida64.exe/ida.exe")
    parser.add_argument("--idalib-mcp", help="path to the idalib-mcp executable")
    parser.add_argument("--out", help="registration output path")
    parser.add_argument("--max-workers", type=int, default=1)
    parser.add_argument("--timeout-ms", type=int, default=600000)
    parser.add_argument("--print-only", action="store_true", help="print without writing")
    parser.add_argument(
        "--shell",
        choices=("bash", "powershell", "cmd", "none"),
        default="bash",
        help="shell syntax for the environment lines",
    )
    args = parser.parse_args()

    ida_dir = resolve_ida(args.ida)
    exe = resolve_idalib_mcp(args.idalib_mcp)
    validate_upstream(exe)

    registration = build_registration(ida_dir, exe, args.max_workers, args.timeout_ms)
    raw = json.dumps(registration, indent=2, ensure_ascii=False)

    if args.print_only:
        print(raw)
        return 0

    out = (Path(args.out).expanduser() if args.out else default_registration_path()).resolve()
    write_registration(out, registration)

    print(f"IDA install   : {ida_dir}")
    print(f"idalib-mcp    : {exe}")
    print(f"registration  : {out}")
    print()
    print("Use it in the current shell (restart the daemon to reload its env):")
    print_env(out, args.shell)
    print()
    print("Then:")
    print("  rea-session daemon stop")
    print("  rea-session open <target> --provider ida")
    print("  rea-session call binary_session")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
