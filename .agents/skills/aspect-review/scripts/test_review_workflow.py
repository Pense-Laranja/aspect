import ast
import asyncio
import contextlib
import io
import json
import os
import stat
import sys
import tempfile
import textwrap
import unittest

SCRIPT_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "review_workflow.py")


def load_workflow_namespace(wf_args=None, agent_fn=None, parallel_fn=None, workspaces=None):
    with open(SCRIPT_PATH, "r", encoding="utf-8") as f:
        source = f.read()

    phases = []
    logs = []

    async def default_parallel(items, fn):
        return await asyncio.gather(*(fn(it) for it in items))

    async def default_agent(prompt, **kwargs):
        return {"summary": "ok", "findings": [], "handoffs": []}

    ns = {
        "__name__": "__workflow__",
        "__file__": SCRIPT_PATH,
        "args": wf_args if wf_args is not None else {},
        "WORKSPACES": workspaces if workspaces is not None else [],
        "agent": agent_fn or default_agent,
        "parallel": parallel_fn or default_parallel,
        "phase": lambda name: phases.append(name),
        "log": lambda msg: logs.append(msg),
    }

    code = compile(source, SCRIPT_PATH, "exec", flags=ast.PyCF_ALLOW_TOP_LEVEL_AWAIT)
    stdout_buf = io.StringIO()

    async def runner():
        with contextlib.redirect_stdout(stdout_buf):
            coro = eval(code, ns)
            if asyncio.iscoroutine(coro):
                await coro

    return ns, phases, logs, stdout_buf, runner


def write_executable(path, content):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(content)
    os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def make_fake_aspect_script(state_file):
    return textwrap.dedent(
        f"""\
        #!{sys.executable}
        import json
        import os
        import sys

        with open({state_file!r}, "r", encoding="utf-8") as f:
            cfg = json.load(f)

        argv = sys.argv[1:]
        calls_path = cfg["calls_file"]
        with open(calls_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(argv) + "\\n")

        if argv[:3] == ["review", "prepare", "-h"]:
            sys.stderr.write("Usage of review prepare:\\n  -json\\n")
            sys.exit(2)

        if argv[:2] == ["review", "prepare"]:
            prep = cfg["prepare_output"]
            run_dir = prep.get("run_dir") or cfg["run_dir"]
            if prep.get("status") == "pending" and cfg.get("write_manifest", True):
                os.makedirs(run_dir, exist_ok=True)
                manifest = {{
                    "schema": "aspect-review/manifest/v1",
                    "run_id": prep["run_id"],
                    "engine": prep.get("engine", "antigravity (flash)"),
                    "tasks": prep.get("tasks") or [],
                }}
                with open(os.path.join(run_dir, "manifest.json"), "w", encoding="utf-8") as mf:
                    json.dump(manifest, mf)
            print(json.dumps(prep))
            sys.exit(0)

        if argv[:2] == ["review", "finalize"]:
            run_dir = cfg["run_dir"]
            fin = cfg.get("finalize", {{}})
            if fin.get("write_status", True):
                os.makedirs(run_dir, exist_ok=True)
                with open(os.path.join(run_dir, "status.json"), "w", encoding="utf-8") as sf:
                    json.dump({{"state": fin.get("state", "completed"), "outcome": fin.get("outcome", "PASS")}}, sf)
                with open(os.path.join(run_dir, "rendered.md"), "w", encoding="utf-8") as rf:
                    rf.write(fin.get("rendered", "## Review gates: pass\\n"))
            stdout = fin.get("stdout")
            if stdout is None and int(fin.get("exit_code", 0)) in (0, 1, 2) and fin.get("write_status", True):
                stdout = f"Aspect gate outcome: {{fin.get('outcome', 'PASS')}} — Engine: antigravity (flash)\\n"
            if stdout:
                sys.stdout.write(stdout)
            if fin.get("stderr"):
                sys.stderr.write(fin["stderr"])
            sys.exit(int(fin.get("exit_code", 0)))

        sys.exit(1)
        """
    )


class ReviewWorkflowTests(unittest.TestCase):
    def setUp(self):
        self._orig_env = os.environ.copy()
        self._home_tmp = tempfile.TemporaryDirectory()
        os.environ["HOME"] = self._home_tmp.name
        for key in ("ASPECT_BIN", "ASPECT_HOME", "ASPECT_ALLOW_REPO_EXEC", "GEMINI_API_KEY", "GOOGLE_API_KEY"):
            os.environ.pop(key, None)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self._orig_env)
        self._home_tmp.cleanup()

    def _load_helpers_only(self):
        with open(SCRIPT_PATH, "r", encoding="utf-8") as f:
            source = f.read()
        tree = ast.parse(source, filename=SCRIPT_PATH)
        filtered = [
            node
            for node in tree.body
            if not (
                isinstance(node, ast.Assign)
                and any(getattr(t, "id", "") == "RESULT" for t in node.targets)
            )
        ]
        mod = ast.Module(body=filtered, type_ignores=[])
        ns = {"__file__": SCRIPT_PATH, "phase": lambda _: None, "log": lambda _: None}
        exec(compile(mod, SCRIPT_PATH, "exec"), ns)
        return ns

    def test_script_has_only_stdlib_imports_and_compiles_with_top_level_await(self):
        with open(SCRIPT_PATH, "r", encoding="utf-8") as f:
            source = f.read()
        code = compile(source, SCRIPT_PATH, "exec", flags=ast.PyCF_ALLOW_TOP_LEVEL_AWAIT)
        self.assertIsNotNone(code)

        tree = ast.parse(source, filename=SCRIPT_PATH)
        allowed_stdlib = {"json", "os", "shutil", "subprocess"}
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    self.assertIn(alias.name.split(".")[0], allowed_stdlib)
            elif isinstance(node, ast.ImportFrom):
                self.assertEqual(node.level, 0, "relative imports are forbidden in run_workflow")
                self.assertIn((node.module or "").split(".")[0], allowed_stdlib)

    def test_resolve_aspect_cmd_fallback_chain_and_untrusted_repo_isolation(self):
        ns = self._load_helpers_only()
        resolve_aspect_cmd = ns["resolve_aspect_cmd"]

        with tempfile.TemporaryDirectory() as tmp:
            repo = os.path.join(tmp, "repo")
            os.makedirs(os.path.join(repo, ".claude", "review"))
            os.makedirs(os.path.join(repo, "cmd", "aspect"))
            calls_file = os.path.join(tmp, "calls.jsonl")
            open(calls_file, "w", encoding="utf-8").close()
            state_file = os.path.join(tmp, "state.json")
            with open(state_file, "w", encoding="utf-8") as f:
                json.dump({"calls_file": calls_file, "run_dir": repo, "prepare_output": {}}, f)

            # Plant a marker-writing .claude/review/lib.sh + aspect.lock AND cmd/aspect/main.go in repo.
            lib_marker = os.path.join(tmp, "lib_sh_ran")
            repo_aspect = os.path.join(tmp, "repo_bin", "aspect")
            write_executable(repo_aspect, make_fake_aspect_script(state_file))
            with open(os.path.join(repo, ".claude", "review", "aspect.lock"), "w", encoding="utf-8") as f:
                f.write("deadbeef\n")
            with open(os.path.join(repo, ".claude", "review", "lib.sh"), "w", encoding="utf-8") as f:
                f.write(
                    f"review_aspect() {{ : > {lib_marker!r}; export PATH={os.path.dirname(repo_aspect)!r}:$PATH; }}\n"
                )
            with open(os.path.join(repo, "cmd", "aspect", "main.go"), "w", encoding="utf-8") as f:
                f.write("package main\n")

            bin_dir = os.path.join(tmp, "bin")
            fake_go = os.path.join(bin_dir, "go")
            write_executable(
                fake_go,
                textwrap.dedent(
                    f"""\
                    #!{sys.executable}
                    import sys
                    if sys.argv[1:5] == ["run", "./cmd/aspect", "review", "prepare"]:
                        sys.stderr.write("  -json\\n")
                        sys.exit(2)
                    sys.exit(1)
                    """
                ),
            )
            # Symlink bash into bin_dir so lib.sh could run if permitted.
            real_bash = "/bin/bash"
            if os.path.exists(real_bash):
                os.symlink(real_bash, os.path.join(bin_dir, "bash"))

            # 1. Without allow_repo_exec, untrusted repo's lib.sh and cmd/aspect/main.go MUST NOT execute!
            with self.assertRaises(RuntimeError) as ctx:
                resolve_aspect_cmd(repo, env={"PATH": bin_dir})
            self.assertIn("go install", str(ctx.exception))
            self.assertFalse(os.path.exists(lib_marker), "untrusted repo lib.sh must not execute by default")

            # 2. Explicit invalid aspect_bin raises RuntimeError.
            with self.assertRaises(RuntimeError):
                resolve_aspect_cmd(repo, aspect_bin=os.path.join(tmp, "nonexistent"), env={"PATH": ""})

            # 3. With allow_repo_exec=True (or ASPECT_ALLOW_REPO_EXEC=1), repo-local lib.sh / go run works.
            if os.path.exists(real_bash):
                cmd, cwd = resolve_aspect_cmd(repo, env={"PATH": bin_dir}, allow_repo_exec=True)
                self.assertEqual(cmd, [repo_aspect])
                self.assertIsNone(cwd)
                self.assertTrue(os.path.exists(lib_marker))
                os.unlink(lib_marker)

            # Remove aspect.lock to test repo-local `go run ./cmd/aspect` under ASPECT_ALLOW_REPO_EXEC=1.
            os.unlink(os.path.join(repo, ".claude", "review", "aspect.lock"))
            cmd, cwd = resolve_aspect_cmd(repo, env={"PATH": bin_dir, "ASPECT_ALLOW_REPO_EXEC": "1"})
            self.assertEqual(cmd, [fake_go, "run", "./cmd/aspect"])
            self.assertEqual(cwd, os.path.abspath(repo))

            # 4. Trusted ASPECT_HOME wins without needing allow_repo_exec.
            aspect_src = os.path.join(tmp, "aspect_src")
            os.makedirs(os.path.join(aspect_src, "cmd", "aspect"))
            with open(os.path.join(aspect_src, "cmd", "aspect", "main.go"), "w", encoding="utf-8") as f:
                f.write("package main\n")
            cmd, cwd = resolve_aspect_cmd(repo, env={"PATH": bin_dir, "ASPECT_HOME": aspect_src})
            self.assertEqual(cmd, [fake_go, "run", "./cmd/aspect"])
            self.assertEqual(cwd, os.path.abspath(aspect_src))

            # 5. Trusted `aspect` on PATH wins over ASPECT_HOME and untrusted repo lib.sh.
            with open(os.path.join(repo, ".claude", "review", "aspect.lock"), "w", encoding="utf-8") as f:
                f.write("deadbeef\n")
            path_aspect = os.path.join(bin_dir, "aspect")
            write_executable(path_aspect, make_fake_aspect_script(state_file))
            cmd, cwd = resolve_aspect_cmd(
                repo, env={"PATH": bin_dir, "ASPECT_HOME": aspect_src}, allow_repo_exec=True
            )
            self.assertEqual(cmd, [path_aspect])
            self.assertIsNone(cwd)
            self.assertFalse(os.path.exists(lib_marker))

            # 6. ASPECT_BIN env var wins over PATH.
            env_aspect = os.path.join(tmp, "custom", "aspect-env")
            write_executable(env_aspect, make_fake_aspect_script(state_file))
            cmd, cwd = resolve_aspect_cmd(repo, env={"PATH": bin_dir, "ASPECT_BIN": env_aspect})
            self.assertEqual(cmd, [os.path.abspath(env_aspect)])
            self.assertIsNone(cwd)

            # 7. Explicit aspect_bin argument wins over ASPECT_BIN.
            arg_aspect = os.path.join(tmp, "custom", "aspect-arg")
            write_executable(arg_aspect, make_fake_aspect_script(state_file))
            cmd, cwd = resolve_aspect_cmd(
                repo, aspect_bin=arg_aspect, env={"PATH": bin_dir, "ASPECT_BIN": env_aspect}
            )
            self.assertEqual(cmd, [os.path.abspath(arg_aspect)])
            self.assertIsNone(cwd)

    def test_path_traversal_and_symlink_containment(self):
        ns = self._load_helpers_only()
        validate_run_dir = ns["_validate_run_dir"]
        resolve_safe_run_file = ns["_resolve_safe_run_file"]

        with tempfile.TemporaryDirectory() as tmp:
            outside = os.path.join(tmp, "outside")
            os.makedirs(os.path.join(outside, ".review", "local-1"))
            outside_file = os.path.join(outside, "secret.json")
            with open(outside_file, "w", encoding="utf-8") as f:
                f.write("{}")

            repo = os.path.join(tmp, "repo")
            run_dir = os.path.join(repo, ".review", "local-20261118T010000Z")
            os.makedirs(os.path.join(run_dir, "raw"))
            with open(os.path.join(run_dir, "manifest.json"), "w", encoding="utf-8") as mf:
                json.dump(
                    {
                        "schema": "aspect-review/manifest/v1",
                        "run_id": "local-20261118T010000Z",
                        "tasks": [],
                    },
                    mf,
                )

            run_real, manifest = validate_run_dir(run_dir, expected_repo=repo, require_manifest=True)
            self.assertEqual(manifest["run_id"], "local-20261118T010000Z")

            # Unsafe relative file paths rejected.
            for bad_rel in (
                "",
                "/etc/passwd",
                "../manifest.json",
                "raw/../../outside.json",
                r"raw\..\outside.json",
                "raw/.hidden.json",
            ):
                with self.assertRaises(RuntimeError, msg=f"accepted {bad_rel!r}"):
                    resolve_safe_run_file(run_real, bad_rel)

            # Symlinked output file rejected.
            os.symlink(outside_file, os.path.join(run_real, "raw", "link.json"))
            with self.assertRaises(RuntimeError):
                resolve_safe_run_file(run_real, "raw/link.json")

            # Dangling symlink inside raw/ rejected.
            os.symlink(os.path.join(outside, "missing.err"), os.path.join(run_real, "raw", "dangling.err"))
            with self.assertRaises(RuntimeError):
                resolve_safe_run_file(run_real, "raw/dangling.err")

            # Symlinked raw/ directory rejected.
            os.unlink(os.path.join(run_real, "raw", "link.json"))
            os.unlink(os.path.join(run_real, "raw", "dangling.err"))
            os.rmdir(os.path.join(run_real, "raw"))
            os.symlink(outside, os.path.join(run_real, "raw"))
            with self.assertRaises(RuntimeError):
                resolve_safe_run_file(run_real, "raw/sec.json")

            # Symlinked manifest.json rejected.
            os.unlink(os.path.join(run_dir, "manifest.json"))
            os.symlink(outside_file, os.path.join(run_dir, "manifest.json"))
            with self.assertRaises(RuntimeError):
                validate_run_dir(run_dir, expected_repo=repo, require_manifest=True)

            # Symlinked run_dir rejected.
            link_run = os.path.join(repo, ".review", "local-link")
            os.symlink(os.path.join(outside, ".review", "local-1"), link_run)
            with self.assertRaises(RuntimeError):
                validate_run_dir(link_run, expected_repo=repo, require_manifest=False)

            # Symlinked .review directory rejected.
            repo2 = os.path.join(tmp, "repo2")
            os.makedirs(repo2)
            os.symlink(os.path.join(outside, ".review"), os.path.join(repo2, ".review"))
            with self.assertRaises(RuntimeError):
                validate_run_dir(os.path.join(repo2, ".review", "local-1"), require_manifest=False)

    def test_batching_and_concurrency_clamping(self):
        ns = self._load_helpers_only()
        chunk_tasks = ns["chunk_tasks"]
        parse_max_concurrency = ns["parse_max_concurrency"]

        self.assertEqual(parse_max_concurrency(None), 4)
        self.assertEqual(parse_max_concurrency(""), 4)
        self.assertEqual(parse_max_concurrency("bad"), 4)
        self.assertEqual(parse_max_concurrency(0), 1)
        self.assertEqual(parse_max_concurrency(-5), 1)
        self.assertEqual(parse_max_concurrency("2"), 2)

        batches = chunk_tasks([1, 2, 3, 4, 5], 2)
        self.assertEqual(batches, [[1, 2], [3, 4], [5]])

    def test_reused_and_completed_short_circuit_and_flag_forwarding(self):
        for status_mode in ("reused", "completed"):
            with self.subTest(status=status_mode), tempfile.TemporaryDirectory() as tmp:
                repo = os.path.join(tmp, "repo")
                run_dir = os.path.join(repo, ".review", "local-20261118T010000Z")
                os.makedirs(run_dir)
                calls_file = os.path.join(tmp, "calls.jsonl")
                open(calls_file, "w", encoding="utf-8").close()
                state_file = os.path.join(tmp, "state.json")
                with open(state_file, "w", encoding="utf-8") as f:
                    json.dump(
                        {
                            "calls_file": calls_file,
                            "run_dir": run_dir,
                            "prepare_output": {
                                "run_id": "local-20261118T010000Z",
                                "run_dir": run_dir,
                                "status": status_mode,
                                "engine": "custom-engine",
                                "tasks": [],
                                "result": {
                                    "run_id": "local-20261118T010000Z",
                                    "run_dir": run_dir,
                                    "cached": status_mode == "reused",
                                    "outcome": "PASS",
                                    "rendered": "<!-- aspect-review-gates -->\n## Review gates: pass\n",
                                },
                            },
                        },
                        f,
                    )
                aspect_bin = os.path.join(tmp, "aspect")
                write_executable(aspect_bin, make_fake_aspect_script(state_file))

                agent_calls = []

                async def fake_agent(prompt, **kwargs):
                    agent_calls.append((prompt, kwargs))
                    return {}

                ns, phases, _, stdout_buf, runner = load_workflow_namespace(
                    wf_args={
                        "repo": repo,
                        "aspect_bin": aspect_bin,
                        "pr": "99",
                        "publish": True,
                        "force": True,
                        "agent": "red-team",
                        "engine": "custom-engine",
                    },
                    agent_fn=fake_agent,
                )
                asyncio.run(runner())

                self.assertEqual(len(agent_calls), 0)
                self.assertEqual(ns["RESULT"]["status"], status_mode)
                self.assertEqual(ns["RESULT"]["outcome"], "PASS")
                self.assertEqual(ns["RESULT"]["exit_code"], 0)
                self.assertEqual(phases, ["Prepare review gates"])
                self.assertIn("OUTCOME: PASS", stdout_buf.getvalue())

                with open(calls_file, "r", encoding="utf-8") as f:
                    invocations = [json.loads(line) for line in f if line.strip()]
                prep_calls = [inv for inv in invocations if inv[:2] == ["review", "prepare"] and "-h" not in inv]
                self.assertEqual(len(prep_calls), 1)
                for flag_tok in ("-pr", "99", "-publish", "-force", "-agent", "red-team", "-engine", "custom-engine"):
                    self.assertIn(flag_tok, prep_calls[0])
                self.assertFalse(any(inv[:2] == ["review", "finalize"] for inv in invocations))

    def test_error_isolation_in_parallel_batches_and_finalize_execution(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = os.path.join(tmp, "repo")
            run_dir = os.path.join(repo, ".review", "local-20261118T020000Z")
            os.makedirs(os.path.join(run_dir, "prompts"))
            os.makedirs(os.path.join(run_dir, "raw"))

            tasks = []
            for name in ["security-reviewer", "red-team", "quality-reviewer"]:
                prompt_rel = f"prompts/{name}.json"
                with open(os.path.join(run_dir, prompt_rel), "w", encoding="utf-8") as pf:
                    json.dump(
                        {
                            "task_id": name,
                            "agent": name,
                            "mode": "gate",
                            "part": 1,
                            "total_parts": 1,
                            "system_prompt": "sys",
                            "user_prompt": f"review {name}",
                            "report_schema": {"type": "object", "additionalProperties": False},
                        },
                        pf,
                    )
                tasks.append(
                    {
                        "task_id": name,
                        "agent": name,
                        "mode": "gate",
                        "part": 1,
                        "total_parts": 1,
                        "prompt_file": prompt_rel,
                        "output_file": f"raw/{name}.json",
                        "error_file": f"raw/{name}.err",
                    }
                )

            calls_file = os.path.join(tmp, "calls.jsonl")
            open(calls_file, "w", encoding="utf-8").close()
            state_file = os.path.join(tmp, "state.json")
            with open(state_file, "w", encoding="utf-8") as f:
                json.dump(
                    {
                        "calls_file": calls_file,
                        "run_dir": run_dir,
                        "prepare_output": {
                            "run_id": "local-20261118T020000Z",
                            "run_dir": run_dir,
                            "status": "pending",
                            "engine": "antigravity (flash)",
                            "tasks": tasks,
                        },
                        "finalize": {
                            "state": "completed",
                            "outcome": "BLOCK",
                            "exit_code": 1,
                            "stdout": "Aspect gate outcome: BLOCK — Engine: antigravity (flash)\n",
                            "rendered": "## Review gates: blocked\n",
                        },
                    },
                    f,
                )
            aspect_bin = os.path.join(tmp, "aspect")
            write_executable(aspect_bin, make_fake_aspect_script(state_file))

            batch_sizes = []
            agent_models = []

            async def tracking_parallel(items, fn):
                batch_sizes.append(len(items))
                return await asyncio.gather(*(fn(it) for it in items))

            async def flaky_agent(prompt, **kwargs):
                agent_models.append((kwargs.get("model"), kwargs.get("type_name"), kwargs.get("workspace")))
                if "red-team" in prompt:
                    raise RuntimeError("simulated flash timeout")
                return {"summary": f"clean {prompt}", "findings": [], "handoffs": []}

            ns, _, _, stdout_buf, runner = load_workflow_namespace(
                wf_args={"repo": repo, "aspect_bin": aspect_bin, "max_concurrency": 2},
                agent_fn=flaky_agent,
                parallel_fn=tracking_parallel,
            )
            asyncio.run(runner())

            self.assertEqual(batch_sizes, [2, 1])
            self.assertEqual(
                agent_models,
                [
                    ("flash", "aspect_gate_reviewer", "inherit"),
                    ("flash", "aspect_gate_reviewer", "inherit"),
                    ("flash", "aspect_gate_reviewer", "inherit"),
                ],
            )
            self.assertTrue(os.path.isfile(os.path.join(run_dir, "raw", "security-reviewer.json")))
            self.assertTrue(os.path.isfile(os.path.join(run_dir, "raw", "quality-reviewer.json")))
            self.assertFalse(os.path.exists(os.path.join(run_dir, "raw", "red-team.json")))
            with open(os.path.join(run_dir, "raw", "red-team.err"), "r", encoding="utf-8") as ef:
                self.assertIn("simulated flash timeout", ef.read())

            self.assertEqual(ns["RESULT"]["outcome"], "BLOCK")
            self.assertEqual(ns["RESULT"]["exit_code"], 1)
            self.assertIn("OUTCOME: BLOCK", stdout_buf.getvalue())

    def test_stale_status_json_does_not_mask_finalize_cli_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = os.path.join(tmp, "repo")
            run_dir = os.path.join(repo, ".review", "local-20261118T025000Z")
            os.makedirs(os.path.join(run_dir, "prompts"))
            os.makedirs(os.path.join(run_dir, "raw"))
            # Pre-seed stale completed PASS status.json.
            with open(os.path.join(run_dir, "status.json"), "w", encoding="utf-8") as sf:
                json.dump({"state": "completed", "outcome": "PASS"}, sf)
            with open(os.path.join(run_dir, "prompts", "sec.json"), "w", encoding="utf-8") as pf:
                json.dump({"user_prompt": "p", "report_schema": {}}, pf)

            calls_file = os.path.join(tmp, "calls.jsonl")
            open(calls_file, "w", encoding="utf-8").close()
            state_file = os.path.join(tmp, "state.json")
            with open(state_file, "w", encoding="utf-8") as f:
                json.dump(
                    {
                        "calls_file": calls_file,
                        "run_dir": run_dir,
                        "prepare_output": {
                            "run_id": "local-20261118T025000Z",
                            "run_dir": run_dir,
                            "status": "pending",
                            "tasks": [
                                {
                                    "task_id": "sec",
                                    "agent": "sec",
                                    "mode": "gate",
                                    "part": 1,
                                    "total_parts": 1,
                                    "prompt_file": "prompts/sec.json",
                                    "output_file": "raw/sec.json",
                                    "error_file": "raw/sec.err",
                                }
                            ],
                        },
                        "finalize": {
                            "write_status": False,
                            "exit_code": 1,
                            "stdout": "",
                            "stderr": "aspect: finalize: corrupt manifest\n",
                        },
                    },
                    f,
                )
            aspect_bin = os.path.join(tmp, "aspect")
            write_executable(aspect_bin, make_fake_aspect_script(state_file))

            _, _, _, _, runner = load_workflow_namespace(wf_args={"repo": repo, "aspect_bin": aspect_bin})
            with self.assertRaises(RuntimeError) as ctx:
                asyncio.run(runner())
            self.assertIn("corrupt manifest", str(ctx.exception))

    def test_existing_run_dir_retry_cleans_stale_artifacts_and_handles_multipart_str(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = os.path.join(tmp, "repo")
            run_dir = os.path.join(repo, ".review", "local-20261118T026000Z")
            os.makedirs(os.path.join(run_dir, "prompts"))
            os.makedirs(os.path.join(run_dir, "raw"))

            tasks = [
                {
                    "task_id": "sec.part-1",
                    "agent": "sec",
                    "mode": "gate",
                    "part": 1,
                    "total_parts": 2,
                    "prompt_file": "prompts/sec.part-1.json",
                    "output_file": "raw/sec.part-1.json",
                    "error_file": "raw/sec.part-1.err",
                },
                {
                    "task_id": "sec.part-2",
                    "agent": "sec",
                    "mode": "gate",
                    "part": 2,
                    "total_parts": 2,
                    "prompt_file": "prompts/sec.part-2.json",
                    "output_file": "raw/sec.part-2.json",
                    "error_file": "raw/sec.part-2.err",
                },
            ]
            for t in tasks:
                with open(os.path.join(run_dir, t["prompt_file"]), "w", encoding="utf-8") as pf:
                    json.dump({"user_prompt": f"prompt {t['task_id']}", "report_schema": {}}, pf)

            # Seed stale .err on part-1 (which will recover) and stale .json on part-2 (which will fail).
            with open(os.path.join(run_dir, "raw", "sec.part-1.err"), "w", encoding="utf-8") as f:
                f.write("old error\n")
            with open(os.path.join(run_dir, "raw", "sec.part-2.json"), "w", encoding="utf-8") as f:
                f.write('{"summary":"old"}\n')
            with open(os.path.join(run_dir, "manifest.json"), "w", encoding="utf-8") as mf:
                json.dump(
                    {
                        "schema": "aspect-review/manifest/v1",
                        "run_id": "local-20261118T026000Z",
                        "pr": "55",
                        "tasks": tasks,
                    },
                    mf,
                )

            calls_file = os.path.join(tmp, "calls.jsonl")
            open(calls_file, "w", encoding="utf-8").close()
            state_file = os.path.join(tmp, "state.json")
            with open(state_file, "w", encoding="utf-8") as f:
                json.dump(
                    {
                        "calls_file": calls_file,
                        "run_dir": run_dir,
                        "prepare_output": {},
                        "finalize": {"state": "completed", "outcome": "BLOCK", "exit_code": 1},
                    },
                    f,
                )
            aspect_bin = os.path.join(tmp, "aspect")
            write_executable(aspect_bin, make_fake_aspect_script(state_file))

            seen_roles = []

            async def retry_agent(prompt, **kwargs):
                seen_roles.append(kwargs.get("role"))
                if "part-1" in prompt:
                    # Return pre-serialized string JSON to exercise `isinstance(ans, str)`.
                    return '{"summary":"recovered","findings":[],"handoffs":[]}'
                raise RuntimeError("part 2 failed on retry")

            _, _, _, _, runner = load_workflow_namespace(
                wf_args={"run_dir": run_dir, "aspect_bin": aspect_bin, "publish": True},
                agent_fn=retry_agent,
            )
            asyncio.run(runner())

            self.assertEqual(seen_roles, ["sec (gate mode), part 1/2", "sec (gate mode), part 2/2"])
            self.assertTrue(os.path.isfile(os.path.join(run_dir, "raw", "sec.part-1.json")))
            self.assertFalse(
                os.path.exists(os.path.join(run_dir, "raw", "sec.part-1.err")),
                "stale .err must be unlinked when task succeeds on retry",
            )
            self.assertTrue(os.path.isfile(os.path.join(run_dir, "raw", "sec.part-2.err")))
            self.assertFalse(
                os.path.exists(os.path.join(run_dir, "raw", "sec.part-2.json")),
                "stale .json must be unlinked when task fails on retry",
            )

            with open(calls_file, "r", encoding="utf-8") as f:
                invocations = [json.loads(line) for line in f if line.strip()]
            fin_calls = [inv for inv in invocations if inv[:2] == ["review", "finalize"]]
            self.assertEqual(len(fin_calls), 1)
            self.assertIn("-pr", fin_calls[0])
            self.assertIn("55", fin_calls[0])
            self.assertIn("-publish", fin_calls[0])

    def test_unexpected_interruption_still_runs_finalize_in_finally(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = os.path.join(tmp, "repo")
            run_dir = os.path.join(repo, ".review", "local-20261118T030000Z")
            os.makedirs(os.path.join(run_dir, "prompts"))
            calls_file = os.path.join(tmp, "calls.jsonl")
            open(calls_file, "w", encoding="utf-8").close()
            state_file = os.path.join(tmp, "state.json")
            with open(state_file, "w", encoding="utf-8") as f:
                json.dump(
                    {
                        "calls_file": calls_file,
                        "run_dir": run_dir,
                        "prepare_output": {
                            "run_id": "local-20261118T030000Z",
                            "run_dir": run_dir,
                            "status": "pending",
                            "tasks": [
                                {
                                    "task_id": "security-reviewer",
                                    "agent": "security-reviewer",
                                    "mode": "gate",
                                    "part": 1,
                                    "total_parts": 1,
                                    "prompt_file": "prompts/security-reviewer.json",
                                    "output_file": "raw/security-reviewer.json",
                                    "error_file": "raw/security-reviewer.err",
                                }
                            ],
                        },
                        "finalize": {"state": "completed", "outcome": "BLOCK", "exit_code": 1},
                    },
                    f,
                )
            aspect_bin = os.path.join(tmp, "aspect")
            write_executable(aspect_bin, make_fake_aspect_script(state_file))

            async def crashing_parallel(items, fn):
                raise RuntimeError("orchestrator interrupted mid-batch")

            _, _, _, _, runner = load_workflow_namespace(
                wf_args={"repo": repo, "aspect_bin": aspect_bin},
                parallel_fn=crashing_parallel,
            )
            with self.assertRaises(RuntimeError) as ctx:
                asyncio.run(runner())
            self.assertIn("orchestrator interrupted mid-batch", str(ctx.exception))

            with open(calls_file, "r", encoding="utf-8") as f:
                invocations = [json.loads(line) for line in f if line.strip()]
            self.assertTrue(
                any(inv[:2] == ["review", "finalize"] for inv in invocations),
                f"finalize must execute in finally block; calls were: {invocations}",
            )


if __name__ == "__main__":
    unittest.main()
