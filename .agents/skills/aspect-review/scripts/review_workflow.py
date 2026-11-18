# Antigravity run_workflow script for Aspect Review Gates on Flash.
# NOTE: Must remain completely self-contained (stdlib imports only; run_workflow
# forbids local module imports and supports top-level `await`).

import json
import os
import shutil
import subprocess

MANIFEST_SCHEMA = "aspect-review/manifest/v1"
DEFAULT_MAX_CONCURRENCY = 4
INSTALL_HINT = (
    "Could not find an 'aspect' CLI supporting 'aspect review prepare'. "
    "Install or update it with: go install github.com/Pense-Laranja/aspect/cmd/aspect@latest"
)


def _parse_bool(val):
    if isinstance(val, bool):
        return val
    if isinstance(val, (int, float)):
        return bool(val)
    if isinstance(val, str):
        return val.strip().lower() in ("1", "true", "yes")
    return False


def _is_safe_segment(s):
    return (
        isinstance(s, str)
        and bool(s)
        and s not in (".", "..")
        and not s.startswith(".")
        and "/" not in s
        and "\\" not in s
        and os.path.basename(s) == s
    )


def _strictly_inside(base_real, target_path):
    base_clean = os.path.normpath(base_real)
    target_clean = os.path.normpath(target_path)
    if target_clean == base_clean:
        return False
    try:
        return os.path.commonpath([base_clean, target_clean]) == base_clean
    except ValueError:
        return False


def _check_no_symlink_escape(base_real, target_path):
    if not _strictly_inside(base_real, target_path):
        raise RuntimeError(f"Path {target_path!r} escapes {base_real!r}")
    rel = os.path.relpath(target_path, base_real)
    cur = base_real
    for part in rel.split(os.sep):
        cur = os.path.join(cur, part)
        if os.path.islink(cur):
            raise RuntimeError(f"Symlink {cur!r} is not permitted inside {base_real!r}")
        if not os.path.exists(cur):
            break
        resolved = os.path.realpath(cur)
        if not _strictly_inside(base_real, resolved):
            raise RuntimeError(f"Path {cur!r} resolves outside {base_real!r}")


def _resolve_safe_run_file(run_dir_real, rel_path):
    if not isinstance(rel_path, str) or not rel_path:
        raise RuntimeError(f"Invalid empty run file path: {rel_path!r}")
    if os.path.isabs(rel_path) or rel_path.startswith("/") or "\\" in rel_path:
        raise RuntimeError(f"Run file path must be slash-relative: {rel_path!r}")
    parts = rel_path.split("/")
    if any(not _is_safe_segment(p) for p in parts):
        raise RuntimeError(f"Run file path contains unsafe segment: {rel_path!r}")
    joined = os.path.normpath(os.path.join(run_dir_real, *parts))
    if not _strictly_inside(run_dir_real, joined):
        raise RuntimeError(f"Run file path escapes run directory: {rel_path!r}")
    _check_no_symlink_escape(run_dir_real, joined)
    return joined


def _validate_run_dir(run_dir_path, expected_repo=None, require_manifest=True):
    if not isinstance(run_dir_path, str) or not run_dir_path.strip():
        raise RuntimeError(f"Invalid run directory path: {run_dir_path!r}")
    run_dir_abs = os.path.abspath(run_dir_path)
    if os.path.islink(run_dir_abs):
        raise RuntimeError(f"Run directory must not be a symlink: {run_dir_path!r}")

    parent_abs = os.path.dirname(run_dir_abs)
    if os.path.basename(parent_abs) != ".review" or not _is_safe_segment(os.path.basename(run_dir_abs)):
        raise RuntimeError(f"Run directory must be <repo>/.review/<run-id>: {run_dir_path!r}")
    if os.path.islink(parent_abs):
        raise RuntimeError(f".review directory must not be a symlink: {parent_abs!r}")

    inferred_repo_abs = os.path.dirname(parent_abs)
    inferred_repo_real = os.path.realpath(inferred_repo_abs)
    _check_no_symlink_escape(inferred_repo_real, os.path.join(inferred_repo_real, ".review"))

    review_real = os.path.realpath(parent_abs)
    run_dir_real = os.path.realpath(run_dir_abs)
    if not os.path.isdir(run_dir_real):
        raise RuntimeError(f"Run directory does not exist: {run_dir_path!r}")

    run_id = os.path.basename(run_dir_real)
    if os.path.basename(os.path.dirname(run_dir_real)) != ".review" or not _is_safe_segment(run_id):
        raise RuntimeError(f"Resolved run directory invalid: {run_dir_real!r}")
    if os.path.dirname(run_dir_real) != review_real or os.path.dirname(review_real) != inferred_repo_real:
        raise RuntimeError(f"Run directory escapes repository .review: {run_dir_path!r}")

    if expected_repo:
        expected_repo_real = os.path.realpath(os.path.abspath(expected_repo))
        if inferred_repo_real != expected_repo_real:
            raise RuntimeError(
                f"Run directory {run_dir_path!r} does not belong to repository {expected_repo!r}"
            )

    manifest = None
    manifest_cand = os.path.join(run_dir_real, "manifest.json")
    if require_manifest or os.path.lexists(manifest_cand):
        manifest_path = _resolve_safe_run_file(run_dir_real, "manifest.json")
        if not os.path.isfile(manifest_path):
            raise RuntimeError(f"Missing manifest.json in run directory: {run_dir_path!r}")
        with open(manifest_path, "r", encoding="utf-8") as f:
            manifest = json.load(f)
        if not isinstance(manifest, dict) or manifest.get("schema") != MANIFEST_SCHEMA:
            raise RuntimeError(
                f"Invalid manifest schema {manifest.get('schema') if isinstance(manifest, dict) else None!r} "
                f"(want {MANIFEST_SCHEMA!r})"
            )
        if manifest.get("run_id") != run_id:
            raise RuntimeError(
                f"Manifest run_id {manifest.get('run_id')!r} does not match directory {run_id!r}"
            )

    return run_dir_real, manifest


def _supports_prepare(cmd_prefix, cwd=None, env=None):
    try:
        proc = subprocess.run(
            list(cmd_prefix) + ["review", "prepare", "-h"],
            cwd=cwd,
            env=env,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    combined = (proc.stdout or "") + "\n" + (proc.stderr or "")
    return "-json" in combined


def resolve_aspect_cmd(repo, aspect_bin=None, env=None, allow_repo_exec=False):
    """Resolve an aspect command prefix and optional working-directory override.

    Order of precedence (trusted sources first):
      1. Explicit `aspect_bin` argument or `ASPECT_BIN` environment variable.
      2. Trusted `aspect` on `PATH`.
      3. `ASPECT_HOME` source checkout (`go run ./cmd/aspect`).
      4. Opt-in only (`allow_repo_exec=True` or `ASPECT_ALLOW_REPO_EXEC=1`):
         repo-local `.claude/review/lib.sh` + `aspect.lock` or `go run ./cmd/aspect` in `repo`.
    """
    run_env = dict(os.environ if env is None else env)
    repo_abs = os.path.abspath(repo)
    repo_exec_allowed = _parse_bool(allow_repo_exec) or _parse_bool(
        run_env.get("ASPECT_ALLOW_REPO_EXEC")
    )

    # 1. Explicit aspect_bin argument or ASPECT_BIN environment variable.
    explicit = aspect_bin or run_env.get("ASPECT_BIN") or ""
    if explicit:
        cand = explicit
        if not os.path.isabs(cand) and os.sep not in cand:
            found = shutil.which(cand, path=run_env.get("PATH"))
            if found:
                cand = found
        cand = os.path.abspath(cand)
        if (
            os.path.isfile(cand)
            and os.access(cand, os.X_OK)
            and _supports_prepare([cand], cwd=repo_abs, env=run_env)
        ):
            return [cand], None
        if aspect_bin:
            raise RuntimeError(
                f"Configured aspect_bin {aspect_bin!r} is missing, not executable, or lacks 'aspect review prepare'."
            )

    # 2. Trusted `aspect` on PATH.
    which_aspect = shutil.which("aspect", path=run_env.get("PATH"))
    if which_aspect and _supports_prepare([which_aspect], cwd=repo_abs, env=run_env):
        return [which_aspect], None

    # 3. Trusted ASPECT_HOME source checkout (`go run ./cmd/aspect`).
    go_bin = shutil.which("go", path=run_env.get("PATH"))
    aspect_home = run_env.get("ASPECT_HOME") or ""
    if go_bin and aspect_home:
        src_dir = os.path.abspath(aspect_home)
        main_go = os.path.join(src_dir, "cmd", "aspect", "main.go")
        if os.path.isfile(main_go) and _supports_prepare(
            [go_bin, "run", "./cmd/aspect"], cwd=src_dir, env=run_env
        ):
            return [go_bin, "run", "./cmd/aspect"], src_dir

    # 4. Opt-in repo-local helpers when explicitly permitted.
    if repo_exec_allowed:
        lib_sh = os.path.join(repo_abs, ".claude", "review", "lib.sh")
        lock_file = os.path.join(repo_abs, ".claude", "review", "aspect.lock")
        bash_bin = shutil.which("bash", path=run_env.get("PATH"))
        if bash_bin and os.path.isfile(lib_sh) and os.path.isfile(lock_file):
            try:
                proc = subprocess.run(
                    [
                        bash_bin,
                        "-c",
                        ". .claude/review/lib.sh && review_aspect >/dev/null && command -v aspect",
                    ],
                    cwd=repo_abs,
                    env=run_env,
                    capture_output=True,
                    text=True,
                    timeout=120,
                )
                if proc.returncode == 0:
                    lines = [
                        ln.strip()
                        for ln in (proc.stdout or "").splitlines()
                        if ln.strip()
                    ]
                    if lines:
                        cand = lines[-1]
                        if (
                            os.path.isfile(cand)
                            and os.access(cand, os.X_OK)
                            and _supports_prepare([cand], cwd=repo_abs, env=run_env)
                        ):
                            return [cand], None
            except (OSError, subprocess.SubprocessError):
                pass

        if go_bin:
            main_go = os.path.join(repo_abs, "cmd", "aspect", "main.go")
            if os.path.isfile(main_go) and _supports_prepare(
                [go_bin, "run", "./cmd/aspect"], cwd=repo_abs, env=run_env
            ):
                return [go_bin, "run", "./cmd/aspect"], repo_abs

    raise RuntimeError(INSTALL_HINT)


def parse_max_concurrency(raw):
    if raw is None or raw == "":
        return DEFAULT_MAX_CONCURRENCY
    try:
        val = int(raw)
    except (TypeError, ValueError):
        return DEFAULT_MAX_CONCURRENCY
    return max(1, val)


def chunk_tasks(tasks, batch_size):
    size = max(1, int(batch_size))
    return [tasks[i : i + size] for i in range(0, len(tasks), size)]


def _outcome_exit_code(outcome):
    up = (outcome or "").upper()
    if up in ("PASS", "WARN", "OVERRIDDEN"):
        return 0
    if up == "BLOCK":
        return 1
    return 2


def _validate_task_paths(run_dir_real, tasks):
    validated = []
    for task in tasks:
        if not isinstance(task, dict):
            raise RuntimeError("Invalid non-object task entry in manifest")
        if not _is_safe_segment(task.get("agent")) or not _is_safe_segment(task.get("task_id")):
            raise RuntimeError(
                f"Unsafe task agent/task_id in manifest: {task.get('agent')!r} / {task.get('task_id')!r}"
            )
        prompt_path = _resolve_safe_run_file(run_dir_real, task.get("prompt_file"))
        out_path = _resolve_safe_run_file(run_dir_real, task.get("output_file"))
        err_path = _resolve_safe_run_file(run_dir_real, task.get("error_file"))
        validated.append((task, prompt_path, out_path, err_path))
    return validated


async def run_review_workflow(wf_args=None):
    cfg = wf_args if isinstance(wf_args, dict) else {}
    workspaces = globals().get("WORKSPACES") or []
    default_repo = workspaces[0] if workspaces else os.getcwd()

    explicit_repo = str(cfg.get("repo") or "").strip()
    existing_run_dir = str(cfg.get("run_dir") or "").strip()
    base = str(cfg.get("base") or "origin/main")
    head = str(cfg.get("head") or "HEAD")
    pr = str(cfg.get("pr") or "")
    publish = _parse_bool(cfg.get("publish", False))
    force = _parse_bool(cfg.get("force", False))
    agent_filter = str(cfg.get("agent") or "")
    engine_label = str(cfg.get("engine") or "")
    aspect_bin = cfg.get("aspect_bin") or None
    allow_repo_exec = _parse_bool(cfg.get("allow_repo_exec", False))
    max_concurrency = parse_max_concurrency(cfg.get("max_concurrency"))

    phase("Prepare review gates")

    if existing_run_dir:
        run_dir_real, manifest = _validate_run_dir(
            existing_run_dir,
            expected_repo=explicit_repo or None,
            require_manifest=True,
        )
        repo = os.path.abspath(
            explicit_repo or os.path.dirname(os.path.dirname(os.path.abspath(existing_run_dir)))
        )
        cmd_prefix, cmd_cwd = resolve_aspect_cmd(
            repo, aspect_bin=aspect_bin, allow_repo_exec=allow_repo_exec
        )
        exec_cwd = cmd_cwd or repo
        prep = {
            "run_id": manifest["run_id"],
            "run_dir": run_dir_real,
            "status": "pending",
            "engine": manifest.get("engine", "antigravity (flash)"),
            "pr": pr or manifest.get("pr", ""),
            "tasks": manifest.get("tasks") or [],
        }
    else:
        repo = os.path.abspath(explicit_repo or default_repo)
        cmd_prefix, cmd_cwd = resolve_aspect_cmd(
            repo, aspect_bin=aspect_bin, allow_repo_exec=allow_repo_exec
        )
        exec_cwd = cmd_cwd or repo
        prep_cmd = list(cmd_prefix) + [
            "review",
            "prepare",
            "-repo",
            repo,
            "-base",
            base,
            "-head",
            head,
            "-json",
        ]
        if pr:
            prep_cmd.extend(["-pr", pr])
        if publish:
            prep_cmd.append("-publish")
        if force:
            prep_cmd.append("-force")
        if agent_filter:
            prep_cmd.extend(["-agent", agent_filter])
        if engine_label:
            prep_cmd.extend(["-engine", engine_label])

        prep_proc = subprocess.run(prep_cmd, cwd=exec_cwd, capture_output=True, text=True)
        if prep_proc.returncode != 0:
            detail = (prep_proc.stderr or prep_proc.stdout or "").strip()
            raise RuntimeError(f"aspect review prepare failed (exit {prep_proc.returncode}): {detail}")
        prep = json.loads(prep_proc.stdout)
        status = prep.get("status", "")
        run_dir_real, _ = _validate_run_dir(
            prep["run_dir"],
            expected_repo=repo,
            require_manifest=(status == "pending"),
        )
        if prep.get("run_id") != os.path.basename(run_dir_real):
            raise RuntimeError(
                f"Prepared run_id {prep.get('run_id')!r} does not match {os.path.basename(run_dir_real)!r}"
            )

    run_id = prep["run_id"]
    run_dir = run_dir_real
    pr = str(prep.get("pr") or pr or "")
    status = prep.get("status", "")
    tasks = prep.get("tasks") or []

    if status in ("reused", "completed") and prep.get("result"):
        res = prep["result"]
        outcome = res.get("outcome", "PASS")
        exit_code = _outcome_exit_code(outcome)
        rendered = res.get("rendered", "")
        log(f"Review {status}: run {run_id} -> {outcome} (exit {exit_code})")
        print(f"OUTCOME: {outcome}\nEXIT_CODE: {exit_code}\nRUN_ID: {run_id}\n\n{rendered}")
        return {
            "run_id": run_id,
            "run_dir": run_dir,
            "status": status,
            "outcome": outcome,
            "exit_code": exit_code,
            "rendered": rendered,
        }

    validated_tasks = _validate_task_paths(run_dir_real, tasks)

    fin_proc = None
    try:
        if validated_tasks:
            phase(f"Dispatch {len(validated_tasks)} Flash reviewer task(s)")
            batches = chunk_tasks(validated_tasks, max_concurrency)

            async def run_one(entry):
                task, prompt_path, out_path, err_path = entry
                try:
                    _check_no_symlink_escape(run_dir_real, prompt_path)
                    with open(prompt_path, "r", encoding="utf-8") as f:
                        payload = json.load(f)
                    role = f"{task['agent']} ({task['mode']} mode)"
                    total_parts = int(task.get("total_parts") or 1)
                    if total_parts > 1:
                        role += f", part {task['part']}/{total_parts}"
                    ans = await agent(
                        prompt=payload["user_prompt"],
                        schema=payload["report_schema"],
                        role=role,
                        type_name="aspect_gate_reviewer",
                        workspace="inherit",
                        model="flash",
                    )
                    _check_no_symlink_escape(run_dir_real, out_path)
                    _check_no_symlink_escape(run_dir_real, err_path)
                    os.makedirs(os.path.dirname(out_path), exist_ok=True)
                    text = ans if isinstance(ans, str) else json.dumps(ans, indent=2)
                    with open(out_path, "w", encoding="utf-8") as f:
                        f.write(text + "\n")
                    if os.path.lexists(err_path):
                        os.unlink(err_path)
                except Exception as exc:
                    _check_no_symlink_escape(run_dir_real, err_path)
                    _check_no_symlink_escape(run_dir_real, out_path)
                    os.makedirs(os.path.dirname(err_path), exist_ok=True)
                    msg = str(exc).strip() or type(exc).__name__
                    with open(err_path, "w", encoding="utf-8") as f:
                        f.write(msg + "\n")
                    if os.path.lexists(out_path):
                        os.unlink(out_path)

            for batch_idx, batch in enumerate(batches, start=1):
                log(f"Running reviewer batch {batch_idx}/{len(batches)} ({len(batch)} task(s))")
                await parallel(batch, run_one)
    finally:
        phase("Finalize review gates")
        fin_cmd = list(cmd_prefix) + ["review", "finalize", run_dir, "-repo", repo]
        if pr:
            fin_cmd.extend(["-pr", pr])
        if publish:
            fin_cmd.append("-publish")
        fin_proc = subprocess.run(fin_cmd, cwd=exec_cwd, capture_output=True, text=True)

    detail = (fin_proc.stderr or fin_proc.stdout or "").strip()
    if fin_proc.returncode not in (0, 1, 2):
        raise RuntimeError(f"aspect review finalize failed (exit {fin_proc.returncode}): {detail}")
    if fin_proc.returncode in (1, 2) and "Aspect gate outcome:" not in (fin_proc.stdout or ""):
        raise RuntimeError(f"aspect review finalize failed (exit {fin_proc.returncode}): {detail}")

    status_path = _resolve_safe_run_file(run_dir_real, "status.json")
    outcome = ""
    if os.path.isfile(status_path):
        try:
            with open(status_path, "r", encoding="utf-8") as f:
                st_doc = json.load(f)
            if str(st_doc.get("state") or "") == "completed":
                outcome = str(st_doc.get("outcome") or "").upper()
        except (OSError, ValueError):
            outcome = ""

    if not outcome:
        if fin_proc.returncode == 0:
            outcome = "PASS"
        elif fin_proc.returncode == 1:
            outcome = "BLOCK"
        else:
            outcome = "ERROR"

    rendered_path = _resolve_safe_run_file(run_dir_real, "rendered.md")
    rendered = ""
    if os.path.isfile(rendered_path):
        with open(rendered_path, "r", encoding="utf-8") as f:
            rendered = f.read()
    elif fin_proc.stdout:
        rendered = fin_proc.stdout

    if fin_proc.stderr:
        log(fin_proc.stderr.strip())
    log(f"Review finalized: run {run_id} -> {outcome} (exit {fin_proc.returncode})")
    output_body = (fin_proc.stdout or rendered).strip()
    print(f"OUTCOME: {outcome}\nEXIT_CODE: {fin_proc.returncode}\nRUN_ID: {run_id}\n\n{output_body}")
    return {
        "run_id": run_id,
        "run_dir": run_dir,
        "status": "completed",
        "outcome": outcome,
        "exit_code": fin_proc.returncode,
        "rendered": rendered,
    }


RESULT = await run_review_workflow(args if "args" in globals() and isinstance(args, dict) else {})
