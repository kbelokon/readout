#!/usr/bin/env python3
"""Bounded, attributable Gremlins runner for Readout's Go packages."""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import json
import math
import os
import platform
import re
import secrets
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import tomllib
from pathlib import Path
from typing import Any, Iterator
import uuid


ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / ".gremlins.yaml"
BOUNDED_GO = ROOT / "tools" / "mutation" / "bounded_go_cache.py"
REPORT_DIR = ROOT / "reports" / "mutation" / "go"
LATEST_REPORT = REPORT_DIR / "latest.json"
LATEST_RESUME_REPORT = REPORT_DIR / "latest-resume.json"
FULL_ATTEMPT = REPORT_DIR / "full-attempt.json"
SANITY_PACKAGE = "tools/mutation/testdata/sanity"
GREMLINS_MODULE = "github.com/go-gremlins/gremlins"
GREMLINS_VERSION = "v0.6.0"
GO_VERSION = "1.27.0"
PYTHON_VERSION = "3.13.15"
ZIG_VERSION = "0.16.0"
REPORT_SCHEMA_VERSION = 2
PACKAGES = (
    "cmd/readout",
    "internal/auth",
    "internal/config",
    "internal/demo",
    "internal/fakekube",
    "internal/hooks",
    "internal/kube",
    "internal/web",
    "internal/web/icons",
    "internal/yamlview",
)
EXCLUDED_PACKAGES = {
    "internal/assets": "embed-only generated asset package with no direct tests",
    "internal/version": "linker-injected version data with no executable behavior",
    "internal/web/templates": "template support covered through web; generated files are excluded",
    "tests/e2e/harness": "test harness, not shipped application behavior",
}
KNOWN_STATUSES = {
    "KILLED",
    "LIVED",
    "NOT COVERED",
    "NOT VIABLE",
    "TIMED OUT",
    "RUNNABLE",
    "SKIPPED",
}
STATUS_SEMANTICS = {
    "KILLED": (
        "the Readout shim passed a non-executing build/vet preflight, then go test "
        "exited 1 from a test/runtime failure"
    ),
    "NOT VIABLE": (
        "the Readout build/vet preflight rejected the mutant with a confirmed "
        "in-source diagnostic"
    ),
    "direct_test_kills_available": True,
}

GIB = 1024**3
DEFAULT_WORKERS = 2
MAX_WORKERS = 4
DEFAULT_MAX_MINUTES = 120
MAX_MAX_MINUTES = 240
DEFAULT_CACHE_GIB = 4
MAX_CACHE_GIB = 4
DEFAULT_RUNTIME_GIB = 6
MAX_RUNTIME_GIB = 6
DEFAULT_MIN_FREE_GIB = 30
MIN_MIN_FREE_GIB = 20
DEFAULT_MAX_RSS_GIB = 8
MAX_MAX_RSS_GIB = 8
DEFAULT_CACHE_CHECK_EVERY = 5
DEFAULT_MONITOR_SECONDS = 10
GUARD_POLL_SECONDS = 1
HEARTBEAT_SECONDS = 300
MAX_SOURCE_BYTES = 64 * 1024**2
MAX_SOURCE_FILE_BYTES = 32 * 1024**2
HASH_CHUNK_BYTES = 1024**2
PRESERVED_ENV_KEYS = (
    "PATH",
    "LANG",
    "LC_ALL",
    "LC_CTYPE",
    "USER",
    "LOGNAME",
    "SHELL",
    "TERM",
    "GOPROXY",
    "GOSUMDB",
    "GOPRIVATE",
    "GONOPROXY",
    "GONOSUMDB",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
    "http_proxy",
    "https_proxy",
    "no_proxy",
    "SSL_CERT_FILE",
    "SSL_CERT_DIR",
    "DEVELOPER_DIR",
    "SDKROOT",
    "PKG_CONFIG_PATH",
)


class EvaluationError(RuntimeError):
    pass


class QualityFailure(RuntimeError):
    pass


class ResourceLimitError(RuntimeError):
    pass


def process_group_exists(process_group_id: int) -> bool:
    try:
        os.killpg(process_group_id, 0)
    except ProcessLookupError:
        return False
    except PermissionError as exc:
        raise EvaluationError(
            f"cannot verify process-group exit for PGID {process_group_id}"
        ) from exc
    return True


def kill_spawned_process_group(
    proc: subprocess.Popen[str],
    argv: list[str],
) -> None:
    with contextlib.suppress(OSError, ProcessLookupError):
        os.killpg(proc.pid, signal.SIGKILL)
    try:
        proc.communicate(timeout=5)
    except subprocess.TimeoutExpired as exc:
        raise EvaluationError(
            f"could not reap command after killing its process group: {' '.join(argv)}"
        ) from exc
    deadline = time.monotonic() + 5
    while process_group_exists(proc.pid) and time.monotonic() < deadline:
        time.sleep(0.05)
    if process_group_exists(proc.pid):
        raise EvaluationError(
            f"could not stop command process group {proc.pid}: {' '.join(argv)}"
        )


def run_checked(
    argv: list[str],
    *,
    timeout: int = 120,
    env: dict[str, str] | None = None,
    cwd: Path = ROOT,
) -> subprocess.CompletedProcess[str]:
    proc = subprocess.Popen(
        argv,
        cwd=cwd,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        stdout, stderr = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        try:
            kill_spawned_process_group(proc, argv)
        except EvaluationError as cleanup_error:
            raise cleanup_error from exc
        raise
    except BaseException as exc:
        try:
            kill_spawned_process_group(proc, argv)
        except EvaluationError as cleanup_error:
            raise cleanup_error from exc
        raise
    if process_group_exists(proc.pid):
        kill_spawned_process_group(proc, argv)
        raise EvaluationError(f"command left descendant processes: {' '.join(argv)}")
    completed = subprocess.CompletedProcess(argv, proc.returncode, stdout, stderr)
    if proc.returncode != 0:
        raise EvaluationError(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout:\n{stdout[-4000:]}\nstderr:\n{stderr[-4000:]}"
        )
    return completed


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while chunk := handle.read(HASH_CHUNK_BYTES):
            digest.update(chunk)
    return digest.hexdigest()


def sha256_json(value: Any) -> str:
    payload = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return sha256_bytes(payload)


def test_environment_identity() -> dict[str, Any]:
    return {
        "preserved_sha256": {
            key: sha256_bytes(os.environ[key].encode())
            for key in PRESERVED_ENV_KEYS
            if key in os.environ
        },
        "isolated": [
            "GOCACHE",
            "GOMODCACHE",
            "GOPATH",
            "GOTMPDIR",
            "HOME",
            "TMPDIR",
            "XDG_CACHE_HOME",
            "XDG_CONFIG_HOME",
            "ZIG_GLOBAL_CACHE_DIR",
            "ZIG_LOCAL_CACHE_DIR",
        ],
        "fixed": {
            "CC": "zig cc",
            "CGO_ENABLED": "1",
            "GOMAXPROCS": "1",
            "GOFLAGS": "",
            "GOTOOLCHAIN": "local",
            "GOWORK": "off",
            "TZ": "UTC",
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_CONFIG_NOSYSTEM": "1",
        },
    }


def base_mutation_environment() -> dict[str, str]:
    return {
        key: os.environ[key]
        for key in PRESERVED_ENV_KEYS
        if key in os.environ
    }


def go_child_environment(env: dict[str, str]) -> dict[str, str]:
    child = {key: value for key, value in env.items() if not key.startswith("READOUT_")}
    original_path = env.get("READOUT_ORIGINAL_PATH")
    if original_path is not None:
        child["PATH"] = original_path
    return child


def disable_go_telemetry(
    real_go: str,
    env: dict[str, str],
    expected_root: Path,
) -> None:
    child_env = go_child_environment(env)
    run_checked([real_go, "telemetry", "off"], env=child_env)
    try:
        state = json.loads(
            run_checked(
                [real_go, "env", "-json", "GOTELEMETRY", "GOTELEMETRYDIR"],
                env=child_env,
            ).stdout
        )
        telemetry_dir = Path(state["GOTELEMETRYDIR"]).resolve(strict=False)
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        raise EvaluationError("cannot verify isolated Go telemetry state") from exc
    expected_root = expected_root.resolve()
    if state.get("GOTELEMETRY") != "off" or not telemetry_dir.is_relative_to(
        expected_root
    ):
        raise EvaluationError(
            "Go telemetry must be disabled inside the runner-owned isolated root"
        )


def validate_ambient_mutation_environment() -> None:
    if platform.python_version() != PYTHON_VERSION:
        raise EvaluationError(
            f"Python {PYTHON_VERSION} is required, got {platform.python_version()}; "
            "run through mise"
        )
    if os.environ.get("GOFLAGS", "").strip():
        raise EvaluationError(
            "GOFLAGS must be empty for mutation testing because overlay/modfile/exec "
            "flags can escape the staged, attested source tree"
        )
    if os.environ.get("CGO_ENABLED", "1") != "1":
        raise EvaluationError("CGO_ENABLED must be 1 for the pinned mutation toolchain")
    if os.environ.get("CC", "zig cc") != "zig cc":
        raise EvaluationError("CC must be the pinned mise compiler command 'zig cc'")
    forbidden_cgo = (
        "CXX",
        "CGO_CFLAGS",
        "CGO_CPPFLAGS",
        "CGO_CXXFLAGS",
        "CGO_LDFLAGS",
    )
    present = [name for name in forbidden_cgo if os.environ.get(name, "").strip()]
    if present:
        raise EvaluationError(
            "external CGO compiler flags are not allowed during mutation testing: "
            + ", ".join(present)
        )


@contextlib.contextmanager
def isolated_go_metadata_environment(
    real_go: str,
    *,
    reuse_module_cache: bool,
) -> Iterator[dict[str, str]]:
    with tempfile.TemporaryDirectory(prefix="readout-go-metadata-") as directory:
        root = Path(directory)
        paths = {
            name: root / name
            for name in (
                "go-build",
                "go-mod",
                "go-path",
                "go-tmp",
                "home",
                "tmp",
                "xdg-cache",
                "xdg-config",
            )
        }
        for path in paths.values():
            path.mkdir()
        module_cache = paths["go-mod"]
        if reuse_module_cache:
            raw_env = os.environ.copy()
            raw_env["GOFLAGS"] = ""
            raw_env["GOWORK"] = "off"
            module_cache_raw = run_checked(
                [real_go, "env", "GOMODCACHE"],
                env=raw_env,
            ).stdout.strip()
            if not module_cache_raw:
                raise EvaluationError("go env returned an empty GOMODCACHE")
            module_cache = Path(module_cache_raw).resolve()
        env = base_mutation_environment()
        env.update(
            {
                "CC": "zig cc",
                "CGO_ENABLED": "1",
                "GOCACHE": str(paths["go-build"]),
                "GOMODCACHE": str(module_cache),
                "GOPATH": str(paths["go-path"]),
                "GOTMPDIR": str(paths["go-tmp"]),
                "GOMAXPROCS": "1",
                "GOFLAGS": "",
                "GOTOOLCHAIN": "local",
                "GOWORK": "off",
                "HOME": str(paths["home"]),
                "TMPDIR": str(paths["tmp"]),
                "XDG_CACHE_HOME": str(paths["xdg-cache"]),
                "XDG_CONFIG_HOME": str(paths["xdg-config"]),
                "TZ": "UTC",
                "GIT_CONFIG_GLOBAL": "/dev/null",
                "GIT_CONFIG_NOSYSTEM": "1",
            }
        )
        disable_go_telemetry(real_go, env, root)
        yield env


def iso_time(timestamp: float | None = None) -> str:
    if timestamp is None:
        timestamp = time.time()
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(timestamp))


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(value, indent=2, sort_keys=True) + "\n"
    with tempfile.NamedTemporaryFile("w", dir=path.parent, delete=False) as handle:
        handle.write(payload)
        temporary = Path(handle.name)
    temporary.replace(path)


def load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise EvaluationError(f"invalid Gremlins report {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise EvaluationError(f"Gremlins report is not an object: {path}")
    return value


def positive_int(value: str) -> int:
    try:
        parsed = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be an integer") from exc
    if parsed < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return parsed


def bounded(name: str, value: int, minimum: int, maximum: int) -> int:
    if not minimum <= value <= maximum:
        raise EvaluationError(f"{name} must be between {minimum} and {maximum}, got {value}")
    return value


def ensure_owned_directory(root: Path, *names: str) -> Path:
    root = root.resolve()
    if not root.is_dir():
        raise EvaluationError(f"owned directory root is unavailable: {root}")
    current = root
    for name in names:
        part = Path(name)
        if part.is_absolute() or len(part.parts) != 1 or name in {"", ".", ".."}:
            raise EvaluationError(f"unsafe owned directory component: {name!r}")
        current = current / name
        if current.is_symlink():
            raise EvaluationError(f"owned directory must not be a symlink: {current}")
        try:
            current.mkdir(mode=0o700)
        except FileExistsError:
            pass
        if current.is_symlink() or not current.is_dir():
            raise EvaluationError(f"owned path is not a plain directory: {current}")
        resolved = current.resolve()
        try:
            resolved.relative_to(root)
        except ValueError as exc:
            raise EvaluationError(f"owned directory escaped its root: {current}") from exc
        current = resolved
    return current


def remove_owned_directory(path: Path, parent: Path) -> None:
    """Remove one verified owned child, including Go's read-only module cache."""
    parent = parent.resolve()
    if path.is_symlink():
        raise EvaluationError(f"refusing to remove symlinked owned directory: {path}")
    target = path.resolve()
    if target.parent != parent:
        raise EvaluationError(f"refusing to remove directory outside {parent}: {target}")
    if not target.exists():
        return
    if not target.is_dir():
        raise EvaluationError(f"owned cleanup target is not a directory: {target}")
    for current, directories, _files in os.walk(target, topdown=True, followlinks=False):
        current_path = Path(current)
        if current_path.is_symlink():
            raise EvaluationError(f"owned cleanup traversal reached a symlink: {current_path}")
        os.chmod(current_path, stat.S_IRWXU)
        for name in directories:
            child = current_path / name
            if child.is_symlink():
                continue
            if not child.is_dir():
                raise EvaluationError(f"owned cleanup child is not a directory: {child}")
            os.chmod(child, stat.S_IRWXU)
    shutil.rmtree(target)


def cache_root() -> Path:
    git = resolve_system_executable("git")
    repository_root = run_checked(
        [git, "-C", str(ROOT), "rev-parse", "--show-toplevel"]
    ).stdout.strip()
    if Path(repository_root).resolve() != ROOT.resolve():
        raise EvaluationError(
            f"Git repository root mismatch: {repository_root!r} != {str(ROOT)!r}"
        )
    common = run_checked(
        [
            git,
            "-C",
            str(ROOT),
            "rev-parse",
            "--path-format=absolute",
            "--git-common-dir",
        ]
    ).stdout.strip()
    common_path = Path(common)
    if not common_path.is_absolute() or common_path.is_symlink():
        raise EvaluationError(f"Git common directory is unsafe: {common!r}")
    common_root = common_path.resolve()
    head = common_root / "HEAD"
    objects = common_root / "objects"
    if (
        head.is_symlink()
        or not head.is_file()
        or objects.is_symlink()
        or not objects.is_dir()
    ):
        raise EvaluationError(f"Git common directory lacks trusted HEAD/objects: {common_root}")
    root = common_root / "readout-go-mutation"
    if root.is_symlink():
        raise EvaluationError(f"mutation cache root must not be a symlink: {root}")
    try:
        root.mkdir(mode=0o700)
    except FileExistsError:
        pass
    if root.is_symlink() or not root.is_dir() or root.resolve().parent != common_root:
        raise EvaluationError(f"mutation cache root is not a plain Git-owned directory: {root}")
    return root.resolve()


def ensure_report_directory() -> Path:
    return ensure_owned_directory(ROOT, "reports", "mutation", "go")


@contextlib.contextmanager
def exclusive_run_lock(root: Path) -> Iterator[None]:
    path = root / "run.lock"
    flags = os.O_RDWR | os.O_CREAT
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags, 0o600)
    except OSError as exc:
        raise EvaluationError(f"cannot open safe mutation lock {path}: {exc}") from exc
    metadata = os.fstat(descriptor)
    if not stat.S_ISREG(metadata.st_mode):
        os.close(descriptor)
        raise EvaluationError(f"mutation lock is not a regular file: {path}")
    with os.fdopen(descriptor, "r+", encoding="utf-8") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            handle.seek(0)
            owner = handle.read().strip() or "unknown owner"
            raise EvaluationError(f"another Go mutation run owns {path}: {owner}") from exc
        handle.seek(0)
        handle.truncate()
        handle.write(json.dumps({"pid": os.getpid(), "started_at": iso_time()}) + "\n")
        handle.flush()
        try:
            yield
        finally:
            handle.seek(0)
            handle.truncate()
            handle.flush()
            fcntl.flock(handle, fcntl.LOCK_UN)


def resolve_executable(name: str) -> str:
    if not name or Path(name).name != name:
        raise EvaluationError(f"invalid executable name: {name!r}")
    for raw_directory in os.environ.get("PATH", "").split(os.pathsep):
        if not raw_directory or not Path(raw_directory).is_absolute():
            raise EvaluationError(f"unsafe relative or empty PATH entry: {raw_directory!r}")
        directory = Path(raw_directory)
        candidate = directory / name
        if not candidate.is_file() or not os.access(candidate, os.X_OK):
            continue
        absolute_candidate = candidate.absolute()
        resolved = candidate.resolve()
        if absolute_candidate.is_relative_to(ROOT) or resolved.is_relative_to(ROOT):
            raise EvaluationError(
                f"refusing repository-shadowed executable for {name}: {candidate}"
            )
        return str(resolved)
    raise EvaluationError(f"required executable not found on trusted PATH: {name}")


def resolve_system_executable(name: str) -> str:
    if not name or Path(name).name != name:
        raise EvaluationError(f"invalid system executable name: {name!r}")
    for directory in (Path("/usr/bin"), Path("/bin"), Path("/usr/sbin"), Path("/sbin")):
        candidate = directory / name
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate.resolve())
    raise EvaluationError(f"required system executable is unavailable: {name}")


def verify_tool() -> dict[str, Any]:
    gremlins = resolve_executable("gremlins")
    real_go = resolve_executable("go")
    mise_installs = Path.home() / ".local" / "share" / "mise" / "installs"
    expected_paths = {
        "go": (mise_installs / "go" / GO_VERSION / "bin" / "go").resolve(),
        "gremlins": (
            mise_installs
            / "github-go-gremlins-gremlins"
            / GREMLINS_VERSION.removeprefix("v")
            / "gremlins"
        ).resolve(),
    }
    if Path(real_go) != expected_paths["go"] or Path(gremlins) != expected_paths["gremlins"]:
        raise EvaluationError(
            "Go and Gremlins must resolve to the exact mise-managed pins; run `mise install`"
        )
    build_info = run_checked([real_go, "version", "-m", gremlins]).stdout
    go_version_output = run_checked([real_go, "version"]).stdout.strip()
    if not go_version_output.startswith(f"go version go{GO_VERSION} "):
        raise EvaluationError(f"Go {GO_VERSION} is required, got {go_version_output!r}")
    version_output = run_checked([gremlins, "--version"]).stdout.strip()
    module_pin = re.compile(
        rf"^\s*mod\s+{re.escape(GREMLINS_MODULE)}\s+{re.escape(GREMLINS_VERSION)}(?:\s|$)",
        re.MULTILINE,
    )
    if not module_pin.search(build_info):
        raise EvaluationError(
            f"{gremlins} is not verified as {GREMLINS_MODULE}@{GREMLINS_VERSION}; "
            "run `mise install`"
        )
    go_build_info = run_checked([real_go, "version", "-m", real_go]).stdout
    go_host = json.loads(
        run_checked(
            [real_go, "env", "-json", "GOROOT", "GOHOSTOS", "GOHOSTARCH", "CC"]
        ).stdout
    )
    if not isinstance(go_host, dict):
        raise EvaluationError("go env returned malformed toolchain identity")
    tool_dir = (
        Path(str(go_host.get("GOROOT", "")))
        / "pkg"
        / "tool"
        / f"{go_host.get('GOHOSTOS', '')}_{go_host.get('GOHOSTARCH', '')}"
    )
    compile_tool = tool_dir / "compile"
    link_tool = tool_dir / "link"
    if not compile_tool.is_file() or not link_tool.is_file():
        raise EvaluationError(f"Go compile/link tools are unavailable under {tool_dir}")
    cc_command = str(go_host.get("CC", ""))
    try:
        cc_tokens = shlex.split(cc_command)
    except ValueError as exc:
        raise EvaluationError(f"invalid Go CC command: {cc_command!r}") from exc
    if cc_tokens != ["zig", "cc"]:
        raise EvaluationError(
            f"mutation testing requires the pinned mise CC 'zig cc', got {cc_command!r}"
        )
    zig = resolve_executable("zig")
    expected_zig = (mise_installs / "zig" / ZIG_VERSION / "bin" / "zig").resolve()
    if Path(zig) != expected_zig:
        raise EvaluationError("Zig must resolve to the exact mise-managed pin")
    zig_version = run_checked([zig, "version"]).stdout.strip()
    if zig_version != ZIG_VERSION:
        raise EvaluationError(f"Zig {ZIG_VERSION} is required, got {zig_version!r}")
    return {
        "gremlins_path": gremlins,
        "gremlins_version": GREMLINS_VERSION,
        "gremlins_version_output": version_output,
        "gremlins_binary_sha256": sha256_file(Path(gremlins)),
        "gremlins_build_info_sha256": sha256_bytes(build_info.encode()),
        "go_path": real_go,
        "go_binary_sha256": sha256_file(Path(real_go)),
        "go_build_info_sha256": sha256_bytes(go_build_info.encode()),
        "go_compile_sha256": sha256_file(compile_tool),
        "go_link_sha256": sha256_file(link_tool),
        "cc_command_sha256": sha256_bytes(cc_command.encode()),
        "cc_binary_sha256": sha256_file(Path(zig)),
        "cc_version": zig_version,
    }


def go_environment(real_go: str, env: dict[str, str] | None = None) -> dict[str, str]:
    output = run_checked(
        [
            real_go,
            "env",
            "-json",
            "GOVERSION",
            "GOOS",
            "GOARCH",
            "CGO_ENABLED",
            "CC",
            "GOFLAGS",
            "GOTOOLCHAIN",
        ],
        env=env,
    ).stdout
    value = json.loads(output)
    if not isinstance(value, dict) or not all(isinstance(k, str) for k in value):
        raise EvaluationError("go env returned malformed JSON")
    public_keys = ("GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED")
    result = {key: str(value.get(key, "")) for key in public_keys}
    for key in ("CC", "GOFLAGS", "GOTOOLCHAIN"):
        result[f"{key}_SHA256"] = sha256_bytes(str(value.get(key, "")).encode())
    source_env = os.environ if env is None else env
    result["GOMAXPROCS"] = source_env.get("GOMAXPROCS", "")
    return result


def git_state() -> dict[str, Any]:
    git = resolve_system_executable("git")
    head = run_checked([git, "rev-parse", "HEAD"]).stdout.strip()
    status = run_checked(
        [git, "status", "--porcelain=v1", "--untracked-files=all"]
    ).stdout.splitlines()
    return {"head": head, "dirty": bool(status), "changes": status}


def source_paths() -> list[Path]:
    git = resolve_system_executable("git")
    proc = run_checked(
        [git, "ls-files", "-z", "--cached", "--others", "--exclude-standard"]
    )
    raw_paths = [item for item in proc.stdout.split("\0") if item]
    result: list[Path] = []
    for relative in raw_paths:
        path = ROOT / relative
        if path.exists() or path.is_symlink():
            result.append(path)
    return sorted(result)


def source_manifest() -> dict[str, Any]:
    return manifest_from_paths(ROOT, source_paths())


def manifest_from_paths(root: Path, paths: list[Path]) -> dict[str, Any]:
    root = root.resolve()
    files: dict[str, Any] = {}
    total_size = 0
    for path in sorted(paths):
        relative = str(path.relative_to(root))
        metadata = path.lstat()
        if path.is_symlink():
            raise EvaluationError(
                f"symlink mutation inputs are unsupported by Gremlins v0.6.0: {relative}"
            )
        if path.is_file():
            if metadata.st_size > MAX_SOURCE_FILE_BYTES:
                raise EvaluationError(
                    f"mutation input exceeds the per-file limit "
                    f"({metadata.st_size} > {MAX_SOURCE_FILE_BYTES} bytes): {relative}"
                )
            payload = None
            kind = "file"
        else:
            raise EvaluationError(f"unsupported mutation input type: {relative}")
        size = len(payload) if payload is not None else metadata.st_size
        total_size += size
        if total_size > MAX_SOURCE_BYTES:
            raise EvaluationError(
                f"mutation inputs exceed the {MAX_SOURCE_BYTES / 1024**2:.0f} MiB stage limit"
            )
        files[relative] = {
            "kind": kind,
            "mode": metadata.st_mode & 0o777,
            "size": size,
            "sha256": sha256_bytes(payload) if payload is not None else sha256_file(path),
        }
    payload = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    return {"digest": sha256_bytes(payload), "files": files, "total_size": total_size}


def stage_manifest(stage: Path) -> dict[str, Any]:
    paths = [
        path
        for path in stage.rglob("*")
        if path.is_file() or path.is_symlink()
    ]
    return manifest_from_paths(stage, paths)


def create_source_stage(runtime: Path, manifest: dict[str, Any]) -> Path:
    stage = runtime / "source"
    stage.mkdir()
    for relative, metadata in manifest["files"].items():
        source = ROOT / relative
        destination = stage / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        digest = hashlib.sha256()
        copied = 0
        with source.open("rb") as source_handle, destination.open("xb") as output_handle:
            while chunk := source_handle.read(HASH_CHUNK_BYTES):
                copied += len(chunk)
                if copied > metadata["size"] or copied > MAX_SOURCE_FILE_BYTES:
                    raise EvaluationError(f"mutation input grew while staging: {relative}")
                digest.update(chunk)
                output_handle.write(chunk)
        if copied != metadata["size"] or digest.hexdigest() != metadata["sha256"]:
            raise EvaluationError(f"mutation input changed while staging: {relative}")
        shutil.copystat(source, destination, follow_symlinks=False)
    return stage


def assert_source_unchanged(expected: dict[str, Any]) -> None:
    actual = source_manifest()
    if actual["digest"] != expected["digest"]:
        raise EvaluationError(
            "repository inputs changed during mutation testing; refusing to publish a mixed report"
        )


def assert_inputs_unchanged(expected: dict[str, Any], stage: Path) -> None:
    assert_source_unchanged(expected)
    actual_stage = stage_manifest(stage)
    if actual_stage["digest"] != expected["digest"]:
        raise EvaluationError(
            "staged mutation inputs changed or gained files; refusing to reuse or "
            "publish mixed evidence"
        )


def decode_json_stream(raw: str) -> list[dict[str, Any]]:
    decoder = json.JSONDecoder()
    offset = 0
    values: list[dict[str, Any]] = []
    while offset < len(raw):
        while offset < len(raw) and raw[offset].isspace():
            offset += 1
        if offset >= len(raw):
            break
        value, offset = decoder.raw_decode(raw, offset)
        if isinstance(value, dict):
            values.append(value)
    return values


def go_list_objects(
    package: str,
    real_go: str,
    work_root: Path,
    env: dict[str, str],
    *,
    runtime: Path | None = None,
    limits: dict[str, int] | None = None,
    deadline: float | None = None,
) -> list[dict[str, Any]]:
    work_root = work_root.resolve()
    arguments = ["list", "-deps", "-test", "-json", f"./{package}"]
    if runtime is None:
        proc = run_checked([real_go, *arguments], cwd=work_root, env=env)
    else:
        if limits is None or deadline is None:
            raise EvaluationError("managed Go list is missing resource limits")
        proc = run_managed_go(
            real_go,
            arguments,
            cwd=work_root,
            env=env,
            deadline=deadline,
            limits=limits,
            runtime=runtime,
            label=f"Go dependency query for {package}",
        )
    return decode_json_stream(proc.stdout)


def local_dependency_files(
    package: str,
    real_go: str,
    work_root: Path,
    env: dict[str, str],
    *,
    runtime: Path | None = None,
    limits: dict[str, int] | None = None,
    deadline: float | None = None,
) -> list[Path]:
    work_root = work_root.resolve()
    file_fields = (
        "GoFiles",
        "CgoFiles",
        "CFiles",
        "CXXFiles",
        "MFiles",
        "HFiles",
        "FFiles",
        "SFiles",
        "SwigFiles",
        "SwigCXXFiles",
        "SysoFiles",
        "EmbedFiles",
        "TestGoFiles",
        "XTestGoFiles",
        "TestEmbedFiles",
        "XTestEmbedFiles",
    )
    files: set[Path] = set()
    directories: set[Path] = set()
    for item in go_list_objects(
        package,
        real_go,
        work_root,
        env,
        runtime=runtime,
        limits=limits,
        deadline=deadline,
    ):
        if item.get("Standard"):
            continue
        directory_raw = item.get("Dir")
        if not isinstance(directory_raw, str):
            continue
        directory = Path(directory_raw).resolve()
        try:
            relative_directory = directory.relative_to(work_root)
        except ValueError:
            continue
        original_directory = ROOT / relative_directory
        directories.add(original_directory)
        for field in file_fields:
            names = item.get(field, [])
            if not isinstance(names, list):
                continue
            for name in names:
                if isinstance(name, str):
                    relative_name = Path(name)
                    if relative_name.is_absolute() or ".." in relative_name.parts:
                        continue
                    candidate = (original_directory / relative_name).resolve()
                    try:
                        candidate.relative_to(original_directory.resolve())
                        candidate.relative_to(ROOT)
                    except ValueError:
                        continue
                    if candidate.is_file():
                        files.add(candidate)
    for directory in directories:
        testdata = directory / "testdata"
        if testdata.is_dir():
            files.update(path for path in testdata.rglob("*") if path.is_file())
    return sorted(files)


def verify_scope(
    real_go: str,
    work_root: Path,
    env: dict[str, str],
    *,
    runtime: Path | None = None,
    limits: dict[str, int] | None = None,
    deadline: float | None = None,
) -> dict[str, Any]:
    work_root = work_root.resolve()
    arguments = ["list", "-find", "-json", "./..."]
    if runtime is None:
        proc = run_checked([real_go, *arguments], cwd=work_root, env=env)
    else:
        if limits is None or deadline is None:
            raise EvaluationError("managed Go scope query is missing resource limits")
        proc = run_managed_go(
            real_go,
            arguments,
            cwd=work_root,
            env=env,
            deadline=deadline,
            limits=limits,
            runtime=runtime,
            label="Go mutation scope query",
        )
    discovered: set[str] = set()
    for item in decode_json_stream(proc.stdout):
        directory_raw = item.get("Dir")
        if not isinstance(directory_raw, str):
            continue
        directory = Path(directory_raw).resolve()
        try:
            relative = str(directory.relative_to(work_root))
        except ValueError:
            continue
        discovered.add(relative)
    classified = set(PACKAGES) | set(EXCLUDED_PACKAGES)
    missing = sorted(discovered - classified)
    vanished = sorted(classified - discovered)
    if missing or vanished:
        details = []
        if missing:
            details.append("unclassified packages: " + ", ".join(missing))
        if vanished:
            details.append("configured packages no longer present: " + ", ".join(vanished))
        raise EvaluationError("Go mutation scope drift: " + "; ".join(details))
    return {
        "included": list(PACKAGES),
        "excluded": dict(EXCLUDED_PACKAGES),
    }


def package_key(
    package: str,
    tool: dict[str, Any],
    go_env: dict[str, str],
    environment_identity: dict[str, Any],
    workers: int,
    work_root: Path,
    env: dict[str, str],
    *,
    runtime: Path | None = None,
    limits: dict[str, int] | None = None,
    deadline: float | None = None,
) -> str:
    digest = hashlib.sha256()
    digest.update(b"readout-go-mutation-v2\0")
    digest.update(package.encode())
    digest.update(json.dumps(tool, sort_keys=True).encode())
    digest.update(json.dumps(go_env, sort_keys=True).encode())
    digest.update(json.dumps(environment_identity, sort_keys=True).encode())
    digest.update(f"workers={workers}".encode())
    inputs = [
        ROOT / "go.mod",
        ROOT / "go.sum",
        CONFIG,
        Path(__file__).resolve(),
        BOUNDED_GO,
    ]
    inputs.extend(
        local_dependency_files(
            package,
            str(tool["go_path"]),
            work_root,
            env,
            runtime=runtime,
            limits=limits,
            deadline=deadline,
        )
    )
    for path in sorted(set(inputs)):
        relative = path.relative_to(ROOT)
        digest.update(str(relative).encode())
        digest.update(b"\0")
        update_cache_key_with_file(digest, path)
    return digest.hexdigest()


def update_cache_key_with_file(digest: Any, path: Path) -> None:
    metadata = path.stat()
    if not stat.S_ISREG(metadata.st_mode):
        raise EvaluationError(f"mutation cache-key input is not a regular file: {path}")
    digest.update(f"mode={metadata.st_mode & 0o777:o}\0".encode())
    digest.update(f"size={metadata.st_size}\0".encode())
    digest.update(sha256_file(path).encode())
    digest.update(b"\0")


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def process_group_rows(process_group_id: int) -> list[tuple[int, str]]:
    ps = resolve_system_executable("ps")
    proc = subprocess.run(
        [ps, "-axo", "pgid=,pid=,command="],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        raise EvaluationError(f"cannot inspect mutation processes: {proc.stderr.strip()}")
    rows: list[tuple[int, str]] = []
    for line in proc.stdout.splitlines():
        match = re.match(r"^\s*(\d+)\s+(\d+)\s+(.*)$", line)
        if not match or int(match.group(1)) != process_group_id:
            continue
        rows.append((int(match.group(2)), match.group(3)))
    return rows


def write_runtime_owner(
    runtime: Path,
    child_pgid: int | None = None,
    child_executable: str | None = None,
    child_config: Path | None = None,
    *,
    state: str = "active",
) -> None:
    if state not in {"active", "cleaning"}:
        raise EvaluationError(f"invalid mutation runtime state: {state}")
    value: dict[str, Any] = {
        "pid": os.getpid(),
        "started_at": iso_time(),
        "runtime": str(runtime),
        "state": state,
    }
    if child_pgid is not None:
        value["child_pgid"] = child_pgid
    if child_executable is not None:
        value["child_executable"] = child_executable
    if child_config is not None:
        value["child_config"] = str(child_config)
    atomic_json(runtime_owner_path(runtime), value)


def runtime_owner_path(runtime: Path) -> Path:
    parent = runtime.parent.resolve()
    if runtime.is_symlink() or runtime.resolve(strict=False).parent != parent:
        raise EvaluationError(f"unsafe mutation runtime owner target: {runtime}")
    if not re.fullmatch(r"run-[A-Za-z0-9_-]+", runtime.name):
        raise EvaluationError(f"invalid mutation runtime name: {runtime.name}")
    owner = parent / f".{runtime.name}.owner.json"
    if owner.is_symlink():
        raise EvaluationError(f"mutation runtime owner must not be a symlink: {owner}")
    return owner


def cleanup_runtime(runtime: Path) -> None:
    owner = runtime_owner_path(runtime)
    if runtime.exists() and not owner.is_file():
        raise EvaluationError(f"mutation runtime has no external owner proof: {runtime}")
    if owner.exists():
        try:
            value = json.loads(owner.read_text())
            owner_pid = int(value["pid"])
            child_pgid_raw = value.get("child_pgid")
        except (
            OSError,
            json.JSONDecodeError,
            AttributeError,
            KeyError,
            TypeError,
            ValueError,
        ) as exc:
            raise EvaluationError(f"cannot verify mutation runtime owner: {owner}") from exc
        if (
            owner_pid < 1
            or value.get("runtime") != str(runtime)
            or value.get("state") not in {"active", "cleaning"}
        ):
            raise EvaluationError(f"mutation runtime owner is inconsistent: {owner}")
        if child_pgid_raw is not None:
            try:
                child_pgid = int(child_pgid_raw)
            except (TypeError, ValueError) as exc:
                raise EvaluationError(f"invalid child PGID in {owner}") from exc
            remaining = process_group_rows(child_pgid)
            if remaining:
                raise EvaluationError(
                    f"refusing to remove runtime with live process group {child_pgid}: "
                    f"{remaining!r}"
                )
    if runtime.exists():
        write_runtime_owner(runtime, state="cleaning")
        remove_owned_directory(runtime, runtime.parent)
    if owner.exists():
        if owner.is_symlink() or not owner.is_file():
            raise EvaluationError(f"unsafe mutation runtime owner file: {owner}")
        owner.unlink()


def prune_stale_runtimes(root: Path) -> None:
    if root.is_symlink():
        raise EvaluationError(f"mutation runtime root must not be a symlink: {root}")
    root.mkdir(mode=0o700, exist_ok=True)
    if root.is_symlink() or not root.is_dir():
        raise EvaluationError(f"mutation runtime root is not a plain directory: {root}")
    for owner_path in root.glob(".run-*.owner.json"):
        if owner_path.is_symlink() or not owner_path.is_file():
            raise EvaluationError(f"unsafe mutation runtime owner file: {owner_path}")
        runtime_name = owner_path.name[1 : -len(".owner.json")]
        candidate = root / runtime_name
        if candidate.exists():
            continue
        try:
            owner = json.loads(owner_path.read_text())
            owner_pid = int(owner["pid"])
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as exc:
            raise EvaluationError(f"cannot verify orphaned runtime owner: {owner_path}") from exc
        if (
            owner.get("runtime") != str(candidate)
            or owner.get("state") != "cleaning"
            or pid_alive(owner_pid)
        ):
            raise EvaluationError(f"cannot verify orphaned runtime owner: {owner_path}")
        owner_path.unlink()

    for candidate in root.glob("run-*"):
        if candidate.is_symlink():
            raise EvaluationError(f"mutation runtime must not be a symlink: {candidate}")
        if not candidate.is_dir():
            continue
        owner_path = runtime_owner_path(candidate)
        legacy_owner = candidate / "owner.json"
        selected_owner = owner_path if owner_path.is_file() else legacy_owner
        try:
            owner = json.loads(selected_owner.read_text())
            owner_pid = int(owner["pid"])
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
            raise EvaluationError(f"cannot verify stale mutation runtime owner: {candidate}")
        if owner.get("runtime") != str(candidate):
            raise EvaluationError(f"stale mutation runtime owner mismatch: {candidate}")
        if pid_alive(owner_pid):
            raise EvaluationError(
                f"mutation runtime owner PID {owner_pid} is live despite the exclusive "
                f"runner lock: {candidate}"
            )
        child_pgid_raw = owner.get("child_pgid")
        if child_pgid_raw is not None:
            try:
                child_pgid = int(child_pgid_raw)
            except (TypeError, ValueError) as exc:
                raise EvaluationError(f"invalid stale child PGID in {candidate}") from exc
            rows = process_group_rows(child_pgid)
            if rows:
                child_executable = owner.get("child_executable")
                child_config = owner.get("child_config")
                if not isinstance(child_executable, str) or not isinstance(child_config, str):
                    raise EvaluationError(
                        f"stale mutation runtime lacks child identity; refusing cleanup: {candidate}"
                    )
                leaders = [
                    command
                    for pid, command in rows
                    if pid == child_pgid
                    and child_executable in command
                    and child_config in command
                    and str(candidate) in child_config
                ]
                if len(leaders) != 1:
                    raise EvaluationError(
                        f"stale mutation runtime has an unverified live process group {child_pgid}; "
                        f"refusing cleanup: {candidate}"
                    )
                with contextlib.suppress(OSError, ProcessLookupError):
                    os.killpg(child_pgid, signal.SIGTERM)
                deadline = time.monotonic() + 5
                while process_group_rows(child_pgid) and time.monotonic() < deadline:
                    time.sleep(0.1)
                if process_group_rows(child_pgid):
                    with contextlib.suppress(OSError, ProcessLookupError):
                        os.killpg(child_pgid, signal.SIGKILL)
                    deadline = time.monotonic() + 5
                    while process_group_rows(child_pgid) and time.monotonic() < deadline:
                        time.sleep(0.1)
                if process_group_rows(child_pgid):
                    raise EvaluationError(
                        f"could not stop stale mutation process group {child_pgid}; refusing cleanup"
                    )
        if selected_owner == legacy_owner:
            # Legacy owner files lived inside the tree. Preserve their evidence
            # outside it before a read-only module-cache cleanup can begin.
            owner["state"] = "cleaning"
            owner["pid"] = os.getpid()
            atomic_json(owner_path, owner)
        cleanup_runtime(candidate)


def mutation_environment(
    root: Path,
    tool: dict[str, Any],
    limits: dict[str, int],
) -> tuple[Path, dict[str, str]]:
    if not BOUNDED_GO.is_file() or not os.access(BOUNDED_GO, os.X_OK):
        raise EvaluationError(f"missing executable bounded Go wrapper: {BOUNDED_GO}")
    runtime_root = ensure_owned_directory(root, "runtime")
    prune_stale_runtimes(runtime_root)
    runtime = Path(tempfile.mkdtemp(prefix="run-", dir=runtime_root))
    try:
        write_runtime_owner(runtime)
    except BaseException:
        remove_owned_directory(runtime, runtime_root)
        raise
    try:
        capability = secrets.token_hex(32)
        capability_path = runtime / ".runner-capability"
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        descriptor = os.open(capability_path, flags, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(capability + "\n")
            handle.flush()
        go_cache = runtime / "go-build"
        go_mod_cache = runtime / "go-mod"
        go_path = runtime / "go-path"
        go_tmp = runtime / "go-tmp"
        go_preflight = runtime / "go-preflight"
        home = runtime / "home"
        temp = runtime / "tmp"
        xdg_cache = runtime / "xdg-cache"
        xdg_config = runtime / "xdg-config"
        zig_global = runtime / "zig-global"
        zig_local = runtime / "zig-local"
        shim = runtime / "bin"
        for directory in (
            go_cache,
            go_mod_cache,
            go_path,
            go_tmp,
            go_preflight,
            home,
            temp,
            xdg_cache,
            xdg_config,
            zig_global,
            zig_local,
            shim,
        ):
            directory.mkdir()
        (shim / "go").symlink_to(BOUNDED_GO)

        env = base_mutation_environment()
        if not env.get("PATH"):
            raise EvaluationError("mutation environment requires a non-empty PATH")
        original_path = env["PATH"]
        env.update(
            {
                "GOCACHE": str(go_cache),
                "CC": "zig cc",
                "CGO_ENABLED": "1",
                "GOMODCACHE": str(go_mod_cache),
                "GOPATH": str(go_path),
                "GOTMPDIR": str(go_tmp),
                "GOMAXPROCS": "1",
                "GOFLAGS": "",
                "GOTOOLCHAIN": "local",
                "GOWORK": "off",
                "HOME": str(home),
                "TMPDIR": str(temp),
                "XDG_CACHE_HOME": str(xdg_cache),
                "XDG_CONFIG_HOME": str(xdg_config),
                "ZIG_GLOBAL_CACHE_DIR": str(zig_global),
                "ZIG_LOCAL_CACHE_DIR": str(zig_local),
                "PATH": str(shim) + os.pathsep + env.get("PATH", ""),
                "TZ": "UTC",
                "GIT_CONFIG_GLOBAL": "/dev/null",
                "GIT_CONFIG_NOSYSTEM": "1",
                "READOUT_MUTATION_RUNTIME": str(runtime),
                "READOUT_MUTATION_CACHE_ROOT": str(root.resolve()),
                "READOUT_MUTATION_CAPABILITY": capability,
                "READOUT_ORIGINAL_PATH": original_path,
                "READOUT_REAL_GO": str(tool["go_path"]),
                "READOUT_GO_CACHE_STATE": str(runtime / "go-test-count"),
                "READOUT_GO_CACHE_GATE": str(runtime / "go-cache-gate"),
                "READOUT_GO_PREFLIGHT_DIR": str(go_preflight),
                "READOUT_GO_PREFLIGHT_TIMEOUT_SECONDS": "300",
                "READOUT_GO_COMPILE_REJECTIONS": str(
                    runtime / "compile-rejections.jsonl"
                ),
                "READOUT_GO_CACHE_MAX_BYTES": str(limits["cache_max_bytes"]),
                "READOUT_GO_CACHE_CHECK_EVERY": str(limits["cache_check_every"]),
                "READOUT_GO_MIN_FREE_BYTES": str(limits["minimum_free_bytes"]),
                "READOUT_GO_GUARD_FAILURE": str(runtime / "go-guard-failure"),
            }
        )
        disable_go_telemetry(str(tool["go_path"]), env, runtime)
        return runtime, env
    except BaseException:
        cleanup_runtime(runtime)
        raise


def directory_size_bytes(path: Path) -> int:
    if not path.exists():
        return 0
    du = resolve_system_executable("du")
    argv = [du, "-sk", str(path)]
    proc = subprocess.run(
        argv,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        # Active Go processes can remove cache entries between du's directory
        # scan and stat. One immediate retry avoids a spurious abort while any
        # persistent measurement failure still fails closed.
        proc = subprocess.run(
            argv,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    if proc.returncode != 0:
        raise EvaluationError(f"cannot measure {path}: {proc.stderr.strip()}")
    try:
        return int(proc.stdout.split()[0]) * 1024
    except (IndexError, ValueError) as exc:
        raise EvaluationError(f"cannot parse du output for {path}") from exc


def process_group_rss_kib(process_group_id: int) -> int:
    ps = resolve_system_executable("ps")
    proc = subprocess.run(
        [ps, "-axo", "pgid=,rss="],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        raise EvaluationError(f"cannot inspect mutation processes: {proc.stderr.strip()}")
    total = 0
    for line in proc.stdout.splitlines():
        fields = line.split()
        if len(fields) != 2:
            continue
        try:
            pgid, rss = (int(field) for field in fields)
        except ValueError:
            continue
        if pgid == process_group_id:
            total += rss
    return total


def stop_process_group(proc: subprocess.Popen[str]) -> None:
    if proc.poll() is not None and not process_group_rows(proc.pid):
        return
    with contextlib.suppress(OSError, ProcessLookupError):
        os.killpg(proc.pid, signal.SIGTERM)
    with contextlib.suppress(subprocess.TimeoutExpired):
        proc.wait(timeout=1)
    deadline = time.monotonic() + 5
    while process_group_rows(proc.pid) and time.monotonic() < deadline:
        time.sleep(0.1)
    if not process_group_rows(proc.pid):
        with contextlib.suppress(subprocess.TimeoutExpired):
            proc.wait(timeout=1)
        return
    with contextlib.suppress(OSError, ProcessLookupError):
        os.killpg(proc.pid, signal.SIGKILL)
    deadline = time.monotonic() + 5
    while process_group_rows(proc.pid) and time.monotonic() < deadline:
        time.sleep(0.1)
    with contextlib.suppress(subprocess.TimeoutExpired):
        proc.wait(timeout=1)
    remaining = process_group_rows(proc.pid)
    if remaining:
        raise EvaluationError(f"could not stop mutation process group {proc.pid}: {remaining!r}")


def read_go_guard_failure(runtime: Path) -> str | None:
    path = runtime / "go-guard-failure"
    if not path.exists():
        return None
    if path.is_symlink() or not path.is_file():
        return "Go guard failure marker was replaced with an unsafe file type"
    message = path.read_text(errors="replace").strip()
    return message[-2000:] or "bounded Go wrapper reported an unspecified failure"


def compile_rejection_records(runtime: Path) -> list[dict[str, Any]]:
    path = runtime / "compile-rejections.jsonl"
    if not path.exists():
        return []
    if path.is_symlink() or not path.is_file():
        raise EvaluationError("compile-rejection evidence is not a regular runner-owned file")
    records: list[dict[str, Any]] = []
    for line in path.read_text().splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError as exc:
            raise EvaluationError("compile-rejection evidence contains malformed JSON") from exc
        if (
            not isinstance(value, dict)
            or not isinstance(value.get("package"), str)
            or not isinstance(value.get("diagnostic_sha256"), str)
            or not re.fullmatch(r"[0-9a-f]{64}", value["diagnostic_sha256"])
            or not isinstance(value.get("diagnostic_count"), int)
            or isinstance(value.get("diagnostic_count"), bool)
            or value["diagnostic_count"] < 1
            or set(value)
            != {"package", "diagnostic_sha256", "diagnostic_count"}
        ):
            raise EvaluationError("compile-rejection evidence contains a malformed record")
        records.append(value)
    return records


def monitor_process(
    proc: subprocess.Popen[str],
    *,
    deadline: float,
    limits: dict[str, int],
    runtime: Path,
    label: str,
) -> tuple[str, str]:
    started = time.monotonic()
    next_heartbeat = started + HEARTBEAT_SECONDS
    next_resource_sample = started + limits["monitor_seconds"]
    while True:
        now = time.monotonic()
        remaining = deadline - now
        if remaining <= 0:
            stop_process_group(proc)
            raise ResourceLimitError(f"{label} exceeded the whole-run deadline")
        try:
            output = proc.communicate(timeout=min(GUARD_POLL_SECONDS, remaining))
        except subprocess.TimeoutExpired:
            guard_failure = read_go_guard_failure(runtime)
            if guard_failure:
                stop_process_group(proc)
                raise ResourceLimitError(f"{label} stopped by Go guard: {guard_failure}")
            now = time.monotonic()
            if now < next_resource_sample:
                continue
            free, runtime_bytes, rss_kib = enforce_resource_limits(
                proc, limits=limits, runtime=runtime, label=label
            )
            next_resource_sample = now + limits["monitor_seconds"]
            if now >= next_heartbeat:
                print(
                    f"{label}: still running; elapsed={(time.monotonic() - started) / 60:.1f}m "
                    f"rss={rss_kib / 1024**2:.2f}GiB runtime={runtime_bytes / GIB:.2f}GiB "
                    f"free={free / GIB:.1f}GiB",
                    file=sys.stderr,
                    flush=True,
                )
                next_heartbeat = now + HEARTBEAT_SECONDS
        else:
            # A short process can finish before the first periodic sample. Disk
            # and generated-data excess remain observable, so never accept its
            # output before enforcing the limits once more.
            enforce_resource_limits(
                proc,
                limits=limits,
                runtime=runtime,
                label=label,
            )
            return output


def enforce_resource_limits(
    proc: subprocess.Popen[str],
    *,
    limits: dict[str, int],
    runtime: Path,
    label: str,
) -> tuple[int, int, int]:
    free = min(shutil.disk_usage(ROOT).free, shutil.disk_usage(runtime).free)
    runtime_bytes = directory_size_bytes(runtime)
    rss_kib = process_group_rss_kib(proc.pid)
    failure = None
    if free < limits["minimum_free_bytes"]:
        failure = f"free disk fell below {limits['minimum_free_bytes'] / GIB:.1f} GiB"
    elif runtime_bytes > limits["runtime_max_bytes"]:
        failure = (
            f"isolated mutation runtime exceeded "
            f"{limits['runtime_max_bytes'] / GIB:.1f} GiB"
        )
    elif rss_kib * 1024 > limits["maximum_rss_bytes"]:
        failure = (
            f"mutation process group exceeded "
            f"{limits['maximum_rss_bytes'] / GIB:.1f} GiB RSS"
        )
    if failure:
        stop_process_group(proc)
        raise ResourceLimitError(f"{label} stopped by resource guard: {failure}")
    return free, runtime_bytes, rss_kib


def run_managed_go(
    real_go: str,
    arguments: list[str],
    *,
    cwd: Path,
    env: dict[str, str],
    deadline: float,
    limits: dict[str, int],
    runtime: Path,
    label: str,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    if not arguments or arguments[0] not in {"list", "test"}:
        raise EvaluationError(f"unsupported managed Go command: {arguments!r}")
    overlay = runtime / "managed-go-overlay.json"
    expected_overlay = {"Replace": {}}
    if overlay.exists():
        if overlay.is_symlink() or load_json(overlay) != expected_overlay:
            raise EvaluationError(f"unsafe managed Go overlay marker: {overlay}")
    else:
        atomic_json(overlay, expected_overlay)
    argv = [real_go, arguments[0], f"-overlay={overlay}", *arguments[1:]]
    proc = subprocess.Popen(
        argv,
        cwd=cwd,
        env=go_child_environment(env),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        write_runtime_owner(runtime, proc.pid, real_go, overlay)
        stdout, stderr = monitor_process(
            proc,
            deadline=deadline,
            limits=limits,
            runtime=runtime,
            label=label,
        )
    except BaseException:
        # Keep the child identity if termination cannot be proven. cleanup_runtime
        # will then refuse deletion and the next run can recover the process group.
        stop_process_group(proc)
        write_runtime_owner(runtime)
        raise
    remaining = process_group_rows(proc.pid)
    if remaining:
        stop_process_group(proc)
        write_runtime_owner(runtime)
        raise EvaluationError(f"{label} left child processes behind: {remaining!r}")
    write_runtime_owner(runtime)
    completed = subprocess.CompletedProcess(argv, proc.returncode, stdout, stderr)
    if check and proc.returncode != 0:
        raise EvaluationError(
            f"command failed ({proc.returncode}): {' '.join(argv)}\n"
            f"stdout:\n{stdout[-4000:]}\nstderr:\n{stderr[-4000:]}"
        )
    return completed


def gremlins_argv(
    tool: dict[str, Any],
    package: str,
    output: Path,
    workers: int,
    work_root: Path,
) -> list[str]:
    # Deliberately do not use Gremlins v0.6.0's --test-cpu option. That release
    # passes "-cpu 1" to `go test` as one malformed argv item, and exit 1 is then
    # misclassified as a killed mutant. GOMAXPROCS limits each test process.
    return [
        str(tool["gremlins_path"]),
        "--config",
        str(work_root / ".gremlins.yaml"),
        "--silent",
        "unleash",
        f"./{package}",
        "--output",
        str(output),
        "--workers",
        str(workers),
    ]


def run_gremlins(
    package: str,
    output: Path,
    *,
    deadline: float,
    env: dict[str, str],
    limits: dict[str, int],
    runtime: Path,
    tool: dict[str, Any],
    workers: int,
    work_root: Path,
) -> dict[str, Any]:
    rejections_before = len(compile_rejection_records(runtime))
    proc = subprocess.Popen(
        gremlins_argv(tool, package, output, workers, work_root),
        cwd=work_root,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        write_runtime_owner(
            runtime,
            proc.pid,
            str(tool["gremlins_path"]),
            work_root / ".gremlins.yaml",
        )
        stdout, stderr = monitor_process(
            proc,
            deadline=deadline,
            limits=limits,
            runtime=runtime,
            label=f"mutation shard {package}",
        )
    except BaseException:
        stop_process_group(proc)
        with contextlib.suppress(OSError, EvaluationError):
            write_runtime_owner(runtime)
        raise
    guard_failure = read_go_guard_failure(runtime)
    if guard_failure:
        stop_process_group(proc)
        write_runtime_owner(runtime)
        raise ResourceLimitError(f"mutation shard {package} stopped by Go guard: {guard_failure}")
    remaining = process_group_rows(proc.pid)
    if remaining:
        stop_process_group(proc)
        write_runtime_owner(runtime)
        raise EvaluationError(f"Gremlins left child processes behind for {package}: {remaining!r}")
    write_runtime_owner(runtime)
    if proc.returncode != 0 or not output.is_file():
        raise EvaluationError(
            f"Gremlins failed for {package} ({proc.returncode})\n"
            f"stdout:\n{stdout[-8000:]}\nstderr:\n{stderr[-8000:]}"
        )
    report = load_json(output)
    new_rejections = compile_rejection_records(runtime)[rejections_before:]
    module = report.get("go_module")
    allowed_packages = {f"./{package}"}
    if isinstance(module, str) and module:
        allowed_packages.add(f"{module}/{package}")
    if any(record["package"] not in allowed_packages for record in new_rejections):
        raise EvaluationError(
            f"compile-rejection evidence for {package} contains another package"
        )
    report["_readout"] = {
        "classifier": "build-preflight-v1",
        "compile_rejections": len(new_rejections),
        "compile_rejection_evidence": new_rejections,
    }
    return report


def summarize_report(package: str, report: dict[str, Any]) -> dict[str, Any]:
    files = report.get("files")
    if not isinstance(files, list) or not files:
        raise EvaluationError(f"{package}: report has no files")
    counts = {status: 0 for status in KNOWN_STATUSES}
    gaps: list[dict[str, Any]] = []
    for file_report in files:
        if not isinstance(file_report, dict):
            raise EvaluationError(f"{package}: malformed file entry")
        filename = file_report.get("file_name")
        mutations = file_report.get("mutations")
        if not isinstance(filename, str) or not isinstance(mutations, list):
            raise EvaluationError(f"{package}: malformed file report")
        filename_path = Path(filename)
        if not filename or filename_path.is_absolute() or ".." in filename_path.parts:
            raise EvaluationError(f"{package}: unsafe report filename: {filename!r}")
        if filename.endswith("_templ.go"):
            raise EvaluationError(f"{package}: excluded generated file leaked into report")
        for mutation in mutations:
            if not isinstance(mutation, dict) or mutation.get("status") not in KNOWN_STATUSES:
                raise EvaluationError(f"{package}: unknown or malformed mutation: {mutation!r}")
            mutation_type = mutation.get("type")
            line = mutation.get("line")
            column = mutation.get("column")
            if (
                not isinstance(mutation_type, str)
                or not mutation_type
                or not isinstance(line, int)
                or isinstance(line, bool)
                or line < 1
                or not isinstance(column, int)
                or isinstance(column, bool)
                or column < 1
            ):
                raise EvaluationError(f"{package}: mutation lacks valid type/location: {mutation!r}")
            status = str(mutation["status"])
            counts[status] += 1
            if status != "KILLED":
                gaps.append(
                    {
                        "file": f"{package}/{filename}",
                        "line": mutation.get("line"),
                        "column": mutation.get("column"),
                        "type": mutation.get("type"),
                        "status": status,
                    }
                )
    if counts["RUNNABLE"] or counts["SKIPPED"]:
        raise EvaluationError(f"{package}: incomplete run statuses: {counts}")
    if not sum(counts.values()):
        raise EvaluationError(f"{package}: report contains zero mutants")
    classifier = report.get("_readout")
    if (
        not isinstance(classifier, dict)
        or classifier.get("classifier") != "build-preflight-v1"
        or not isinstance(classifier.get("compile_rejections"), int)
        or isinstance(classifier.get("compile_rejections"), bool)
        or classifier["compile_rejections"] < 0
        or not isinstance(classifier.get("compile_rejection_evidence"), list)
        or len(classifier["compile_rejection_evidence"])
        != classifier["compile_rejections"]
    ):
        raise EvaluationError(f"{package}: report lacks valid build-preflight evidence")
    for record in classifier["compile_rejection_evidence"]:
        if (
            not isinstance(record, dict)
            or not isinstance(record.get("package"), str)
            or not isinstance(record.get("diagnostic_sha256"), str)
            or not re.fullmatch(r"[0-9a-f]{64}", record["diagnostic_sha256"])
            or not isinstance(record.get("diagnostic_count"), int)
            or isinstance(record.get("diagnostic_count"), bool)
            or record["diagnostic_count"] < 1
            or set(record)
            != {"package", "diagnostic_sha256", "diagnostic_count"}
        ):
            raise EvaluationError(f"{package}: malformed compile-rejection evidence")
        module = report.get("go_module")
        allowed_packages = {f"./{package}"}
        if isinstance(module, str) and module:
            allowed_packages.add(f"{module}/{package}")
        if record["package"] not in allowed_packages:
            raise EvaluationError(f"{package}: compile-rejection evidence package mismatch")
    if classifier["compile_rejections"] != counts["NOT VIABLE"]:
        raise EvaluationError(
            f"{package}: NOT VIABLE count is not fully explained by compile preflight"
        )
    expected = {
        "mutants_killed": counts["KILLED"],
        "mutants_lived": counts["LIVED"],
        "mutants_not_viable": counts["NOT VIABLE"],
        "mutants_not_covered": counts["NOT COVERED"],
    }
    for key, value in expected.items():
        if not isinstance(report.get(key), int) or isinstance(report.get(key), bool):
            raise EvaluationError(f"{package}: report summary {key} is not an integer")
        if report.get(key) != value:
            raise EvaluationError(
                f"{package}: report summary mismatch for {key}: "
                f"{report.get(key)!r} != {value}"
            )
    return {"package": package, "counts": counts, "gaps": gaps}


def calculate_metrics(summaries: list[dict[str, Any]]) -> dict[str, Any]:
    totals = {status: 0 for status in KNOWN_STATUSES}
    packages: dict[str, Any] = {}
    for summary in summaries:
        counts = summary["counts"]
        for status in KNOWN_STATUSES:
            totals[status] += int(counts[status])
        eligible = (
            int(counts["KILLED"])
            + int(counts["LIVED"])
            + int(counts["NOT COVERED"])
            + int(counts["TIMED OUT"])
        )
        packages[str(summary["package"])] = {
            "mutation_score": 100.0 * int(counts["KILLED"]) / eligible if eligible else 100.0,
            "mutants_killed": int(counts["KILLED"]),
            "mutants_lived": int(counts["LIVED"]),
            "mutants_not_covered": int(counts["NOT COVERED"]),
            "mutants_not_viable": int(counts["NOT VIABLE"]),
            "mutants_timed_out": int(counts["TIMED OUT"]),
        }
    eligible = totals["KILLED"] + totals["LIVED"] + totals["NOT COVERED"] + totals["TIMED OUT"]
    executed = totals["KILLED"] + totals["LIVED"]
    unresolved = (
        totals["LIVED"]
        + totals["NOT COVERED"]
        + totals["TIMED OUT"]
    )
    return {
        "mutants_total": sum(
            totals[status]
            for status in ("KILLED", "LIVED", "NOT COVERED", "NOT VIABLE", "TIMED OUT")
        ),
        "mutants_eligible": eligible,
        "mutants_killed": totals["KILLED"],
        "mutants_lived": totals["LIVED"],
        "mutants_not_covered": totals["NOT COVERED"],
        "mutants_not_viable": totals["NOT VIABLE"],
        "mutants_timed_out": totals["TIMED OUT"],
        "mutants_unresolved": unresolved,
        "mutation_score": 100.0 * totals["KILLED"] / eligible if eligible else 100.0,
        "gremlins_test_efficacy": (
            100.0 * totals["KILLED"] / executed if executed else 100.0
        ),
        "killed_is_direct_test_evidence": True,
        "packages": packages,
    }


def validated_sanity_summary(report: dict[str, Any]) -> dict[str, Any]:
    summary = summarize_report(SANITY_PACKAGE, report)
    counts = summary["counts"]
    expected_counts = {
        "KILLED": 2,
        "LIVED": 2,
        "NOT COVERED": 0,
        "NOT VIABLE": 1,
        "TIMED OUT": 0,
        "RUNNABLE": 0,
        "SKIPPED": 0,
    }
    if counts != expected_counts:
        raise EvaluationError(
            "Gremlins sanity failed: expected exact known killed/lived/compile-invalid "
            f"classification {expected_counts}, got {counts}. Refusing to trust the run."
        )
    return summary


def sanity_check(
    *,
    cache: Path,
    deadline: float,
    env: dict[str, str],
    limits: dict[str, int],
    runtime: Path,
    tool: dict[str, Any],
    environment_identity: dict[str, Any],
    work_root: Path,
    force: bool,
    expected_manifest: dict[str, Any],
) -> dict[str, Any]:
    fixture = work_root / SANITY_PACKAGE
    if not fixture.is_dir():
        raise EvaluationError(f"missing mutation sanity fixture: {fixture}")

    baseline = run_managed_go(
        str(tool["go_path"]),
        ["test", f"./{SANITY_PACKAGE}", "-count=1"],
        cwd=work_root,
        env=env,
        deadline=deadline,
        limits=limits,
        runtime=runtime,
        label="Go mutation sanity baseline",
        check=False,
    )
    if baseline.returncode != 0:
        raise EvaluationError(
            "mutation sanity fixture baseline failed\n"
            f"stdout:\n{baseline.stdout}\nstderr:\n{baseline.stderr}"
        )
    assert_inputs_unchanged(expected_manifest, work_root)

    sanity_key = sha256_bytes(
        b"readout-go-mutation-sanity-v1\0"
        + json.dumps(tool, sort_keys=True).encode()
        + json.dumps(go_environment(str(tool["go_path"]), env), sort_keys=True).encode()
        + json.dumps(environment_identity, sort_keys=True).encode()
        + CONFIG.read_bytes()
        + Path(__file__).read_bytes()
        + BOUNDED_GO.read_bytes()
        + b"".join(path.read_bytes() for path in sorted(fixture.glob("*.go")))
    )
    sanity_cache = ensure_owned_directory(cache, "sanity")
    report_path = sanity_cache / f"{sanity_key}.json"
    if report_path.is_file() and not force:
        report = load_json(report_path)
        summary = validated_sanity_summary(report)
        cache_hit = True
    else:
        report_path.parent.mkdir(parents=True, exist_ok=True)
        output = runtime / "tmp" / "sanity-report.json"
        report = run_gremlins(
            SANITY_PACKAGE,
            output,
            deadline=deadline,
            env=env,
            limits=limits,
            runtime=runtime,
            tool=tool,
            workers=1,
            work_root=work_root,
        )
        summary = validated_sanity_summary(report)
        assert_inputs_unchanged(expected_manifest, work_root)
        atomic_json(report_path, report)
        cache_hit = False
    counts = summary["counts"]
    return {
        "cache_hit": cache_hit,
        "counts": counts,
        "report_sha256": sha256_json(report),
        "report": report,
    }


def run_package(
    package: str,
    *,
    deadline: float,
    env: dict[str, str],
    limits: dict[str, int],
    runtime: Path,
    tool: dict[str, Any],
    workers: int,
    work_root: Path,
) -> dict[str, Any]:
    output = runtime / "tmp" / f"{package.replace('/', '_')}-{uuid.uuid4()}.json"
    started = time.monotonic()
    print(f"mutation shard {package}: running", file=sys.stderr, flush=True)
    report = run_gremlins(
        package,
        output,
        deadline=deadline,
        env=env,
        limits=limits,
        runtime=runtime,
        tool=tool,
        workers=workers,
        work_root=work_root,
    )
    summarize_report(package, report)
    print(
        f"mutation shard {package}: completed in {time.monotonic() - started:.1f}s",
        file=sys.stderr,
        flush=True,
    )
    return report


def self_test() -> None:
    summaries = [
        {
            "package": "internal/example",
            "counts": {
                "KILLED": 95,
                "LIVED": 2,
                "NOT COVERED": 3,
                "NOT VIABLE": 1,
                "TIMED OUT": 1,
                "RUNNABLE": 0,
                "SKIPPED": 0,
            },
        }
    ]
    metrics = calculate_metrics(summaries)
    assert metrics["mutants_total"] == 102, metrics
    assert metrics["mutants_unresolved"] == 6, metrics
    assert math.isclose(metrics["mutation_score"], 95.0 / 101.0 * 100.0), metrics
    assert math.isclose(
        metrics["gremlins_test_efficacy"],
        95.0 / 97.0 * 100.0,
    ), metrics
    assert bounded("workers", 2, 1, 4) == 2
    argv = gremlins_argv(
        {"gremlins_path": "/tools/gremlins"},
        "internal/example",
        Path("/tmp/report.json"),
        2,
        Path("/tmp/source"),
    )
    assert "--test-cpu" not in argv, argv
    print("Go mutation evaluator self-test: ok")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--full", action="store_true", help="ignore package result caches")
    parser.add_argument("--explain", action="store_true", help="print unresolved mutant locations")
    parser.add_argument("--check", action="store_true", help="check this run against strict policy")
    parser.add_argument("--check-report", action="store_true", help="check the latest fresh full report")
    parser.add_argument("--clean-cache", action="store_true", help="remove only runner-owned caches")
    parser.add_argument(
        "--max-not-covered",
        type=int,
        default=0,
        help="allowed NOT COVERED count with --check (default: 0)",
    )
    parser.add_argument(
        "--package",
        action="append",
        choices=PACKAGES,
        dest="packages",
        help="restrict the run to one package; repeat to select several",
    )
    parser.add_argument("--workers", type=positive_int, default=DEFAULT_WORKERS)
    parser.add_argument("--max-minutes", type=positive_int, default=DEFAULT_MAX_MINUTES)
    parser.add_argument("--cache-gib", type=positive_int, default=DEFAULT_CACHE_GIB)
    parser.add_argument("--runtime-gib", type=positive_int, default=DEFAULT_RUNTIME_GIB)
    parser.add_argument("--min-free-gib", type=positive_int, default=DEFAULT_MIN_FREE_GIB)
    parser.add_argument("--max-rss-gib", type=positive_int, default=DEFAULT_MAX_RSS_GIB)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--guard", action="store_true", help="run fast infrastructure checks")
    parser.add_argument("--verify-tool", action="store_true")
    parser.add_argument("--sanity-only", action="store_true")
    return parser.parse_args()


def limits_from_args(args: argparse.Namespace) -> dict[str, int]:
    bounded("workers", args.workers, 1, MAX_WORKERS)
    bounded("max-minutes", args.max_minutes, 1, MAX_MAX_MINUTES)
    bounded("cache-gib", args.cache_gib, 1, MAX_CACHE_GIB)
    bounded("runtime-gib", args.runtime_gib, 1, MAX_RUNTIME_GIB)
    bounded("min-free-gib", args.min_free_gib, MIN_MIN_FREE_GIB, 10_000)
    bounded("max-rss-gib", args.max_rss_gib, 1, MAX_MAX_RSS_GIB)
    if args.max_not_covered < 0:
        raise EvaluationError("--max-not-covered cannot be negative")
    return {
        "cache_max_bytes": args.cache_gib * GIB,
        "runtime_max_bytes": args.runtime_gib * GIB,
        "minimum_free_bytes": args.min_free_gib * GIB,
        "maximum_rss_bytes": args.max_rss_gib * GIB,
        "cache_check_every": DEFAULT_CACHE_CHECK_EVERY,
        "monitor_seconds": DEFAULT_MONITOR_SECONDS,
        "max_seconds": args.max_minutes * 60,
    }


def explain_gaps(summaries: list[dict[str, Any]]) -> None:
    for summary in summaries:
        for gap in summary["gaps"]:
            print(
                f"{gap['status']:11} {gap['type']} "
                f"{gap['file']}:{gap['line']}:{gap['column']}",
                file=sys.stderr,
            )


def check_metrics(metrics: dict[str, Any], max_not_covered: int) -> None:
    failures = []
    for key in ("mutants_lived", "mutants_timed_out"):
        if int(metrics[key]) != 0:
            failures.append(f"{key}={metrics[key]}")
    if int(metrics["mutants_not_covered"]) > max_not_covered:
        failures.append(
            f"mutants_not_covered={metrics['mutants_not_covered']} > {max_not_covered}"
        )
    if failures:
        raise QualityFailure("mutation quality check failed: " + ", ".join(failures))


def prune_result_cache(root: Path, keep: int = 6) -> None:
    sanity_root = ensure_owned_directory(root, "sanity")
    results_root = ensure_owned_directory(root, "results")
    for directory in [sanity_root, *results_root.glob("*")]:
        if not directory.is_dir() or directory.is_symlink():
            continue
        files = sorted(
            (path for path in directory.glob("*.json") if path.is_file()),
            key=lambda path: path.stat().st_mtime,
            reverse=True,
        )
        for path in files[keep:]:
            path.unlink()


def clean_owned_cache(root: Path) -> None:
    prune_stale_runtimes(ensure_owned_directory(root, "runtime"))
    for name in ("runtime", "results", "sanity"):
        candidate = root / name
        if candidate.is_symlink():
            raise EvaluationError(f"refusing symlinked mutation cache target: {candidate}")
        target = candidate.resolve()
        if target.parent != root.resolve():
            raise EvaluationError(f"refusing unexpected mutation cache target: {target}")
        if target.exists():
            remove_owned_directory(target, root)
    print(f"removed runner-owned Go mutation cache: {root}")


def current_input_hashes(manifest: dict[str, Any]) -> dict[str, Any]:
    return {
        "source_manifest_sha256": manifest["digest"],
        "source_file_count": len(manifest["files"]),
        "source_bytes": manifest["total_size"],
        "config_sha256": sha256_file(CONFIG),
        "evaluator_sha256": sha256_file(Path(__file__)),
        "bounded_go_cache_sha256": sha256_file(BOUNDED_GO),
    }


def is_fresh_full_attempt(complete_scope: bool, forced_full: bool, sanity_only: bool) -> bool:
    return complete_scope and forced_full and not sanity_only


def report_destination(complete_scope: bool, forced_full: bool) -> Path:
    if complete_scope and forced_full:
        return LATEST_REPORT
    if complete_scope:
        return LATEST_RESUME_REPORT
    return REPORT_DIR / "latest-partial.json"


def fast_guard(tool: dict[str, Any] | None = None) -> None:
    self_test()
    try:
        mise_config = tomllib.loads((ROOT / ".mise.toml").read_text())
        tools = mise_config["tools"]
    except (OSError, KeyError, tomllib.TOMLDecodeError) as exc:
        raise EvaluationError(f"cannot read mise tool pins: {exc}") from exc
    expected_pins = {
        "go": GO_VERSION,
        "python": PYTHON_VERSION,
        GREMLINS_MODULE.replace("github.com/", "github:"): GREMLINS_VERSION.removeprefix("v"),
    }
    for name, expected in expected_pins.items():
        if tools.get(name) != expected:
            raise EvaluationError(f".mise.toml must pin {name} to {expected}")
    actual_python = platform.python_version()
    if actual_python != PYTHON_VERSION:
        raise EvaluationError(
            f"Python {PYTHON_VERSION} is required, got {actual_python}; run through mise"
        )
    config = CONFIG.read_text()
    if "test-cpu" in config or "--test-cpu" in config:
        raise EvaluationError("Gremlins v0.6.0 test-cpu is forbidden by the honesty guard")
    if not BOUNDED_GO.is_file() or not os.access(BOUNDED_GO, os.X_OK):
        raise EvaluationError(f"bounded Go wrapper must be executable: {BOUNDED_GO}")
    for name in ("sanity.go", "sanity_test.go"):
        path = ROOT / SANITY_PACKAGE / name
        if not path.is_file():
            raise EvaluationError(f"missing mutation sanity fixture: {path}")
    real_go = str(tool["go_path"]) if tool else resolve_executable("go")
    go_pin = tools.get("go")
    go_version_output = run_checked([real_go, "version"]).stdout
    if not isinstance(go_pin, str) or f"go{go_pin} " not in go_version_output:
        raise EvaluationError(f"active Go does not match .mise.toml pin {go_pin!r}")
    with isolated_go_metadata_environment(real_go, reuse_module_cache=True) as env:
        verify_scope(real_go, ROOT, env)
        web_inputs = local_dependency_files("internal/web", real_go, ROOT, env)
        fakekube_root = ROOT / "internal" / "fakekube"
        if not any(path.is_relative_to(fakekube_root) for path in web_inputs):
            raise EvaluationError(
                "test-only dependency guard failed: internal/web no longer keys internal/fakekube"
            )
        if any(not path.is_relative_to(ROOT) for path in web_inputs):
            raise EvaluationError("generated external Go files leaked into the package cache key")
        guard_tool = tool or {
            "go_path": real_go,
            "gremlins_version": GREMLINS_VERSION,
            "gremlins_binary_sha256": "fast-guard",
        }
        key = package_key(
            "internal/web",
            guard_tool,
            go_environment(real_go, env),
            test_environment_identity(),
            DEFAULT_WORKERS,
            ROOT,
            env,
        )
    if not re.fullmatch(r"[0-9a-f]{64}", key):
        raise EvaluationError("package cache key guard returned malformed output")
    ignored_prefixes = (".git/", ".mise/", "node_modules/", "reports/mutation/")
    leaked = [
        str(path.relative_to(ROOT))
        for path in source_paths()
        if str(path.relative_to(ROOT)).startswith(ignored_prefixes)
    ]
    if leaked:
        raise EvaluationError("ignored bulk inputs leaked into mutation stage: " + ", ".join(leaked))
    print("Go mutation infrastructure guard: ok")


def check_latest_report(tool: dict[str, Any], max_not_covered: int) -> None:
    ensure_report_directory()
    if FULL_ATTEMPT.exists():
        raise EvaluationError(
            f"an unfinished fresh-full attempt is recorded at {FULL_ATTEMPT}; "
            "complete a new mutation-go-full run"
        )
    report = load_json(LATEST_REPORT)
    if report.get("schema_version") != REPORT_SCHEMA_VERSION:
        raise EvaluationError("latest Go mutation report has an unsupported schema")
    if report.get("scope") != list(PACKAGES):
        raise EvaluationError("latest Go mutation report does not cover the complete configured scope")
    if report.get("forced_full_run") is not True or report.get("cache_hits") != 0:
        raise EvaluationError("latest Go mutation report is not a fresh full run")
    if report.get("status_semantics") != STATUS_SEMANTICS:
        raise EvaluationError("latest Go mutation report does not declare current status semantics")
    sanity = report.get("sanity")
    if not isinstance(sanity, dict) or sanity.get("cache_hit") is not False:
        raise EvaluationError("latest full report lacks a fresh Gremlins sanity proof")
    manifest = source_manifest()
    inputs = report.get("inputs")
    if not isinstance(inputs, dict) or inputs != current_input_hashes(manifest):
        raise EvaluationError("latest Go mutation report does not match current source inputs")
    recorded_tool = report.get("tool")
    tool_keys = (
        "gremlins_version",
        "gremlins_binary_sha256",
        "gremlins_build_info_sha256",
        "go_binary_sha256",
        "go_build_info_sha256",
        "go_compile_sha256",
        "go_link_sha256",
        "cc_command_sha256",
        "cc_binary_sha256",
        "cc_version",
    )
    if (
        not isinstance(recorded_tool, dict)
        or any(not isinstance(tool.get(key), str) or not tool.get(key) for key in tool_keys)
        or any(recorded_tool.get(key) != tool.get(key) for key in tool_keys)
    ):
        raise EvaluationError("latest Go mutation report does not match the current Gremlins binary")
    if report.get("test_environment") != test_environment_identity():
        raise EvaluationError(
            "latest Go mutation report does not match the current test environment"
        )
    with isolated_go_metadata_environment(
        str(tool["go_path"]),
        reuse_module_cache=False,
    ) as current_env:
        if report.get("go_environment") != go_environment(
            str(tool["go_path"]), current_env
        ):
            raise EvaluationError(
                "latest Go mutation report does not match the current Go environment"
            )
    packages = report.get("packages")
    raw_reports = report.get("package_reports")
    evidence = report.get("package_evidence")
    if not isinstance(packages, list) or not isinstance(raw_reports, dict):
        raise EvaluationError("latest Go mutation report has malformed package summaries")
    if not isinstance(evidence, list) or len(evidence) != len(PACKAGES):
        raise EvaluationError("latest Go mutation report has malformed package evidence")
    evidence_by_package = {
        item.get("package"): item for item in evidence if isinstance(item, dict)
    }
    if set(raw_reports) != set(PACKAGES) or set(evidence_by_package) != set(PACKAGES):
        raise EvaluationError("latest Go mutation report package evidence does not match scope")
    recalculated_summaries = []
    for package in PACKAGES:
        raw = raw_reports[package]
        item = evidence_by_package[package]
        if not isinstance(raw, dict) or item.get("fresh") is not True:
            raise EvaluationError(f"latest Go mutation report is not fresh for {package}")
        if item.get("report_sha256") != sha256_json(raw):
            raise EvaluationError(f"latest Go mutation report evidence hash failed for {package}")
        recalculated_summaries.append(summarize_report(package, raw))
    if recalculated_summaries != packages:
        raise EvaluationError("latest Go mutation summaries do not match raw Gremlins reports")
    sanity_report = sanity.get("report") if isinstance(sanity, dict) else None
    if not isinstance(sanity_report, dict):
        raise EvaluationError("latest full report lacks its raw Gremlins sanity report")
    sanity_summary = validated_sanity_summary(sanity_report)
    if sanity.get("counts") != sanity_summary["counts"]:
        raise EvaluationError("latest full report sanity counts do not match raw report")
    if sanity.get("report_sha256") != sha256_json(sanity_report):
        raise EvaluationError("latest full report sanity evidence hash failed")
    recalculated = calculate_metrics(recalculated_summaries)
    if recalculated != report.get("metrics"):
        raise EvaluationError("latest Go mutation report metrics do not match its mutant records")
    check_metrics(recalculated, max_not_covered)
    print(f"Go mutation report verified: {LATEST_REPORT}")


def install_signal_handlers() -> None:
    def interrupt(_signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupt)
    if hasattr(signal, "SIGHUP"):
        signal.signal(signal.SIGHUP, interrupt)


def main() -> int:
    args = parse_args()
    if args.self_test:
        self_test()
        return 0
    if platform.system().lower() not in {"darwin", "linux"}:
        raise EvaluationError("the Go mutation resource guard supports only macOS and Linux")
    install_signal_handlers()
    cache = cache_root()
    if args.clean_cache:
        with exclusive_run_lock(cache):
            clean_owned_cache(cache)
        return 0

    validate_ambient_mutation_environment()

    if args.guard:
        fast_guard()
        return 0

    tool = verify_tool()
    if args.verify_tool:
        print(f"Gremlins {GREMLINS_VERSION} verified: {tool['gremlins_path']}")
        return 0
    if args.check_report:
        check_latest_report(tool, args.max_not_covered)
        return 0

    limits = limits_from_args(args)
    free = min(shutil.disk_usage(ROOT).free, shutil.disk_usage(cache).free)
    if free < limits["minimum_free_bytes"]:
        raise ResourceLimitError(
            f"at least {limits['minimum_free_bytes'] / GIB:.0f} GiB free disk is required"
        )
    selected_packages = tuple(dict.fromkeys(args.packages or PACKAGES))
    complete_scope = selected_packages == PACKAGES

    with exclusive_run_lock(cache):
        ensure_report_directory()
        started_git = git_state()
        started_wall = time.time()
        started = time.monotonic()
        deadline = started + limits["max_seconds"]
        environment_identity = test_environment_identity()
        runtime, env = mutation_environment(cache, tool, limits)
        runtime_cleaned = False
        try:
            manifest = source_manifest()
            run_id = str(uuid.uuid4())
            fresh_full_attempt = is_fresh_full_attempt(
                complete_scope,
                args.full,
                args.sanity_only,
            )
            if fresh_full_attempt:
                atomic_json(
                    FULL_ATTEMPT,
                    {
                        "run_id": run_id,
                        "started_at": iso_time(started_wall),
                        "source_manifest_sha256": manifest["digest"],
                        "tool": tool,
                    },
                )
            work_root = create_source_stage(runtime, manifest)
            assert_inputs_unchanged(manifest, work_root)
            scope = verify_scope(
                str(tool["go_path"]),
                work_root,
                env,
                runtime=runtime,
                limits=limits,
                deadline=deadline,
            )
            assert_inputs_unchanged(manifest, work_root)
            go_env = go_environment(str(tool["go_path"]), env)
            sanity = sanity_check(
                cache=cache,
                deadline=deadline,
                env=env,
                limits=limits,
                runtime=runtime,
                tool=tool,
                environment_identity=environment_identity,
                work_root=work_root,
                force=bool(args.full),
                expected_manifest=manifest,
            )
            print(
                "mutation sanity: ok" + (" (cache hit)" if sanity["cache_hit"] else ""),
                file=sys.stderr,
                flush=True,
            )
            if args.sanity_only:
                public_sanity = {key: value for key, value in sanity.items() if key != "report"}
                cleanup_runtime(runtime)
                runtime_cleaned = True
                print(json.dumps({"sanity": public_sanity, "scope": scope}, sort_keys=True))
                return 0

            summaries: list[dict[str, Any]] = []
            package_evidence: list[dict[str, Any]] = []
            package_reports: dict[str, dict[str, Any]] = {}
            cache_hits = 0
            for package in selected_packages:
                key = package_key(
                    package,
                    tool,
                    go_env,
                    environment_identity,
                    args.workers,
                    work_root,
                    env,
                    runtime=runtime,
                    limits=limits,
                    deadline=deadline,
                )
                assert_inputs_unchanged(manifest, work_root)
                result_dir = ensure_owned_directory(
                    cache,
                    "results",
                    package.replace("/", "_"),
                )
                report_path = result_dir / f"{key}.json"
                if report_path.is_file() and not args.full:
                    report = load_json(report_path)
                    summary = summarize_report(package, report)
                    cache_hits += 1
                    print(f"mutation shard {package}: cache hit", file=sys.stderr, flush=True)
                    fresh = False
                else:
                    report = run_package(
                        package,
                        deadline=deadline,
                        env=env,
                        limits=limits,
                        runtime=runtime,
                        tool=tool,
                        workers=args.workers,
                        work_root=work_root,
                    )
                    summary = summarize_report(package, report)
                    assert_inputs_unchanged(manifest, work_root)
                    atomic_json(report_path, report)
                    fresh = True
                summaries.append(summary)
                package_reports[package] = report
                package_evidence.append(
                    {
                        "package": package,
                        "cache_key": key,
                        "fresh": fresh,
                        "report_sha256": sha256_json(report),
                    }
                )

            assert_inputs_unchanged(manifest, work_root)
            metrics = calculate_metrics(summaries)
            completed_wall = time.time()
            latest = {
                "schema_version": REPORT_SCHEMA_VERSION,
                "run_id": run_id,
                "started_at": iso_time(started_wall),
                "completed_at": iso_time(completed_wall),
                "elapsed_seconds": round(time.monotonic() - started, 6),
                "scope": list(selected_packages),
                "scope_policy": scope,
                "status_semantics": STATUS_SEMANTICS,
                "forced_full_run": bool(args.full),
                "cache_hits": cache_hits,
                "git_at_start": started_git,
                "git_at_end": git_state(),
                "tool": tool,
                "go_environment": go_env,
                "test_environment": environment_identity,
                "limits": limits,
                "inputs": current_input_hashes(manifest),
                "sanity": sanity,
                "package_evidence": package_evidence,
                "package_reports": package_reports,
                "metrics": metrics,
                "packages": summaries,
            }
            report_path = report_destination(complete_scope, args.full)
            # Cleanup is part of the proof. Never publish a report or clear the
            # unfinished-full marker while a runtime or process owner remains.
            cleanup_runtime(runtime)
            runtime_cleaned = True
            atomic_json(report_path, latest)
            if fresh_full_attempt:
                FULL_ATTEMPT.unlink()
            print(f"mutation report: {report_path}", file=sys.stderr)
            prune_result_cache(cache)
            if args.explain:
                explain_gaps(summaries)
            print(json.dumps(metrics, sort_keys=True, separators=(",", ":")))
            if args.check:
                check_metrics(metrics, args.max_not_covered)
            return 0
        finally:
            if not runtime_cleaned and (
                runtime.exists() or runtime_owner_path(runtime).exists()
            ):
                cleanup_runtime(runtime)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except QualityFailure as exc:
        print(f"Go mutation quality failure: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
    except ResourceLimitError as exc:
        print(f"Go mutation resource guard: {exc}", file=sys.stderr)
        raise SystemExit(75) from exc
    except KeyboardInterrupt:
        print("Go mutation run interrupted", file=sys.stderr)
        raise SystemExit(130)
    except (EvaluationError, OSError, subprocess.TimeoutExpired) as exc:
        print(f"Go mutation error: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc
