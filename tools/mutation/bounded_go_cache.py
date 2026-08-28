#!/usr/bin/env python3
"""Run Go for Gremlins while bounding its isolated build cache."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import stat
import subprocess
import sys
import time
import uuid


class WrapperError(RuntimeError):
    pass


def positive_int(name: str) -> int:
    try:
        value = int(os.environ[name])
    except (KeyError, ValueError) as exc:
        raise WrapperError(f"{name} must be a positive integer") from exc
    if value < 1:
        raise WrapperError(f"{name} must be a positive integer")
    return value


def inside(path: Path, parent: Path) -> bool:
    try:
        path.relative_to(parent)
    except ValueError:
        return False
    return True


def runtime_paths() -> tuple[Path, Path, Path, Path]:
    runtime_raw = os.environ.get("READOUT_MUTATION_RUNTIME")
    cache_raw = os.environ.get("GOCACHE")
    state_raw = os.environ.get("READOUT_GO_CACHE_STATE")
    gate_raw = os.environ.get("READOUT_GO_CACHE_GATE")
    if not runtime_raw or not cache_raw or not state_raw or not gate_raw:
        raise WrapperError("bounded Go wrapper is missing its mutation environment")

    raw_paths = [Path(value) for value in (runtime_raw, cache_raw, state_raw, gate_raw)]
    if any(path.is_symlink() for path in raw_paths):
        raise WrapperError("mutation runtime and cache paths must not be symlinks")
    runtime, cache, state, gate = (path.resolve() for path in raw_paths)
    verify_runtime_capability(runtime)
    if not runtime.is_dir() or cache.parent != runtime or cache.name != "go-build":
        raise WrapperError("GOCACHE is not the runner-owned isolated cache")
    if not cache.is_dir() or cache.is_symlink():
        raise WrapperError("GOCACHE is not a plain runner-owned directory")
    if (
        state.parent != runtime
        or state.name != "go-test-count"
        or gate.parent != runtime
        or gate.name != "go-cache-gate"
    ):
        raise WrapperError("cache coordination files escaped the mutation runtime")
    return runtime, cache, state, gate


def verify_runtime_capability(runtime: Path) -> None:
    cache_root_raw = os.environ.get("READOUT_MUTATION_CACHE_ROOT")
    expected_token = os.environ.get("READOUT_MUTATION_CAPABILITY")
    if not cache_root_raw or not expected_token or not re.fullmatch(r"[0-9a-f]{64}", expected_token):
        raise WrapperError("bounded Go wrapper is missing its runner capability")
    cache_root_path = Path(cache_root_raw)
    if cache_root_path.is_symlink():
        raise WrapperError("mutation cache root must not be a symlink")
    cache_root = cache_root_path.resolve()
    runtime_root = cache_root / "runtime"
    if (
        cache_root.name != "readout-go-mutation"
        or not cache_root.is_dir()
        or runtime_root.is_symlink()
        or not runtime_root.is_dir()
        or runtime.parent != runtime_root
        or not re.fullmatch(r"run-[A-Za-z0-9_-]+", runtime.name)
    ):
        raise WrapperError("mutation runtime is outside the Git-owned cache hierarchy")
    git_common = cache_root.parent
    if not (git_common / "HEAD").is_file() or not (git_common / "objects").is_dir():
        raise WrapperError("mutation cache root is not inside a Git common directory")
    capability = runtime / ".runner-capability"
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(capability, flags)
    metadata = os.fstat(descriptor)
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_mode & 0o077:
        os.close(descriptor)
        raise WrapperError("mutation runner capability has unsafe permissions")
    with os.fdopen(descriptor, "r", encoding="utf-8") as handle:
        actual_token = handle.read(129).strip()
    if not secrets.compare_digest(actual_token, expected_token):
        raise WrapperError("mutation runner capability does not match")


def go_child_environment() -> dict[str, str]:
    child = {
        key: value
        for key, value in os.environ.items()
        if not key.startswith("READOUT_")
    }
    original_path = os.environ.get("READOUT_ORIGINAL_PATH")
    if original_path is not None:
        child["PATH"] = original_path
    return child


def real_go_path(runtime: Path) -> Path:
    raw = os.environ.get("READOUT_REAL_GO")
    if not raw:
        raise WrapperError("READOUT_REAL_GO is missing")
    real_go = Path(raw).resolve()
    if not real_go.is_file() or not os.access(real_go, os.X_OK):
        raise WrapperError(f"real Go executable is unavailable: {real_go}")
    if inside(real_go, runtime):
        raise WrapperError("real Go executable points back into the mutation runtime")
    return real_go


def record_guard_failure(message: str) -> None:
    runtime_raw = os.environ.get("READOUT_MUTATION_RUNTIME")
    failure_raw = os.environ.get("READOUT_GO_GUARD_FAILURE")
    if not runtime_raw or not failure_raw:
        return
    runtime = Path(runtime_raw).resolve()
    failure = Path(failure_raw).resolve(strict=False)
    if not runtime.is_dir() or failure.parent != runtime or failure.name != "go-guard-failure":
        return
    flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(failure, flags, 0o600)
        metadata = os.fstat(descriptor)
        if not stat.S_ISREG(metadata.st_mode):
            os.close(descriptor)
            return
        with os.fdopen(descriptor, "a", encoding="utf-8") as handle:
            handle.write(message.replace("\n", " ")[:1000] + "\n")
    except OSError:
        return


def directory_size_bytes(path: Path) -> int:
    if not path.exists():
        return 0
    total = 0
    pending = [path]
    while pending:
        directory = pending.pop()
        with os.scandir(directory) as entries:
            for entry in entries:
                if entry.is_symlink():
                    raise WrapperError(f"unexpected symlink in isolated Go cache: {entry.path}")
                metadata = entry.stat(follow_symlinks=False)
                if stat.S_ISDIR(metadata.st_mode):
                    pending.append(Path(entry.path))
                elif stat.S_ISREG(metadata.st_mode):
                    blocks = getattr(metadata, "st_blocks", 0)
                    total += blocks * 512 if blocks else metadata.st_size
                else:
                    raise WrapperError(f"unexpected file type in isolated Go cache: {entry.path}")
    return total


def ensure_disk_reserve(runtime: Path) -> None:
    minimum = positive_int("READOUT_GO_MIN_FREE_BYTES")
    free = shutil.disk_usage(runtime).free
    if free < minimum:
        raise WrapperError(
            f"free disk fell below the mutation safety floor "
            f"({free / 1024**3:.1f} GiB < {minimum / 1024**3:.1f} GiB)"
        )


def bump_counter(path: Path) -> int:
    descriptor = open_regular_coordination_file(path)
    with os.fdopen(descriptor, "r+", encoding="utf-8") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        handle.seek(0)
        try:
            value = int(handle.read().strip()) + 1
        except ValueError:
            value = 1
        handle.seek(0)
        handle.truncate()
        handle.write(f"{value}\n")
        handle.flush()
        return value


def open_regular_coordination_file(path: Path) -> int:
    flags = os.O_RDWR | os.O_CREAT
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags, 0o600)
    metadata = os.fstat(descriptor)
    if not stat.S_ISREG(metadata.st_mode):
        os.close(descriptor)
        raise WrapperError(f"cache coordination path is not a regular file: {path}")
    return descriptor


def clear_owned_cache(cache: Path, runtime: Path) -> None:
    if cache.is_symlink() or cache.parent != runtime or cache.name != "go-build":
        raise WrapperError("refusing to clear anything except the runner-owned Go cache")
    for child in cache.iterdir():
        if child.is_symlink():
            raise WrapperError(f"unexpected symlink in isolated Go cache: {child}")
        if child.is_dir():
            shutil.rmtree(child)
        elif child.is_file():
            child.unlink()
        else:
            raise WrapperError(f"unexpected file type in isolated Go cache: {child}")


def compile_probe_args(
    arguments: list[str],
    output: Path,
) -> tuple[list[str], str, float] | None:
    if not arguments or arguments[0] != "test" or "-failfast" not in arguments:
        return None
    if len(arguments) < 2:
        raise WrapperError("malformed Gremlins go test command")
    package = arguments[-1]
    if package.startswith("-"):
        raise WrapperError("Gremlins go test command has no package")
    build_flags: list[str] = []
    go_test_timeout: float | None = None
    index = 1
    while index < len(arguments) - 1:
        argument = arguments[index]
        if argument == "-tags":
            if index + 1 >= len(arguments) - 1:
                raise WrapperError("Gremlins go test -tags is missing its value")
            build_flags.extend((argument, arguments[index + 1]))
            index += 2
        elif argument == "-timeout":
            if index + 1 >= len(arguments) - 1:
                raise WrapperError("Gremlins go test -timeout is missing its value")
            if go_test_timeout is not None:
                raise WrapperError("Gremlins go test contains duplicate -timeout flags")
            go_test_timeout = go_duration_seconds(arguments[index + 1])
            index += 2
        elif argument == "-failfast":
            index += 1
        else:
            raise WrapperError(f"unexpected Gremlins go test argument: {argument}")
    if go_test_timeout is None:
        raise WrapperError("Gremlins go test command has no -timeout budget")
    return (
        ["test", *build_flags, "-c", "-o", str(output), package],
        package,
        go_test_timeout,
    )


GO_DURATION_UNITS = {
    "ns": 1e-9,
    "us": 1e-6,
    "µs": 1e-6,
    "μs": 1e-6,
    "ms": 1e-3,
    "s": 1.0,
    "m": 60.0,
    "h": 3600.0,
}
GO_DURATION_COMPONENT = re.compile(
    r"(?P<number>(?:\d+(?:\.\d*)?|\.\d+))(?P<unit>ns|us|µs|μs|ms|s|m|h)"
)


def go_duration_seconds(value: str) -> float:
    if not value or value.startswith(("-", "+")):
        raise WrapperError(f"invalid positive Go duration: {value!r}")
    position = 0
    total = 0.0
    while position < len(value):
        match = GO_DURATION_COMPONENT.match(value, position)
        if match is None:
            raise WrapperError(f"invalid positive Go duration: {value!r}")
        total += float(match.group("number")) * GO_DURATION_UNITS[match.group("unit")]
        position = match.end()
    if total <= 0 or total == float("inf"):
        raise WrapperError(f"invalid positive Go duration: {value!r}")
    return total


def preflight_timeout_seconds(go_test_timeout: float) -> float:
    # Gremlins gives `go test` two seconds more than its own process context.
    # Stop the build probe one additional second earlier so the wrapper can
    # report the guard failure and reap compiler descendants first.
    available = go_test_timeout - 3.0
    if available <= 0:
        raise WrapperError(
            f"Gremlins go test timeout {go_test_timeout:g}s is too short for safe preflight"
        )
    return min(float(positive_int("READOUT_GO_PREFLIGHT_TIMEOUT_SECONDS")), available)


def process_table() -> dict[int, int]:
    ps = next(
        (
            candidate
            for candidate in (Path("/bin/ps"), Path("/usr/bin/ps"))
            if candidate.is_file() and os.access(candidate, os.X_OK)
        ),
        None,
    )
    if ps is None:
        raise WrapperError("system ps is unavailable for compiler cleanup")
    proc = subprocess.run(
        [str(ps), "-axo", "pid=,ppid="],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        raise WrapperError(f"cannot inspect compiler process tree: {proc.stderr.strip()}")
    table: dict[int, int] = {}
    for line in proc.stdout.splitlines():
        fields = line.split()
        if len(fields) != 2:
            continue
        try:
            pid, parent = (int(field) for field in fields)
        except ValueError:
            continue
        table[pid] = parent
    return table


def descendants(root_pid: int, table: dict[int, int]) -> set[int]:
    children: dict[int, list[int]] = {}
    for pid, parent in table.items():
        children.setdefault(parent, []).append(pid)
    found: set[int] = set()
    pending = list(children.get(root_pid, ()))
    while pending:
        pid = pending.pop()
        if pid in found:
            continue
        found.add(pid)
        pending.extend(children.get(pid, ()))
    return found


def kill_compiler_tree(proc: subprocess.Popen[str]) -> None:
    targets = {proc.pid}
    with contextlib.suppress(ProcessLookupError):
        os.kill(proc.pid, signal.SIGSTOP)
    for _attempt in range(5):
        discovered = descendants(proc.pid, process_table())
        new_targets = discovered - targets
        targets.update(discovered)
        for pid in new_targets:
            with contextlib.suppress(ProcessLookupError):
                os.kill(pid, signal.SIGSTOP)
        if not new_targets:
            break
        time.sleep(0.02)
    for pid in targets - {proc.pid}:
        with contextlib.suppress(ProcessLookupError):
            os.kill(pid, signal.SIGKILL)
    with contextlib.suppress(ProcessLookupError):
        os.kill(proc.pid, signal.SIGKILL)
    try:
        proc.communicate(timeout=5)
    except subprocess.TimeoutExpired as exc:
        raise WrapperError("could not reap timed-out Go compile preflight") from exc
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        remaining = targets & process_table().keys()
        if not remaining:
            return
        time.sleep(0.05)
    remaining = sorted(targets & process_table().keys())
    if remaining:
        raise WrapperError(f"compiler descendants survived cleanup: {remaining}")


def preflight_directory(runtime: Path) -> Path:
    raw = os.environ.get("READOUT_GO_PREFLIGHT_DIR")
    if not raw:
        raise WrapperError("READOUT_GO_PREFLIGHT_DIR is missing")
    path = Path(raw)
    if path.is_symlink():
        raise WrapperError("Go compile-preflight directory must not be a symlink")
    resolved = path.resolve()
    if resolved.parent != runtime or resolved.name != "go-preflight" or not resolved.is_dir():
        raise WrapperError("Go compile-preflight directory escaped the mutation runtime")
    return resolved


def record_compile_rejection(
    runtime: Path,
    package: str,
    evidence: dict[str, str | int],
) -> None:
    raw = os.environ.get("READOUT_GO_COMPILE_REJECTIONS")
    if not raw:
        raise WrapperError("READOUT_GO_COMPILE_REJECTIONS is missing")
    path = Path(raw)
    if path.is_symlink():
        raise WrapperError("compile-rejection evidence must not be a symlink")
    resolved = path.resolve(strict=False)
    if (
        resolved.parent != runtime
        or resolved.name != "compile-rejections.jsonl"
    ):
        raise WrapperError("compile-rejection evidence escaped the mutation runtime")
    flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(resolved, flags, 0o600)
    metadata = os.fstat(descriptor)
    if not stat.S_ISREG(metadata.st_mode):
        os.close(descriptor)
        raise WrapperError("compile-rejection evidence is not a regular file")
    with os.fdopen(descriptor, "a", encoding="utf-8") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        handle.write(json.dumps({"package": package, **evidence}, sort_keys=True) + "\n")
        handle.flush()


def compiler_rejection_evidence(
    returncode: int,
    stdout: str,
    stderr: str,
    source_root: Path,
) -> dict[str, str | int] | None:
    if returncode != 1:
        return None
    diagnostic = stdout + ("\n" if stdout and stderr else "") + stderr
    lowered = diagnostic.lower()
    infrastructure_markers = (
        "cannot allocate memory",
        "connection refused",
        "dial tcp",
        "exec format error",
        "fork/exec",
        "i/o timeout",
        "internal compiler error",
        "module cache",
        "no space left on device",
        "out of memory",
        "resource temporarily unavailable",
        "signal: killed",
        "tls handshake timeout",
        "unexpected eof",
    )
    if not diagnostic or any(marker in lowered for marker in infrastructure_markers):
        return None
    if not any(line.startswith("# ") for line in diagnostic.splitlines()):
        return None
    source_root = source_root.resolve()
    matches = re.finditer(
        r"(?m)^(?P<path>(?:\./)?[^\s:\n]+\.go):\d+:\d+:\s+\S",
        diagnostic,
    )
    valid_diagnostics = 0
    for match in matches:
        raw_path = Path(match.group("path"))
        candidate = raw_path.resolve() if raw_path.is_absolute() else (source_root / raw_path).resolve()
        if inside(candidate, source_root):
            valid_diagnostics += 1
    if not valid_diagnostics:
        return None
    canonical = diagnostic.replace(str(source_root), "<source>")
    return {
        "diagnostic_count": valid_diagnostics,
        "diagnostic_sha256": hashlib.sha256(canonical.encode()).hexdigest(),
    }


def prove_mutant_compiles(real_go: Path, runtime: Path, gate_descriptor: int) -> bool:
    if not sys.argv[1:] or sys.argv[1] != "test" or "-failfast" not in sys.argv[1:]:
        return True
    preflight = preflight_directory(runtime)
    output = preflight / f"{os.getpid()}-{uuid.uuid4().hex}.test"
    probe = compile_probe_args(sys.argv[1:], output)
    if probe is None:
        raise WrapperError("failed to construct a compile preflight for mutant tests")
    arguments, package, go_test_timeout = probe
    try:
        proc = subprocess.Popen(
            [str(real_go), *arguments],
            env=go_child_environment(),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            pass_fds=(gate_descriptor,),
        )
        timeout_seconds = preflight_timeout_seconds(go_test_timeout)
        try:
            stdout, stderr = proc.communicate(timeout=timeout_seconds)
        except subprocess.TimeoutExpired as exc:
            message = f"Go build/vet preflight exceeded its {timeout_seconds:g}s limit"
            record_guard_failure(message)
            try:
                kill_compiler_tree(proc)
            except WrapperError as cleanup_error:
                raise cleanup_error from exc
            raise WrapperError(message) from exc
        except BaseException as exc:
            try:
                kill_compiler_tree(proc)
            except WrapperError as cleanup_error:
                raise cleanup_error from exc
            raise
        if proc.returncode != 0:
            if stdout:
                print(stdout, end="", file=sys.stdout, flush=True)
            if stderr:
                print(stderr, end="", file=sys.stderr, flush=True)
            evidence = compiler_rejection_evidence(
                proc.returncode,
                stdout,
                stderr,
                Path.cwd(),
            )
            if evidence is None:
                raise WrapperError(
                    "compile preflight failed without a confirmed in-source Go compiler "
                    f"diagnostic (exit {proc.returncode})"
                )
            record_compile_rejection(runtime, package, evidence)
            return False
        return True
    finally:
        with contextlib.suppress(FileNotFoundError):
            output.unlink()


def execution_arguments(arguments: list[str]) -> list[str]:
    if arguments and arguments[0] == "test" and "-failfast" in arguments:
        # Vet already passed in the non-executing preflight. Disable the second
        # vet invocation so exit 1 now comes from test/runtime execution rather
        # than a duplicate static-analysis phase.
        return ["test", "-vet=off", *arguments[1:]]
    return arguments


def main() -> int:
    runtime, cache, state, gate = runtime_paths()
    real_go = real_go_path(runtime)
    ensure_disk_reserve(runtime)
    should_measure = False
    if len(sys.argv) >= 2 and sys.argv[1] == "test":
        check_every = positive_int("READOUT_GO_CACHE_CHECK_EVERY")
        should_measure = bump_counter(state) % check_every == 0

    descriptor = open_regular_coordination_file(gate)
    turnstile = open_regular_coordination_file(runtime / "go-cache-turnstile")
    if should_measure:
        # Writer-preference turnstile: once a cleanup sample arrives, no new
        # Go reader can overtake it while existing readers drain.
        fcntl.flock(turnstile, fcntl.LOCK_EX)
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        limit = positive_int("READOUT_GO_CACHE_MAX_BYTES")
        if directory_size_bytes(cache) > limit:
            print(
                f"mutation cache exceeded {limit / 1024**3:.1f} GiB; "
                "clearing the isolated runner-owned cache",
                file=sys.stderr,
                flush=True,
            )
            clear_owned_cache(cache, runtime)
        fcntl.flock(descriptor, fcntl.LOCK_SH)
        fcntl.flock(turnstile, fcntl.LOCK_UN)
    else:
        # Every reader passes the turnstile exclusively but only holds it long
        # enough to acquire the shared gate. A waiting cleanup writer therefore
        # blocks new readers and cannot starve behind a continuous stream.
        fcntl.flock(turnstile, fcntl.LOCK_EX)
        fcntl.flock(descriptor, fcntl.LOCK_SH)
        fcntl.flock(turnstile, fcntl.LOCK_UN)
    os.close(turnstile)

    ensure_disk_reserve(runtime)
    if not prove_mutant_compiles(real_go, runtime, descriptor):
        return 2
    os.set_inheritable(descriptor, True)
    arguments = execution_arguments(sys.argv[1:])
    os.execve(str(real_go), [str(real_go), *arguments], go_child_environment())
    raise WrapperError("exec of the real Go binary unexpectedly returned")


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        record_guard_failure(str(exc))
        print(f"bounded Go cache error: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc
