#!/usr/bin/env python3
"""Niten: pair-session state, reviews and hook gates.

The executor is the interactive Claude Code session that runs the skill. The
reviewer is Codex (`codex exec`, read-only sandbox), called by `review` and
`final`. State lives next to the plan in `<plan>.niten/`; active sessions are
registered in ~/.claude/niten/active.json, bound to the Claude Code session id,
so the hooks can find them.
"""

import argparse
import datetime as dt
import glob
import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys

HOME = os.path.expanduser("~")
REGISTRY = os.path.join(HOME, ".claude", "niten", "active.json")
CONFIG = os.path.join(HOME, ".claude", "niten", "config.json")
HERE = os.path.dirname(os.path.abspath(__file__))
SCHEMA = os.path.join(HERE, "verdict.schema.json")

DELIVERY = [
    re.compile(r"\bgit\b[^|;&]*\bpush\b"),
    re.compile(r"\bgh\s+pr\s+(create|merge)\b"),
    re.compile(r"\bbb\s+pr\b"),
    re.compile(r"pullrequests"),
]

# Commands with an effect outside the working tree, or one that cannot be undone. In a
# Niten session each one needs the user's approval with an explanation, whatever the
# permission settings would otherwise allow.
IMPORTANT = [
    ("git push or a history rewrite", r"\bgit\b[^|;&]*\b(push|reset\s+--hard|clean\s+-\w*f|branch\s+-D|tag\s+-d|filter-branch|filter-repo)\b"),
    ("a registry push, login or image removal", r"\bdocker\b[^|;&]*\b(push|login|rmi|system\s+prune|image\s+(rm|prune))\b|\bdocker\s+buildx\b[^|;&]*(--push\b|\bimagetools\s+create\b)"),
    ("an infrastructure change", r"\b(terraform|tofu|terragrunt)\b[^|;&]*\b(apply|destroy|import|taint|untaint|force-unlock|state\s+(rm|mv|push|replace-provider))\b"),
    ("a cluster change", r"\bkubectl\b[^|;&]*\b(apply|create|delete|edit|patch|replace|scale|annotate|label|set|rollout|cordon|uncordon|drain|taint|exec|cp|port-forward)\b"),
    ("a release change", r"\bhelm\b[^|;&]*\b(install|upgrade|uninstall|delete|rollback|push)\b"),
    ("a pull request, release or repository change", r"\bgh\s+(pr|release|repo)\s+(create|merge|close|delete|edit)\b|\bgh\s+api\b[^|;&]*(-X|--method)\s*(POST|PUT|PATCH|DELETE)\b"),
    ("a write to a web API", r"\b(curl|wget|http)\b[^|;&]*(-X\s*|--request\s+|--method\s+)(POST|PUT|PATCH|DELETE)\b|\bcurl\b[^|;&]*\s(-d|--data\S*|-F|--form|-T|--upload-file)\s"),
    ("a recursive forced delete", r"\brm\s+(-\w*r\w*f|-\w*f\w*r)\b|\brm\s+-r\s+-f\b|\brm\s+-f\s+-r\b"),
    ("a package publication", r"\b(npm|yarn|pnpm)\s+publish\b|\btwine\s+upload\b|\bgradlew?\b[^|;&]*\bpublish\w*\b"),
    ("a command on another machine", r"(^|[\s;&|(])(ssh|scp|rsync)\s"),
    ("a command as root", r"(^|[\s;&|(])sudo\s"),
]

# aws operations that only read; every other aws operation changes something.
AWS_READ = re.compile(r"^(describe|list|get|wait|help|ls)\b|^(describe-|list-|get-|batch-get-|search-|lookup-|validate-|filter-|test-)")
AWS_VALUED = {"--profile", "--region", "--output", "--endpoint-url", "--query", "--color", "--ca-bundle",
              "--cli-read-timeout", "--cli-connect-timeout", "--cli-binary-format"}
MIN_EXPLANATION = 25


def aws_writes(command):
    """Whether a command runs an aws operation that is not read-only."""
    try:
        words = shlex.split(command, comments=False, posix=True)
    except ValueError:
        words = command.split()
    for i, w in enumerate(words):
        if os.path.basename(w) != "aws":
            continue
        args, j = [], i + 1
        while j < len(words) and len(args) < 2 and words[j] not in ("|", "&&", "||", ";"):
            t = words[j]
            if t.startswith("-"):
                if "=" not in t and t in AWS_VALUED:
                    j += 1
            else:
                args.append(t)
            j += 1
        if len(args) == 2 and not AWS_READ.search(args[1]):
            return True
    return False


def important(command):
    """The reason a command needs the user's approval, or None."""
    extra = (load_json(CONFIG, {}) or {}).get("ask", [])
    for label, pattern in IMPORTANT + [("a command in your Niten ask list", p) for p in extra]:
        if re.search(pattern, command):
            return label
    if aws_writes(command):
        return "a cloud change"
    return None


def now():
    return dt.datetime.now().astimezone().isoformat(timespec="seconds")


def die(msg, code=2):
    print("niten: " + msg, file=sys.stderr)
    sys.exit(code)


def git(path, *args):
    r = subprocess.run(["git", "-C", path, *args], capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def load_json(path, default=None):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def save_json(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    os.replace(tmp, path)


# ---------------------------------------------------------------- registry


def registry():
    return load_json(REGISTRY, [])


def session_id():
    return os.environ.get("CLAUDE_CODE_SESSION_ID", "")


def register(cwd, state_dir):
    reg = [e for e in registry() if e["state_dir"] != state_dir]
    reg.append({"cwd": cwd, "state_dir": state_dir, "session_id": session_id(), "since": now()})
    save_json(REGISTRY, reg)


def unregister(state_dir):
    save_json(REGISTRY, [e for e in registry() if e["state_dir"] != state_dir])


def within(path, root):
    path, root = os.path.realpath(path), os.path.realpath(root)
    return path == root or path.startswith(root.rstrip(os.sep) + os.sep)


def find_active(cwd, sid=None):
    """The active niten session of a Claude session: by its session id when both
    sides have one, otherwise the session whose cwd or repositories contain cwd."""
    sid = session_id() if sid is None else sid
    if sid:
        for e in registry():
            if e.get("session_id") == sid:
                return e["state_dir"]
    best, best_len = None, -1
    for e in registry():
        if sid and e.get("session_id"):
            continue
        st = load_json(os.path.join(e["state_dir"], "state.json"))
        if not st:
            continue
        roots = [e["cwd"]] + [r["path"] for r in st["repos"].values()]
        for root in roots:
            if within(cwd, root) and len(root) > best_len:
                best, best_len = e["state_dir"], len(root)
    return best


def state_dir_arg(args):
    if getattr(args, "state", None):
        return os.path.abspath(args.state)
    d = find_active(os.getcwd())
    if not d:
        die("no active niten session for " + os.getcwd() + "; pass --state, attach, or run start")
    return d


def load_state(d):
    st = load_json(os.path.join(d, "state.json"))
    if not st:
        die("no state in " + d)
    return st


def save_state(d, st):
    save_json(os.path.join(d, "state.json"), st)


# ---------------------------------------------------------------- plan


STEP_RE = re.compile(r"^### (S-\d+)\s*[—-]\s*(.+?)\s*$", re.M)
ALIAS_RE = re.compile(r"^- (repo-\d+): `([^`]+)`", re.M)


def plan_steps(text):
    steps = []
    ms = list(STEP_RE.finditer(text))
    for i, m in enumerate(ms):
        end = ms[i + 1].start() if i + 1 < len(ms) else len(text)
        nxt = re.search(r"^## ", text[m.end():end], re.M)
        if nxt:
            end = m.end() + nxt.start()
        steps.append({"id": m.group(1), "title": m.group(2), "text": text[m.start():end].strip()})
    return steps


def plan_repos(text):
    """Repository names from the frontmatter, with their repo-N aliases."""
    names = []
    fm = re.match(r"^---\n(.*?)\n---\n", text, re.S)
    if fm:
        block = re.search(r"^repos:\n((?:\s+- .+\n)+)", fm.group(1) + "\n", re.M)
        if block:
            names = [l.strip()[2:].strip() for l in block.group(1).splitlines() if l.strip()]
    aliases = {m.group(2): m.group(1) for m in ALIAS_RE.finditer(text)}
    return names, aliases


def step_of(st, sid):
    for s in st["steps"]:
        if s["id"] == sid:
            return s
    die("unknown step " + sid + "; steps: " + ", ".join(s["id"] for s in st["steps"]))


def current_step(st):
    for s in st["steps"]:
        if s["status"] != "approved":
            return s
    return None


# ---------------------------------------------------------------- codex


def codex_bin():
    if os.environ.get("NITEN_CODEX"):
        return os.environ["NITEN_CODEX"]
    cfg = os.path.join(HOME, ".config", "shogun", "config.toml")
    try:
        m = re.search(r'^codex_command\s*=\s*"([^"]+)"', open(cfg).read(), re.M)
        if m and os.access(m.group(1), os.X_OK):
            return m.group(1)
    except OSError:
        pass
    p = shutil.which("codex")
    if p:
        return p
    found = sorted(glob.glob(os.path.join(HOME, ".vscode/extensions/openai.chatgpt-*/bin/macos-aarch64/codex")))
    if found:
        return found[-1]
    die("codex not found; set NITEN_CODEX")


def run_codex(prompt, root, out_path):
    model = os.environ.get("NITEN_REVIEW_MODEL", "gpt-6-astra")
    effort = os.environ.get("NITEN_REVIEW_EFFORT", "high")
    timeout = int(os.environ.get("NITEN_REVIEW_TIMEOUT", "1800"))
    argv = [codex_bin(), "exec", "--output-schema", SCHEMA, "-o", out_path,
            "-m", model, "-c", "model_reasoning_effort=" + effort, "-c", 'approval_policy="never"',
            "-s", "read-only", "--ephemeral", "--skip-git-repo-check", "-C", root,
            "-c", 'web_search="disabled"', "-"]
    print(f"niten: reviewer {model}/{effort} working (timeout {timeout // 60} min)...", file=sys.stderr, flush=True)
    try:
        r = subprocess.run(argv, input=prompt, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        die("reviewer timed out; the step stays unapproved")
    if r.returncode != 0:
        tail = "\n".join((r.stderr or r.stdout).strip().splitlines()[-15:])
        die(f"reviewer failed (exit {r.returncode}):\n{tail}")
    verdict = load_json(out_path)
    if not isinstance(verdict, dict) or verdict.get("verdict") not in ("approve", "request_changes"):
        die("reviewer returned no valid verdict in " + out_path)
    return verdict, model, effort


def approved(v):
    bad = [f for f in v["findings"] if f["severity"] in ("blocker", "major")]
    unmet = [c for c in v["criteria"] if c["status"] == "not_met"]
    return v["verdict"] == "approve" and not bad and not unmet


def repos_block(st):
    lines = []
    for name, r in st["repos"].items():
        head = git(r["path"], "rev-parse", "HEAD")
        branch = git(r["path"], "rev-parse", "--abbrev-ref", "HEAD")
        stat = git(r["path"], "diff", "--stat", r["base"]) or "(no changes against base)"
        status = git(r["path"], "status", "--porcelain") or "(clean)"
        alias = f" ({r['alias']})" if r.get("alias") else ""
        lines.append(
            f"- {name}{alias}: path {r['path']}\n"
            f"  base {r['base']} (branch at start: {r['branch']}), now {head} on {branch}\n"
            f"  already changed or untracked before the session (not part of the change):\n"
            + "".join(f"    {l}\n" for l in (r["preexisting"] or "(none)").splitlines())
            + f"  diff --stat against base:\n" + "".join(f"    {l}\n" for l in stat.splitlines())
            + f"  git status now:\n" + "".join(f"    {l}\n" for l in status.splitlines())
        )
    return "\n".join(lines)


def prior_findings(reviews):
    if not reviews:
        return "none"
    last = load_json(reviews[-1]["file"], {})
    fs = last.get("findings", [])
    if not fs:
        return "the previous review had no findings"
    return "\n".join(f"- [{f['severity']}] {f['location']}: {f['problem']} (asked fix: {f['fix']})" for f in fs)


REVIEW_RULES = """\
You are the independent reviewer in a pair session; another model (the executor) did
the work. You may read any file and run read-only commands (git log/diff/show/status,
grep, cat, ls). You cannot write files or use the network, so for external operations
(registries, cloud APIs) judge the executor's recorded commands and outputs in the
evidence file, and say in the criterion's evidence when you could not verify yourself.

Check:
1. Every acceptance criterion and verification of the step is met and proven by the
   diff, the files or the recorded evidence. A claim without proof is not met.
2. Nothing outside the step's scope changed: no side refactors, no unrelated files, no
   edits to the plan. Changes listed as already present before the session do not count.
3. No secrets or credentials in the diff or in the evidence.
4. Findings of the previous review, if any, are resolved.

Approve only if all of this holds. Otherwise request changes. Each finding names the
file or command, the problem and the exact fix. Severity: blocker (wrong or unsafe),
major (a criterion not met or not proven), minor (does not block). Return the JSON
object the schema requires; list every criterion id of the step in `criteria`.
"""


def review_step(d, st, sid, final=False):
    plan_path = st["plan"]
    text = open(plan_path).read()
    if not final:
        step = step_of(st, sid)
        ev = os.path.join(d, "evidence", sid + ".md")
        if not os.path.exists(ev) or os.path.getsize(ev) == 0:
            die(f"write the evidence first: {ev} (the commands you ran, their exit codes and the relevant output)")
        section = next(s["text"] for s in plan_steps(text) if s["id"] == sid)
        reviews = step["reviews"]
        what = f"step {sid} ({step['title']})"
        scope = f"The step, as the plan states it:\n\n{section}\n"
        evidence = f"Evidence file of this step: {ev}"
    else:
        step = st["final"]
        reviews = step["reviews"]
        what = "the whole change (final review)"
        scope = ("Review the complete change against the plan's goal, every requirement and its "
                 "acceptance criteria, and the plan's end-to-end verification. Every step was "
                 "approved separately; look for gaps between steps, missing criteria and "
                 "anything outside the plan's scope.\n")
        evs = sorted(glob.glob(os.path.join(d, "evidence", "*.md")))
        evidence = "Evidence files: " + ", ".join(evs) if evs else "Evidence files: none"
    n = len(reviews) + 1
    prompt = (
        f"Review {what} of the plan at {plan_path}. Read the plan file for the requirements, "
        f"criteria and context.\n\n{scope}\nRepositories:\n{repos_block(st)}\n{evidence}\n\n"
        f"Findings of the previous review of this {('change' if final else 'step')}:\n{prior_findings(reviews)}\n\n"
        + REVIEW_RULES
    )
    name = ("final" if final else sid) + f"-r{n}"
    os.makedirs(os.path.join(d, "reviews"), exist_ok=True)
    with open(os.path.join(d, "reviews", name + ".prompt.md"), "w") as f:
        f.write(prompt)
    out = os.path.join(d, "reviews", name + ".json")
    root = os.path.commonpath([r["path"] for r in st["repos"].values()] + [os.path.dirname(plan_path)])
    verdict, model, effort = run_codex(prompt, root, out)
    ok = approved(verdict)
    reviews.append({"n": n, "file": out, "verdict": verdict["verdict"], "approved": ok, "at": now(),
                    "model": model, "effort": effort})
    step["status"] = "approved" if ok else "changes_requested"
    save_state(d, st)
    print(f"{what}: review {n} -> {'APPROVED' if ok else 'CHANGES REQUESTED'}")
    print("summary: " + verdict["summary"])
    for c in verdict["criteria"]:
        print(f"  criterion {c['id']}: {c['status']} — {c['evidence']}")
    for f in verdict["findings"]:
        print(f"  [{f['severity']}] {f['location']}: {f['problem']}\n      fix: {f['fix']}")
    if verdict["verdict"] == "approve" and not ok:
        print("  (the reviewer said approve, but a blocker/major finding or an unmet criterion keeps it open)")
    print("review file: " + out)


# ---------------------------------------------------------------- commands


def cmd_start(args):
    plan = os.path.realpath(args.plan)
    text = open(plan).read()
    steps = plan_steps(text)
    if not steps:
        die("no steps (### S-NNN — title) in " + plan)
    names, aliases = plan_repos(text)
    paths = {}
    for spec in args.repo or []:
        k, _, v = spec.partition("=")
        paths[k] = os.path.realpath(os.path.expanduser(v))
    manifest = load_json(os.path.splitext(plan)[0] + ".manifest.json", {}) or {}
    roots = {r.get("id"): r.get("root") for r in manifest.get("repos", []) if r.get("root")}
    cwd = os.path.realpath(args.cwd or os.getcwd())
    repos = {}
    for name in names:
        alias = aliases.get(name, "")
        candidates = [paths.get(name), paths.get(alias), roots.get(alias), roots.get(name)]
        if os.environ.get("NITEN_WORKSPACE"):
            candidates.append(os.path.join(os.path.expanduser(os.environ["NITEN_WORKSPACE"]), name))
        candidates += [os.path.join(cwd, name), cwd if os.path.basename(cwd) == name else None]
        p = next((c for c in candidates if c and os.path.isdir(c) and git(c, "rev-parse", "--git-dir")), None)
        if not p:
            die(f"repository {name}: not found; pass --repo {name}=PATH (or set NITEN_WORKSPACE)")
        repos[name] = {"path": os.path.realpath(p), "alias": aliases.get(name, ""),
                       "base": git(p, "rev-parse", "HEAD"), "branch": git(p, "rev-parse", "--abbrev-ref", "HEAD"),
                       "preexisting": git(p, "status", "--porcelain")}
    d = os.path.join(os.path.dirname(plan), os.path.splitext(os.path.basename(plan))[0] + ".niten")
    if os.path.exists(os.path.join(d, "state.json")) and not args.restart:
        die(f"a session already exists in {d}; continue it, or pass --restart to begin again")
    st = {"plan": plan, "plan_sha256": hashlib.sha256(text.encode()).hexdigest(), "started": now(),
          "cwd": cwd, "repos": repos, "paused": None,
          "steps": [{"id": s["id"], "title": s["title"], "status": "pending", "reviews": []} for s in steps],
          "final": {"status": "pending", "reviews": []}}
    os.makedirs(os.path.join(d, "evidence"), exist_ok=True)
    save_state(d, st)
    register(st["cwd"], d)
    print("niten session started: " + d)
    for name, r in repos.items():
        print(f"  {name}: {r['path']} at {r['base'][:12]} on {r['branch']}" + (" (has pre-existing changes)" if r["preexisting"] else ""))
    for s in st["steps"]:
        print(f"  {s['id']}: {s['title']}")
    print("evidence goes to " + os.path.join(d, "evidence", "<step>.md"))


def cmd_status(args):
    d = state_dir_arg(args)
    st = load_state(d)
    print("session: " + d)
    print("plan: " + st["plan"])
    with open(st["plan"]) as f:
        if hashlib.sha256(f.read().encode()).hexdigest() != st["plan_sha256"]:
            print("WARNING: the plan file changed since the session started")
    if st["paused"]:
        print(f"PAUSED since {st['paused']['since']}: {st['paused']['reason']}")
    for s in st["steps"]:
        print(f"  {s['id']} [{s['status']}] {s['title']} ({len(s['reviews'])} review(s))")
    print(f"  final [{st['final']['status']}] ({len(st['final']['reviews'])} review(s))")
    cur = current_step(st)
    print("next: " + (f"{cur['id']}" if cur else ("final review" if st["final"]["status"] != "approved" else "delivery")))


def cmd_review(args):
    d = state_dir_arg(args)
    st = load_state(d)
    cur = current_step(st)
    if cur and cur["id"] != args.step:
        die(f"{cur['id']} is the current step; steps are reviewed in order")
    review_step(d, st, args.step)


def cmd_final(args):
    d = state_dir_arg(args)
    st = load_state(d)
    cur = current_step(st)
    if cur:
        die(f"{cur['id']} is not approved yet; the final review comes after every step")
    review_step(d, st, None, final=True)


def cmd_pause(args):
    d = state_dir_arg(args)
    st = load_state(d)
    st["paused"] = {"reason": args.reason, "since": now()}
    save_state(d, st)
    print("paused: " + args.reason)


def cmd_resume(args):
    d = state_dir_arg(args)
    st = load_state(d)
    st["paused"] = None
    save_state(d, st)
    print("resumed")
    cmd_status(args)


def cmd_attach(args):
    d = os.path.abspath(args.state)
    load_state(d)
    if not session_id():
        die("no CLAUDE_CODE_SESSION_ID: attach from inside the Claude Code session")
    reg = registry()
    for e in reg:
        if e["state_dir"] == d:
            e["session_id"] = session_id()
            break
    else:
        reg.append({"cwd": os.path.realpath(os.getcwd()), "state_dir": d, "session_id": session_id(), "since": now()})
    save_json(REGISTRY, reg)
    print("attached to this Claude session: " + d)
    cmd_status(argparse.Namespace(state=d))


def cmd_finish(args):
    d = state_dir_arg(args)
    st = load_state(d)
    if st["final"]["status"] != "approved" and not args.abort:
        die("the final review has not approved the change; pass --abort to end the session anyway")
    st["finished"] = {"at": now(), "aborted": bool(args.abort)}
    save_state(d, st)
    unregister(d)
    print("session " + ("aborted" if args.abort else "finished") + ": " + d)


# ---------------------------------------------------------------- hooks


def hook_input():
    """The hook's event, or {} when it does not name a Claude Code session: a hook
    decides only for the session that triggered it."""
    try:
        data = json.load(sys.stdin)
    except ValueError:
        return {}
    return data if isinstance(data, dict) and data.get("session_id") else {}


def cmd_hook_stop(args):
    data = hook_input()
    if not data:
        return
    d = find_active(data.get("cwd") or os.getcwd(), data.get("session_id", ""))
    if not d:
        return
    st = load_json(os.path.join(d, "state.json"))
    if not st or st.get("paused"):
        return
    cur = current_step(st)
    script = os.path.join(HERE, "niten.py")
    if cur:
        reason = (f"Niten session ({d}): step {cur['id']} is {cur['status']}, not approved by the reviewer. "
                  f"Finish the step, write its evidence and run `python3 {script} review {cur['id']}`, "
                  f"then fix any findings. If you need the user (access, a decision, missing information), "
                  f"run `python3 {script} pause \"<what you need>\"` and ask.")
    elif st["final"]["status"] != "approved":
        reason = (f"Niten session ({d}): every step is approved; run the final review "
                  f"`python3 {script} final` before you stop.")
    else:
        return
    print(json.dumps({"decision": "block", "reason": reason}))


def decision(kind, reason):
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": kind,
        "permissionDecisionReason": reason,
    }}))


def cmd_hook_pretooluse(args):
    """In a Niten session: delivery waits for the final review, and every important
    command is put to the user with the executor's explanation of why the step needs
    it, even where the permission settings would run it without asking."""
    data = hook_input()
    if not data or data.get("tool_name") != "Bash":
        return
    tool_input = data.get("tool_input") or {}
    command = tool_input.get("command", "")
    delivery = any(p.search(command) for p in DELIVERY)
    label = important(command)
    if not delivery and not label:
        return
    d = find_active(data.get("cwd") or os.getcwd(), data.get("session_id", ""))
    if not d:
        return
    st = load_json(os.path.join(d, "state.json"))
    if not st:
        return
    script = os.path.join(HERE, "niten.py")
    if delivery and st["final"]["status"] != "approved":
        decision("deny", f"Niten session ({d}): delivery (push or pull request) waits for the final review. "
                         f"Approve every step, then run `python3 {script} final`.")
        return
    label = label or "delivery"
    explanation = " ".join((tool_input.get("description") or "").split())
    if len(explanation) < MIN_EXPLANATION:
        decision("deny", f"Niten: this is {label}, so the user approves it. Run the same command again with a "
                         f"Bash `description` that tells the user, in one or two sentences, what it does and why "
                         f"the current plan step needs it.")
        return
    cur = current_step(st)
    where = f"{cur['id']} ({cur['title']})" if cur else "final stage"
    decision("ask", f"Niten {where}, {label}: {explanation}")


def main():
    ap = argparse.ArgumentParser(prog="niten.py")
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("start")
    p.add_argument("--plan", required=True)
    p.add_argument("--repo", action="append", help="NAME=PATH or repo-N=PATH")
    p.add_argument("--cwd")
    p.add_argument("--restart", action="store_true")
    p.set_defaults(fn=cmd_start)
    for name, fn in [("status", cmd_status), ("final", cmd_final), ("resume", cmd_resume)]:
        p = sub.add_parser(name)
        p.add_argument("--state")
        p.set_defaults(fn=fn)
    p = sub.add_parser("review")
    p.add_argument("step")
    p.add_argument("--state")
    p.set_defaults(fn=cmd_review)
    p = sub.add_parser("pause")
    p.add_argument("reason")
    p.add_argument("--state")
    p.set_defaults(fn=cmd_pause)
    p = sub.add_parser("finish")
    p.add_argument("--abort", action="store_true")
    p.add_argument("--state")
    p.set_defaults(fn=cmd_finish)
    p = sub.add_parser("attach")
    p.add_argument("--state", required=True)
    p.set_defaults(fn=cmd_attach)
    sub.add_parser("hook-stop").set_defaults(fn=cmd_hook_stop)
    sub.add_parser("hook-pretooluse").set_defaults(fn=cmd_hook_pretooluse)
    args = ap.parse_args()
    if args.cmd.startswith("hook-"):
        # A hook never fails the session: any error here means no decision.
        try:
            args.fn(args)
        except Exception as e:  # noqa: BLE001
            print("niten hook error: " + str(e), file=sys.stderr)
        sys.exit(0)
    args.fn(args)


if __name__ == "__main__":
    main()
