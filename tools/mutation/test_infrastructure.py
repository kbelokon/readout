#!/usr/bin/env python3
"""Fast regression tests for the Go mutation harness itself."""

from __future__ import annotations

import os
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

from tools.mutation import bounded_go_cache as wrapper
from tools.mutation import evaluate


ROOT = Path(__file__).resolve().parents[2]
WRAPPER = ROOT / "tools" / "mutation" / "bounded_go_cache.py"


def wrapper_runtime(directory: str) -> tuple[Path, Path, dict[str, str]]:
    root = Path(directory).resolve()
    git_common = root / ".git"
    (git_common / "objects").mkdir(parents=True)
    (git_common / "HEAD").write_text("ref: refs/heads/test\n")
    cache_root = git_common / "readout-go-mutation"
    runtime = cache_root / "runtime" / "run-test"
    runtime.mkdir(parents=True)
    cache = runtime / "go-build"
    cache.mkdir()
    capability = "a" * 64
    capability_path = runtime / ".runner-capability"
    capability_path.write_text(capability + "\n")
    capability_path.chmod(0o600)
    return runtime, cache, {
        "READOUT_MUTATION_CACHE_ROOT": str(cache_root),
        "READOUT_MUTATION_CAPABILITY": capability,
    }


def report_with(statuses: list[str]) -> dict[str, object]:
    counts = {status: statuses.count(status) for status in evaluate.KNOWN_STATUSES}
    compile_evidence = [
        {
            "package": "./canary",
            "diagnostic_sha256": f"{index + 1:064x}",
            "diagnostic_count": 1,
        }
        for index in range(counts["NOT VIABLE"])
    ]
    return {
        "files": [
            {
                "file_name": "canary.go",
                "mutations": [
                    {
                        "status": status,
                        "line": index + 1,
                        "column": 1,
                        "type": "CONDITIONALS_NEGATION",
                    }
                    for index, status in enumerate(statuses)
                ],
            }
        ],
        "mutants_killed": counts["KILLED"],
        "mutants_lived": counts["LIVED"],
        "mutants_not_covered": counts["NOT COVERED"],
        "mutants_not_viable": counts["NOT VIABLE"],
        "_readout": {
            "classifier": "build-preflight-v1",
            "compile_rejections": counts["NOT VIABLE"],
            "compile_rejection_evidence": compile_evidence,
        },
    }


class MutationHarnessTests(unittest.TestCase):
    def test_evaluator_self_test(self) -> None:
        evaluate.self_test()

    def test_gremlins_command_cannot_enable_broken_test_cpu(self) -> None:
        argv = evaluate.gremlins_argv(
            {"gremlins_path": "/tools/gremlins"},
            "internal/web",
            Path("/tmp/report.json"),
            2,
            Path("/tmp/source"),
        )
        self.assertNotIn("--test-cpu", argv)
        self.assertNotIn("test-cpu", (ROOT / ".gremlins.yaml").read_text())

    def test_report_parser_counts_raw_mutants(self) -> None:
        summary = evaluate.summarize_report(
            "canary",
            report_with(["KILLED", "LIVED", "NOT COVERED", "NOT VIABLE", "TIMED OUT"]),
        )
        self.assertEqual(summary["counts"]["KILLED"], 1)
        self.assertEqual(summary["counts"]["LIVED"], 1)
        self.assertEqual(evaluate.calculate_metrics([summary])["mutants_total"], 5)

    def test_report_parser_rejects_unfinished_run(self) -> None:
        with self.assertRaises(evaluate.EvaluationError):
            evaluate.summarize_report("canary", report_with(["RUNNABLE"]))

    def test_report_parser_rejects_zero_or_unlocated_mutants(self) -> None:
        with self.assertRaises(evaluate.EvaluationError):
            evaluate.summarize_report("canary", report_with([]))
        malformed = report_with(["KILLED"])
        malformed["files"][0]["mutations"][0].pop("line")
        with self.assertRaises(evaluate.EvaluationError):
            evaluate.summarize_report("canary", malformed)

    def test_report_digest_detects_tampering(self) -> None:
        report = report_with(["KILLED", "LIVED"])
        digest = evaluate.sha256_json(report)
        report["mutants_killed"] = 0
        self.assertNotEqual(evaluate.sha256_json(report), digest)

    def test_resumable_run_cannot_replace_fresh_full_proof(self) -> None:
        self.assertEqual(evaluate.report_destination(True, True), evaluate.LATEST_REPORT)
        self.assertEqual(
            evaluate.report_destination(True, False),
            evaluate.LATEST_RESUME_REPORT,
        )
        self.assertFalse(evaluate.is_fresh_full_attempt(True, True, True))

    def test_full_report_checker_rejects_tampered_raw_evidence(self) -> None:
        go = shutil.which("go")
        self.assertIsNotNone(go)
        real_go = str(Path(str(go)).resolve())
        tool = {
            "gremlins_version": evaluate.GREMLINS_VERSION,
            "gremlins_binary_sha256": "gremlins-binary",
            "gremlins_build_info_sha256": "gremlins-build-info",
            "go_binary_sha256": "go-binary",
            "go_build_info_sha256": "go-build-info",
            "go_compile_sha256": "go-compile",
            "go_link_sha256": "go-link",
            "cc_command_sha256": "cc-command",
            "cc_binary_sha256": "cc-binary",
            "cc_version": evaluate.ZIG_VERSION,
            "go_path": real_go,
        }
        raw_reports = {
            package: report_with(["KILLED"]) for package in evaluate.PACKAGES
        }
        summaries = [
            evaluate.summarize_report(package, raw_reports[package])
            for package in evaluate.PACKAGES
        ]
        evidence = [
            {
                "package": package,
                "cache_key": f"key-{package}",
                "fresh": True,
                "report_sha256": evaluate.sha256_json(raw_reports[package]),
            }
            for package in evaluate.PACKAGES
        ]
        sanity_raw = report_with(
            ["KILLED", "KILLED", "LIVED", "LIVED", "NOT VIABLE"]
        )
        sanity_raw["_readout"]["compile_rejection_evidence"][0]["package"] = (
            f"./{evaluate.SANITY_PACKAGE}"
        )
        sanity_summary = evaluate.summarize_report(evaluate.SANITY_PACKAGE, sanity_raw)
        env = os.environ.copy()
        env.update(
            {
                "CC": "zig cc",
                "CGO_ENABLED": "1",
                "GOMAXPROCS": "1",
                "GOFLAGS": "",
                "GOTOOLCHAIN": "local",
                "GOWORK": "off",
            }
        )
        manifest = evaluate.source_manifest()
        environment_identity = evaluate.test_environment_identity()
        report = {
            "schema_version": evaluate.REPORT_SCHEMA_VERSION,
            "scope": list(evaluate.PACKAGES),
            "forced_full_run": True,
            "cache_hits": 0,
            "status_semantics": evaluate.STATUS_SEMANTICS,
            "sanity": {
                "cache_hit": False,
                "counts": sanity_summary["counts"],
                "report_sha256": evaluate.sha256_json(sanity_raw),
                "report": sanity_raw,
            },
            "inputs": evaluate.current_input_hashes(manifest),
            "tool": tool,
            "go_environment": evaluate.go_environment(real_go, env),
            "test_environment": environment_identity,
            "packages": summaries,
            "package_reports": raw_reports,
            "package_evidence": evidence,
            "metrics": evaluate.calculate_metrics(summaries),
        }
        with tempfile.TemporaryDirectory() as directory:
            latest = Path(directory) / "latest.json"
            full_attempt = Path(directory) / "full-attempt.json"
            evaluate.atomic_json(latest, report)
            with (
                mock.patch.object(evaluate, "LATEST_REPORT", latest),
                mock.patch.object(evaluate, "FULL_ATTEMPT", full_attempt),
            ):
                evaluate.check_latest_report(tool, 0)
                evaluate.atomic_json(full_attempt, {"run_id": "unfinished"})
                with self.assertRaises(evaluate.EvaluationError):
                    evaluate.check_latest_report(tool, 0)
                full_attempt.unlink()
                report["package_reports"][evaluate.PACKAGES[0]]["mutants_killed"] = 0
                evaluate.atomic_json(latest, report)
                with self.assertRaises(evaluate.EvaluationError):
                    evaluate.check_latest_report(tool, 0)

    def test_mutation_environment_drops_ambient_project_state(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "READOUT_SESSION_SECRET": "must-not-leak",
                "KUBECONFIG": "/tmp/must-not-leak",
                "GOOS": "linux",
                "GOWORK": "/tmp/must-not-leak.work",
            },
            clear=False,
        ):
            base = evaluate.base_mutation_environment()
            identity = evaluate.test_environment_identity()
        self.assertNotIn("READOUT_SESSION_SECRET", base)
        self.assertNotIn("KUBECONFIG", base)
        self.assertNotIn("GOOS", base)
        self.assertNotIn("GOWORK", base)
        self.assertNotIn("READOUT_SESSION_SECRET", identity["preserved_sha256"])
        self.assertNotIn("KUBECONFIG", identity["preserved_sha256"])
        self.assertTrue(base.get("PATH"))

    def test_nonempty_goflags_is_rejected(self) -> None:
        with mock.patch.dict(os.environ, {"GOFLAGS": "-overlay=/tmp/escape.json"}):
            with self.assertRaises(evaluate.EvaluationError):
                evaluate.validate_ambient_mutation_environment()

    def test_child_environment_removes_runner_state_and_restores_path(self) -> None:
        child = evaluate.go_child_environment(
            {
                "PATH": "/runtime/bin:/trusted/bin",
                "READOUT_ORIGINAL_PATH": "/trusted/bin",
                "READOUT_MUTATION_CAPABILITY": "secret",
                "GOCACHE": "/runtime/go-build",
            }
        )
        self.assertEqual(child["PATH"], "/trusted/bin")
        self.assertEqual(child["GOCACHE"], "/runtime/go-build")
        self.assertNotIn("READOUT_MUTATION_CAPABILITY", child)

    def test_executable_resolver_rejects_repository_shadowing(self) -> None:
        report_root = ROOT / "reports" / "mutation"
        report_root.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=report_root) as directory:
            shadow = Path(directory) / "synthetic-shadow"
            shadow.write_text("#!/bin/sh\nexit 0\n")
            shadow.chmod(0o755)
            with (
                mock.patch.dict(os.environ, {"PATH": str(shadow.parent)}),
                self.assertRaises(evaluate.EvaluationError),
            ):
                evaluate.resolve_executable("synthetic-shadow")

    def test_executable_resolver_rejects_relative_path_entries(self) -> None:
        with (
            mock.patch.dict(os.environ, {"PATH": "../sibling-bin:/usr/bin"}),
            self.assertRaises(evaluate.EvaluationError),
        ):
            evaluate.resolve_executable("git")

    def test_preflight_timeout_stays_inside_gremlins_deadline(self) -> None:
        self.assertAlmostEqual(wrapper.go_duration_seconds("1m2.5s"), 62.5)
        with mock.patch.dict(
            os.environ,
            {"READOUT_GO_PREFLIGHT_TIMEOUT_SECONDS": "300"},
        ):
            self.assertEqual(wrapper.preflight_timeout_seconds(12.0), 9.0)
            self.assertEqual(wrapper.preflight_timeout_seconds(900.0), 300.0)
        for value in ("", "0s", "-1s", "12", "nan"):
            with self.subTest(value=value), self.assertRaises(wrapper.WrapperError):
                wrapper.go_duration_seconds(value)

    def test_run_checked_timeout_reaps_its_descendant_group(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            child_pid = root / "child.pid"
            script = root / "spawn.py"
            script.write_text(
                "import pathlib, subprocess, sys, time\n"
                "child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\n"
                "pathlib.Path(sys.argv[1]).write_text(str(child.pid))\n"
                "time.sleep(60)\n"
            )
            with self.assertRaises(subprocess.TimeoutExpired):
                evaluate.run_checked(
                    [sys.executable, str(script), str(child_pid)],
                    timeout=1,
                )
            pid = int(child_pid.read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)

    def test_package_cache_file_identity_includes_executable_mode(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "exec-plugin.sh"
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o644)
            first = hashlib.sha256()
            evaluate.update_cache_key_with_file(first, path)
            path.chmod(0o755)
            second = hashlib.sha256()
            evaluate.update_cache_key_with_file(second, path)
        self.assertNotEqual(first.hexdigest(), second.hexdigest())

    def test_owned_cleanup_handles_read_only_go_module_directories(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory).resolve()
            runtime = parent / "run-test"
            module = runtime / "go-mod" / "example.com" / "module@v1.0.0"
            module.mkdir(parents=True)
            (module / "go.mod").write_text("module example.com/module\n")
            module.chmod(0o555)
            evaluate.remove_owned_directory(runtime, parent)
            self.assertFalse(runtime.exists())

    def test_runtime_sidecar_recovers_cleanup_interrupted_after_owner_removal(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            runtime_root = Path(directory).resolve()
            runtime = runtime_root / "run-partial"
            runtime.mkdir()
            evaluate.write_runtime_owner(runtime, state="cleaning")
            owner = evaluate.runtime_owner_path(runtime)
            value = json.loads(owner.read_text())
            value["pid"] = 999_999_999
            evaluate.atomic_json(owner, value)
            evaluate.prune_stale_runtimes(runtime_root)
            self.assertFalse(runtime.exists())
            self.assertFalse(owner.exists())

    def test_runtime_cleanup_refuses_a_live_owned_process_group(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            runtime_root = Path(directory).resolve()
            runtime = runtime_root / "run-live"
            runtime.mkdir()
            evaluate.write_runtime_owner(runtime, 4242, "/tool/go", runtime / "marker")
            with (
                mock.patch.object(
                    evaluate,
                    "process_group_rows",
                    return_value=[(4242, "/tool/go test")],
                ),
                self.assertRaises(evaluate.EvaluationError),
            ):
                evaluate.cleanup_runtime(runtime)
            self.assertTrue(runtime.is_dir())
            self.assertTrue(evaluate.runtime_owner_path(runtime).is_file())

    def test_mutation_stage_input_omits_ignored_bulk_directories(self) -> None:
        prefixes = (".git/", ".mise/", "node_modules/", "reports/mutation/")
        paths = [str(path.relative_to(ROOT)) for path in evaluate.source_paths()]
        self.assertFalse([path for path in paths if path.startswith(prefixes)])

    def test_source_manifest_rejects_an_accidental_huge_input(self) -> None:
        report_root = ROOT / "reports" / "mutation"
        report_root.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=report_root) as directory:
            large = Path(directory) / "accidental.bin"
            with large.open("wb") as handle:
                handle.truncate(evaluate.MAX_SOURCE_FILE_BYTES + 1)
            with (
                mock.patch.object(evaluate, "source_paths", return_value=[large]),
                self.assertRaises(evaluate.EvaluationError),
            ):
                evaluate.source_manifest()

    def test_source_manifest_rejects_symlinks_gremlins_would_drop(self) -> None:
        report_root = ROOT / "reports" / "mutation"
        report_root.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=report_root) as directory:
            target = Path(directory) / "target.txt"
            target.write_text("fixture")
            symlink = Path(directory) / "fixture-link"
            symlink.symlink_to(target.name)
            with (
                mock.patch.object(evaluate, "source_paths", return_value=[symlink]),
                self.assertRaises(evaluate.EvaluationError),
            ):
                evaluate.source_manifest()

    def test_bounded_go_wrapper_forwards_to_verified_binary(self) -> None:
        printf = shutil.which("printf")
        self.assertIsNotNone(printf)
        with tempfile.TemporaryDirectory() as directory:
            runtime, cache, capability_env = wrapper_runtime(directory)
            env = os.environ.copy()
            env.update(capability_env)
            env.update(
                {
                    "GOCACHE": str(cache),
                    "READOUT_MUTATION_RUNTIME": str(runtime),
                    "READOUT_REAL_GO": str(Path(str(printf)).resolve()),
                    "READOUT_GO_CACHE_STATE": str(runtime / "go-test-count"),
                    "READOUT_GO_CACHE_GATE": str(runtime / "go-cache-gate"),
                    "READOUT_GO_CACHE_MAX_BYTES": "1",
                    "READOUT_GO_CACHE_CHECK_EVERY": "1",
                    "READOUT_GO_MIN_FREE_BYTES": "1",
                    "READOUT_GO_GUARD_FAILURE": str(runtime / "go-guard-failure"),
                }
            )
            proc = subprocess.run(
                [sys.executable, str(WRAPPER), "version"],
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertFalse((runtime / "go-guard-failure").exists())
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stdout, "version")

    def test_bounded_go_wrapper_rejects_external_cache(self) -> None:
        printf = shutil.which("printf")
        self.assertIsNotNone(printf)
        with tempfile.TemporaryDirectory() as directory:
            runtime, _cache, capability_env = wrapper_runtime(directory)
            external_cache = runtime / "not-the-owned-cache"
            external_cache.mkdir()
            env = os.environ.copy()
            env.update(capability_env)
            env.update(
                {
                    "GOCACHE": str(external_cache),
                    "READOUT_MUTATION_RUNTIME": str(runtime),
                    "READOUT_REAL_GO": str(Path(str(printf)).resolve()),
                    "READOUT_GO_CACHE_STATE": str(runtime / "go-test-count"),
                    "READOUT_GO_CACHE_GATE": str(runtime / "go-cache-gate"),
                    "READOUT_GO_CACHE_MAX_BYTES": "1",
                    "READOUT_GO_CACHE_CHECK_EVERY": "1",
                    "READOUT_GO_MIN_FREE_BYTES": "1",
                    "READOUT_GO_GUARD_FAILURE": str(runtime / "go-guard-failure"),
                }
            )
            proc = subprocess.run(
                [sys.executable, str(WRAPPER), "version"],
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            guard_failure = (runtime / "go-guard-failure").read_text()
        self.assertEqual(proc.returncode, 2)
        self.assertIn("not the runner-owned isolated cache", proc.stderr)
        self.assertIn("not the runner-owned isolated cache", guard_failure)

    def test_bounded_go_wrapper_rejects_a_forged_global_cache_parent(self) -> None:
        printf = shutil.which("printf")
        self.assertIsNotNone(printf)
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory).resolve() / "run-fake"
            cache = runtime / "go-build"
            cache.mkdir(parents=True)
            sentinel = cache / "must-survive"
            sentinel.write_text("global cache stand-in")
            capability = "b" * 64
            (runtime / ".runner-capability").write_text(capability + "\n")
            (runtime / ".runner-capability").chmod(0o600)
            env = os.environ.copy()
            env.update(
                {
                    "GOCACHE": str(cache),
                    "READOUT_MUTATION_RUNTIME": str(runtime),
                    "READOUT_MUTATION_CACHE_ROOT": str(runtime.parent),
                    "READOUT_MUTATION_CAPABILITY": capability,
                    "READOUT_REAL_GO": str(Path(str(printf)).resolve()),
                    "READOUT_GO_CACHE_STATE": str(runtime / "go-test-count"),
                    "READOUT_GO_CACHE_GATE": str(runtime / "go-cache-gate"),
                    "READOUT_GO_CACHE_MAX_BYTES": "1",
                    "READOUT_GO_CACHE_CHECK_EVERY": "1",
                    "READOUT_GO_MIN_FREE_BYTES": "1",
                    "READOUT_GO_GUARD_FAILURE": str(runtime / "go-guard-failure"),
                }
            )
            proc = subprocess.run(
                [sys.executable, str(WRAPPER), "test"],
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertTrue(sentinel.is_file())
        self.assertEqual(proc.returncode, 2)
        self.assertIn("Git-owned cache hierarchy", proc.stderr)

    def test_bounded_go_wrapper_clears_only_its_over_limit_cache(self) -> None:
        printf = shutil.which("printf")
        self.assertIsNotNone(printf)
        with tempfile.TemporaryDirectory() as directory:
            runtime, cache, capability_env = wrapper_runtime(directory)
            sentinel = cache / "oversized"
            sentinel.write_bytes(b"x" * 4096)
            env = os.environ.copy()
            env.update(capability_env)
            env.update(
                {
                    "GOCACHE": str(cache),
                    "READOUT_MUTATION_RUNTIME": str(runtime),
                    "READOUT_REAL_GO": str(Path(str(printf)).resolve()),
                    "READOUT_GO_CACHE_STATE": str(runtime / "go-test-count"),
                    "READOUT_GO_CACHE_GATE": str(runtime / "go-cache-gate"),
                    "READOUT_GO_CACHE_MAX_BYTES": "1",
                    "READOUT_GO_CACHE_CHECK_EVERY": "1",
                    "READOUT_GO_MIN_FREE_BYTES": "1",
                    "READOUT_GO_GUARD_FAILURE": str(runtime / "go-guard-failure"),
                }
            )
            proc = subprocess.run(
                [sys.executable, str(WRAPPER), "test"],
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertFalse(sentinel.exists())
            self.assertFalse((runtime / "go-guard-failure").exists())
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stdout, "test")

    def test_compile_preflight_separates_invalid_mutants_from_test_kills(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            runtime, cache, capability_env = wrapper_runtime(directory)
            preflight = runtime / "go-preflight"
            preflight.mkdir()
            fake_go = root / "fake-go"
            fake_go.write_text(
                "#!/usr/bin/env python3\n"
                "import os, pathlib, subprocess, sys, time\n"
                "if '-c' in sys.argv:\n"
                "    if os.environ['FAKE_COMPILE_MODE'] == 'fail':\n"
                "        print('# ./pkg', file=sys.stderr)\n"
                "        print('tools/mutation/testdata/sanity/sanity.go:1:1: synthetic compile error', file=sys.stderr)\n"
                "        raise SystemExit(1)\n"
                "    if os.environ['FAKE_COMPILE_MODE'] == 'infra':\n"
                "        print('# ./pkg', file=sys.stderr)\n"
                "        print('tools/mutation/testdata/sanity/sanity.go:1:1: no space left on device', file=sys.stderr)\n"
                "        raise SystemExit(1)\n"
                "    if os.environ['FAKE_COMPILE_MODE'] == 'timeout':\n"
                "        child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\n"
                "        pathlib.Path(os.environ['FAKE_FULL_MARKER']).write_text(str(child.pid))\n"
                "        time.sleep(60)\n"
                "    output = pathlib.Path(sys.argv[sys.argv.index('-o') + 1])\n"
                "    output.write_text('compiled')\n"
                "    raise SystemExit(0)\n"
                "pathlib.Path(os.environ['FAKE_FULL_MARKER']).write_text('ran')\n"
                "raise SystemExit(1)\n"
            )
            fake_go.chmod(0o755)
            base_env = os.environ.copy()
            base_env.update(capability_env)
            base_env.update(
                {
                    "GOCACHE": str(cache),
                    "READOUT_MUTATION_RUNTIME": str(runtime),
                    "READOUT_REAL_GO": str(fake_go),
                    "READOUT_GO_CACHE_STATE": str(runtime / "go-test-count"),
                    "READOUT_GO_CACHE_GATE": str(runtime / "go-cache-gate"),
                    "READOUT_GO_CACHE_MAX_BYTES": str(1024**3),
                    "READOUT_GO_CACHE_CHECK_EVERY": "1",
                    "READOUT_GO_MIN_FREE_BYTES": "1",
                    "READOUT_GO_GUARD_FAILURE": str(runtime / "go-guard-failure"),
                    "READOUT_GO_PREFLIGHT_DIR": str(preflight),
                    "READOUT_GO_PREFLIGHT_TIMEOUT_SECONDS": "10",
                    "READOUT_GO_COMPILE_REJECTIONS": str(
                        runtime / "compile-rejections.jsonl"
                    ),
                }
            )

            compile_failure_marker = root / "full-after-compile-failure"
            failure_env = base_env | {
                "FAKE_COMPILE_MODE": "fail",
                "FAKE_FULL_MARKER": str(compile_failure_marker),
            }
            compile_failure = subprocess.run(
                [
                    sys.executable,
                    str(WRAPPER),
                    "test",
                    "-timeout",
                    "10s",
                    "-failfast",
                    "./pkg",
                ],
                env=failure_env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(compile_failure.returncode, 2, compile_failure.stderr)
            self.assertIn("synthetic compile error", compile_failure.stderr)
            self.assertFalse(compile_failure_marker.exists())
            rejection = runtime / "compile-rejections.jsonl"
            rejection_record = json.loads(rejection.read_text())
            self.assertEqual(rejection_record["package"], "./pkg")
            self.assertEqual(rejection_record["diagnostic_count"], 1)
            self.assertRegex(rejection_record["diagnostic_sha256"], r"^[0-9a-f]{64}$")

            rejection.unlink()
            infra_full_marker = root / "full-after-infra-failure"
            infra_env = base_env | {
                "FAKE_COMPILE_MODE": "infra",
                "FAKE_FULL_MARKER": str(infra_full_marker),
            }
            infra_failure = subprocess.run(
                [
                    sys.executable,
                    str(WRAPPER),
                    "test",
                    "-timeout",
                    "10s",
                    "-failfast",
                    "./pkg",
                ],
                env=infra_env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(infra_failure.returncode, 2, infra_failure.stderr)
            self.assertFalse(infra_full_marker.exists())
            self.assertFalse(rejection.exists())
            guard_failure = runtime / "go-guard-failure"
            self.assertIn("without a confirmed", guard_failure.read_text())
            guard_failure.unlink()

            timeout_pid_file = root / "compile-timeout-child"
            timeout_env = base_env | {
                "FAKE_COMPILE_MODE": "timeout",
                "FAKE_FULL_MARKER": str(timeout_pid_file),
            }
            timeout_failure = subprocess.run(
                [
                    sys.executable,
                    str(WRAPPER),
                    "test",
                    "-timeout",
                    "3.5s",
                    "-failfast",
                    "./pkg",
                ],
                env=timeout_env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=10,
                check=False,
            )
            self.assertEqual(timeout_failure.returncode, 2, timeout_failure.stderr)
            self.assertIn("preflight exceeded", timeout_failure.stderr)
            timeout_child_pid = int(timeout_pid_file.read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(timeout_child_pid, 0)
            guard_failure = runtime / "go-guard-failure"
            self.assertIn("preflight exceeded", guard_failure.read_text())
            guard_failure.unlink()

            test_failure_marker = root / "full-after-compile-success"
            success_env = base_env | {
                "FAKE_COMPILE_MODE": "pass",
                "FAKE_FULL_MARKER": str(test_failure_marker),
            }
            test_failure = subprocess.run(
                [
                    sys.executable,
                    str(WRAPPER),
                    "test",
                    "-timeout",
                    "10s",
                    "-failfast",
                    "./pkg",
                ],
                env=success_env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(test_failure.returncode, 1, test_failure.stderr)
            self.assertTrue(test_failure_marker.is_file())
            self.assertFalse(rejection.exists())
            self.assertFalse(list(preflight.iterdir()))
            self.assertFalse((runtime / "go-guard-failure").exists())


if __name__ == "__main__":
    unittest.main()
