#!/usr/bin/env python3
"""Niten: pair-session state, reviews and hook gates.

The executor is the interactive Claude Code session that runs the skill. The
reviewer is Codex (`codex exec`, read-only sandbox), called by `review` and
`final`. State lives next to the plan in `<plan>.niten/`; active sessions are
registered in ~/.claude/niten/active.json, bound to the Claude Code session id,
so the hooks can find them.

Hooks (registered by install.sh), all inert outside an active session:
  Stop              blocks ending a turn while the current step or the final
                    review is unapproved, unless the session waits for the user
  PreToolUse        delivery only after the final review of the exact commits;
                    important actions go to the user with the executor's reason;
                    the session state, the plan and Claude's settings are not
                    edited by hand
  PostToolUse(Failure)  records every Bash command with its exit code and output,
                    and the user's answers to the executor's questions
  UserPromptSubmit  records the user's messages and ends a pause when they answer
"""

import argparse
import datetime as dt
import glob
import hashlib
import hmac
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import time

HOME = os.path.expanduser("~")
NITEN_HOME = os.path.join(HOME, ".claude", "niten")
REGISTRY = os.path.join(NITEN_HOME, "active.json")
CONFIG = os.path.join(NITEN_HOME, "config.json")
KEY = os.path.join(NITEN_HOME, "hook.key")  # signs the log; the executor may not read it
HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "niten.py")
SCHEMA = os.path.join(HERE, "verdict.schema.json")

MAX_REVIEWS = 3          # unapproved reviews of one step before the user must decide
MAX_STOP_BLOCKS = 3      # Stop blocks in a row without progress before the turn may end
MIN_EXPLANATION = 25     # characters of the executor's reason for an important action
LOG_TAIL = 4000          # characters of stdout/stderr kept per logged command

# ---------------------------------------------------------------- classification

DELIVERY = [
    re.compile(r"\bgit\b[^|;&]*\bpush\b"),
    re.compile(r"\bgh\b[^|;&]*\bpr\s+(create|merge)\b"),
    re.compile(r"\bbb\b[^|;&]*\bpr\b"),
    re.compile(r"\b(curl|wget|http|https)\b[^|;&]*pullrequests"),
]

# Actions with an effect outside the working tree, or one that cannot be undone. In a
# Niten session each one needs the user's approval with an explanation, whatever the
# permission settings would otherwise allow.
IMPORTANT = [
    ("git push or a history rewrite", r"\bgit\b[^|;&]*\b(push|reset\s+--hard|clean\s+-\w*f|branch\s+-D|tag\s+-d|filter-branch|filter-repo)\b"),
    ("a registry push, login or image removal", r"\bdocker\b[^|;&]*\b(push|login|rmi|system\s+prune|image\s+(rm|prune))\b|\bdocker\s+buildx\b[^|;&]*(--push\b|\bimagetools\s+create\b)"),
    ("an infrastructure change", r"\b(terraform|tofu|terragrunt)\b[^|;&]*\b(apply|destroy|import|taint|untaint|force-unlock|state\s+(rm|mv|push|replace-provider))\b"),
    ("a cluster change", r"\bkubectl\b[^|;&]*\b(apply|create|delete|edit|patch|replace|scale|annotate|label|set|rollout|cordon|uncordon|drain|taint|exec|cp|port-forward)\b"),
    ("a release change", r"\bhelm\b[^|;&]*\b(install|upgrade|uninstall|delete|rollback|push)\b"),
    ("a pull request, release or repository change", r"\bgh\b[^|;&]*\b(pr|release|repo|issue|secret|variable|workflow)\s+(create|merge|close|delete|edit|set|run|reopen|comment|transfer|archive)\b"),
    ("a write through the GitHub API", r"\bgh\b[^|;&]*\bapi\b[^|;&]*(\s(-f|-F|--field|--raw-field|--input)(\s|=)|(-X|--method)(\s*|=)(POST|PUT|PATCH|DELETE)\b)"),
    ("a write to a web API", r"\b(curl|wget|http|https|xh)\b[^|;&]*(-X\s*|--request(\s+|=)|--method(\s+|=))(POST|PUT|PATCH|DELETE)\b"
                             r"|\bcurl\b[^|;&]*\s(-d|--data\S*|-F|--form\S*|-T|--upload-file|--json)(\s|=)"
                             r"|\bwget\b[^|;&]*--(post|put)-(data|file)\b|\b(http|https|xh)\s+(POST|PUT|PATCH|DELETE)\b"),
    ("a recursive forced delete", r"\brm\s+(-\w*r\w*f|-\w*f\w*r)\b|\brm\s+-r\s+-f\b|\brm\s+-f\s+-r\b"),
    ("a package publication", r"\b(npm|yarn|pnpm)\s+publish\b|\btwine\s+upload\b|\bgradlew?\b[^|;&]*\bpublish\w*\b"),
    ("a command on another machine", r"(^|[\s;&|(])(ssh|scp|rsync)\s"),
    ("a command as root", r"(^|[\s;&|(])sudo\s"),
    ("a Niten override", r"\bniten\.py\b[^|;&]*(--abort|--restart|--unapproved|--user-approved|\bconfirm\b)"),
]

# aws operations that only read; every other aws operation changes something.
AWS_READ = re.compile(r"^(describe|list|get|wait|help|ls)\b|^(describe-|list-|get-|batch-get-|search-|lookup-|validate-|filter-|test-)")
AWS_VALUED = {"--profile", "--region", "--output", "--endpoint-url", "--query", "--color", "--ca-bundle",
              "--cli-read-timeout", "--cli-connect-timeout", "--cli-binary-format"}

# MCP tools whose names say they change something in another system.
MCP_WRITE = re.compile(r"(create|update|delete|remove|send|post|add|edit|move|transition|merge|close|comment|"
                       r"publish|upload|write|assign|schedule|archive|invite|share|rename|set_|_set|replace)", re.I)
FILE_TOOLS = {"Write": "file_path", "Edit": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}

# Values of keys that name a secret (SecretAccessKey, SessionToken, AWS_SECRET_ACCESS_KEY,
# client_secret, password, ...) and well-known token shapes. The key is kept, the value
# masked.
SECRET_KEY = (r"[A-Za-z0-9_.-]*(secret|passw(or)?d|token|credential|api[_-]?key|access[_-]?key|private[_-]?key|"
              r"session[_-]?key|authorization|cookie)[A-Za-z0-9_.-]*")
SECRETS = [
    (re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}"), "Bearer [REDACTED]"),
    (re.compile(r"\b(AKIA|ASIA)[A-Z0-9]{16}\b"), "[REDACTED]"),
    (re.compile(r"(?i)(\"?\b" + SECRET_KEY + r"\"?\s*[:=]\s*)(\"[^\"]*\"|'[^']*'|[^\s,{}\[\]\"']+)"), r"\1[REDACTED]"),
    (re.compile(r"(?i)(--?" + SECRET_KEY + r"(\s+|=))(\"[^\"]*\"|'[^']*'|\S+)"), r"\1[REDACTED]"),
    (re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b"), "[REDACTED]"),
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S), "[REDACTED]"),
    (re.compile(r"[A-Za-z0-9+/=_-]{120,}"), "[REDACTED]"),
]


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


def redact(text):
    for pattern, replacement in SECRETS:
        text = pattern.sub(replacement, text)
    return text


# ---------------------------------------------------------------- the signed log


def hook_key():
    """The key that signs log entries, created on first use, readable only by the
    user; the PreToolUse hook keeps the executor from reading it."""
    try:
        with open(KEY, "rb") as f:
            return f.read()
    except OSError:
        pass
    os.makedirs(NITEN_HOME, exist_ok=True)
    try:
        fd = os.open(KEY, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as f:
            f.write(secrets.token_hex(32).encode())
    except FileExistsError:
        pass
    with open(KEY, "rb") as f:
        return f.read()


def signature(entry):
    body = json.dumps({k: v for k, v in entry.items() if k != "sig"}, sort_keys=True, ensure_ascii=False)
    return hmac.new(hook_key(), body.encode(), hashlib.sha256).hexdigest()


def forged_entries(d):
    """Line numbers of log entries the hooks did not write."""
    bad = []
    try:
        with open(os.path.join(d, "commands.jsonl")) as f:
            for n, line in enumerate(f, 1):
                try:
                    entry = json.loads(line)
                except ValueError:
                    bad.append(n)
                    continue
                if not isinstance(entry, dict) or not hmac.compare_digest(str(entry.get("sig", "")), signature(entry)):
                    bad.append(n)
    except OSError:
        pass
    return bad


# ---------------------------------------------------------------- basics


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


def sha256_file(path):
    try:
        with open(path, "rb") as f:
            return hashlib.sha256(f.read()).hexdigest()
    except OSError:
        return ""


def within(path, root):
    path, root = os.path.realpath(path), os.path.realpath(root)
    return path == root or path.startswith(root.rstrip(os.sep) + os.sep)


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


def find_active(cwd, sid=None, by_cwd=True):
    """The active niten session of a Claude session: by its session id when both
    sides have one, otherwise (commands typed by hand, never hooks) the session
    whose cwd or repositories contain cwd."""
    sid = session_id() if sid is None else sid
    if sid:
        for e in registry():
            if e.get("session_id") == sid:
                return e["state_dir"]
    if not by_cwd:
        return None
    best, best_len = None, -1
    for e in registry():
        if sid and e.get("session_id"):
            continue
        st = load_json(os.path.join(e["state_dir"], "state.json"))
        if not st:
            continue
        for root in [e["cwd"]] + [r["path"] for r in st["repos"].values()]:
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


# ---------------------------------------------------------------- plan and repositories


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


def sidecars(plan):
    stem = os.path.splitext(plan)[0]
    return [plan, stem + ".approval.json", stem + ".manifest.json"]


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


def st_dir(st):
    return os.path.splitext(st["plan"])[0] + ".niten"


def heads(st):
    return {name: git(r["path"], "rev-parse", "HEAD") for name, r in st["repos"].items()}


def status_paths(path):
    """Paths with uncommitted changes (tracked or untracked), from `git status -z`."""
    r = subprocess.run(["git", "-C", path, "status", "--porcelain", "-z", "--untracked-files=all"],
                       capture_output=True)
    fields, out, i = r.stdout.decode("utf-8", "surrogateescape").split("\0"), [], 0
    while i < len(fields):
        entry = fields[i]
        if len(entry) > 3:
            out.append(entry[3:])
            if entry[0] in "RC":
                i += 1  # the original path of a rename or copy follows
        i += 1
    return out


def content_hash(path):
    try:
        with open(path, "rb") as f:
            return hashlib.sha256(f.read()).hexdigest()
    except IsADirectoryError:
        return "directory"
    except OSError:
        return "absent"


def own_paths(st, r):
    """The session's own files inside a repository (a plan kept there)."""
    return [os.path.relpath(p, r["path"]) for p in sidecars(st["plan"]) + [st_dir(st)] if within(p, r["path"])]


def preexisting_state(path):
    """Content of every path that had uncommitted changes when the session started."""
    return {p: content_hash(os.path.join(path, p)) for p in status_paths(path)}


def new_changes(st, r):
    """Uncommitted changes since the session started: a path changed now that was
    not before, or a path changed before whose content is no longer what it was."""
    own = own_paths(st, r)
    before = r.get("preexisting_content", {})
    now_ = [p for p in status_paths(r["path"]) if not any(p.startswith(o) for o in own)]
    out = [p for p in now_ if p not in before]
    out += [p for p, h in before.items() if content_hash(os.path.join(r["path"], p)) != h]
    return sorted(set(out))


def tree_fingerprint(st, r):
    """The content of a repository's working tree beyond its HEAD: tracked changes
    (staged or not) and every untracked file, the session's own files left out."""
    excludes = [":(exclude)" + o for o in own_paths(st, r)]
    diff = subprocess.run(["git", "-C", r["path"], "diff", "HEAD", "--binary", "--", "."] + excludes,
                          capture_output=True).stdout
    h = hashlib.sha256(diff)
    tracked = set(subprocess.run(["git", "-C", r["path"], "diff", "HEAD", "--name-only", "-z"],
                                 capture_output=True).stdout.decode("utf-8", "surrogateescape").split("\0"))
    for p in sorted(status_paths(r["path"])):
        if p not in tracked and not any(p.startswith(o) for o in own_paths(st, r)):
            h.update(p.encode("utf-8", "surrogateescape") + b"\0" + content_hash(os.path.join(r["path"], p)).encode())
    return h.hexdigest()


def trees(st):
    return {name: tree_fingerprint(st, r) for name, r in st["repos"].items()}


def moved_since(st, approval):
    """Repositories whose commits or working tree differ from an approval."""
    now_heads, now_trees = heads(st), trees(st)
    return sorted({n for n in st["repos"] if now_heads[n] != approval.get("heads", {}).get(n)
                   or now_trees[n] != approval.get("trees", {}).get(n)})


# ---------------------------------------------------------------- codex


def codex_bin():
    if os.environ.get("NITEN_CODEX"):
        return os.environ["NITEN_CODEX"]
    cfg = os.path.join(HOME, ".config", "shogun", "config.toml")
    try:
        m = re.search(r'^codex_command\s*=\s*"([^"]+)"', open(cfg).read(), re.M)
        if m and os.access(os.path.expanduser(m.group(1)), os.X_OK):
            return os.path.expanduser(m.group(1))
    except OSError:
        pass
    p = shutil.which("codex")
    if p:
        return p
    found = sorted(glob.glob(os.path.join(HOME, ".vscode/extensions/openai.chatgpt-*/bin/macos-aarch64/codex")))
    if found:
        return found[-1]
    die("codex not found; set NITEN_CODEX")


def valid_verdict(v):
    def strs(d, keys):
        return isinstance(d, dict) and all(isinstance(d.get(k), str) for k in keys)
    return (isinstance(v, dict) and v.get("verdict") in ("approve", "request_changes")
            and isinstance(v.get("summary"), str)
            and isinstance(v.get("criteria"), list) and isinstance(v.get("findings"), list)
            and all(strs(c, ("id", "evidence")) and c.get("status") in ("met", "not_met", "cannot_verify")
                    for c in v["criteria"])
            and all(strs(f, ("location", "problem", "fix")) and f.get("severity") in ("blocker", "major", "minor")
                    for f in v["findings"])
            and isinstance(v.get("previous_findings"), list)
            and all(strs(p, ("id", "note")) and p.get("status") in ("addressed", "not_addressed", "withdrawn")
                    for p in v["previous_findings"])
            and isinstance(v.get("declined"), list) and all(isinstance(x, str) for x in v["declined"]))


def run_codex(prompt, root, out_path):
    """One reviewer call: read-only sandbox, no user config, rules, MCP servers,
    plugins or sub-agents (MCP tools are not bound by the sandbox), schema-checked
    output written to a file of this call only."""
    model = os.environ.get("NITEN_REVIEW_MODEL", "gpt-6-astra")
    effort = os.environ.get("NITEN_REVIEW_EFFORT", "high")
    timeout = int(os.environ.get("NITEN_REVIEW_TIMEOUT", "1800"))
    argv = [codex_bin(), "exec", "--json", "--output-schema", SCHEMA, "-o", out_path,
            "-m", model, "-c", "model_reasoning_effort=" + effort, "-c", 'approval_policy="never"',
            "-s", "read-only", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
            "-C", root, "-c", "mcp_servers={}", "-c", "plugins={}", "-c", "agents.enabled=false",
            "-c", 'web_search="disabled"', "-"]
    print(f"niten: reviewer {model}/{effort} working (timeout {timeout // 60} min)...", file=sys.stderr, flush=True)
    if os.path.exists(out_path):
        os.remove(out_path)
    started = time.time()

    def fail(msg):
        if os.path.exists(out_path):
            os.remove(out_path)
        die(msg + "; nothing was approved")
    try:
        r = subprocess.run(argv, input=prompt, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        fail("reviewer timed out")
    if r.returncode != 0:
        tail_ = "\n".join((r.stderr or r.stdout).strip().splitlines()[-15:])
        fail(f"reviewer failed (exit {r.returncode}):\n{tail_}")
    usage = {}
    for line in (r.stdout or "").splitlines():
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        if isinstance(ev, dict) and ev.get("type") == "turn.completed" and isinstance(ev.get("usage"), dict):
            usage = ev["usage"]
    verdict = load_json(out_path)
    if not valid_verdict(verdict):
        fail("reviewer returned no valid verdict")
    return verdict, {"model": model, "effort": effort, "seconds": round(time.time() - started), "usage": usage}


CRITERION_RE = re.compile(r"\bR-\d+\.C\d+\b")


def criteria_in(text):
    return sorted(set(CRITERION_RE.findall(text)), key=lambda c: [int(x) for x in re.findall(r"\d+", c)])


def judge(verdict, required, ledger, carried, final):
    """Whether a verdict approves its scope: the reviewer approved; judged every
    criterion in scope, none not met; raised no blocker or major finding; settled
    every open earlier finding, leaving no blocker or major one unaddressed. A final
    approval with anything nobody could verify (now, or carried from a step) goes to
    the user. Returns (approved, needs_user, reasons, unverifiable)."""
    reasons = []
    if verdict["verdict"] != "approve":
        reasons.append("the reviewer requested changes")
    by_id = {c["id"]: c for c in verdict["criteria"]}
    missing = [c for c in required if c not in by_id]
    if missing:
        reasons.append("the reviewer did not judge " + ", ".join(missing))
    unmet = [c["id"] for c in verdict["criteria"] if c["status"] == "not_met"]
    if unmet:
        reasons.append("not met: " + ", ".join(unmet))
    bad = [f for f in verdict["findings"] if f["severity"] in ("blocker", "major")]
    if bad:
        reasons.append(f"{len(bad)} blocker/major finding(s)")
    settled = {p["id"]: p for p in verdict["previous_findings"]}
    open_ = {i: f for i, f in ledger.items() if f["status"] == "open"}
    unsettled = [i for i in open_ if i not in settled]
    if unsettled:
        reasons.append("earlier findings not settled: " + ", ".join(unsettled))
    still = [i for i, f in open_.items() if f["severity"] in ("blocker", "major")
             and settled.get(i, {}).get("status") == "not_addressed"]
    if still:
        reasons.append("earlier blocker/major findings not addressed: " + ", ".join(still))
    unverifiable = [c["id"] for c in verdict["criteria"] if c["status"] == "cannot_verify"]
    if final:
        unverifiable += [c for c in carried if c not in unverifiable and by_id.get(c, {}).get("status") != "met"]
    ok = not reasons
    return ok, ok and final and bool(unverifiable), reasons, unverifiable


def update_ledger(ledger, verdict, scope, n):
    for p in verdict["previous_findings"]:
        if p["id"] in ledger and ledger[p["id"]]["status"] == "open" and p["status"] != "not_addressed":
            ledger[p["id"]].update(status=p["status"], note=p["note"], settled_in=n)
    for f in verdict["findings"]:
        ledger[f"{scope}-F{len(ledger) + 1}"] = {**f, "status": "open", "round": n}


# ---------------------------------------------------------------- review


def indent(text, empty):
    return "".join(f"    {l}\n" for l in (text or empty).splitlines())


def repos_block(st, since, final):
    out = []
    for name, r in st["repos"].items():
        start = since.get(name) or r["base"]
        alias = f" ({r['alias']})" if r.get("alias") else ""
        part = (f"- {name}{alias}: {r['path']}\n"
                f"  session base {r['base']} (branch {r['branch']}), now {git(r['path'], 'rev-parse', 'HEAD')} "
                f"on {git(r['path'], 'rev-parse', '--abbrev-ref', 'HEAD')}\n"
                f"  present before the session, not part of the change:\n{indent(r['preexisting'], '(nothing)')}")
        if final:
            part += (f"  the whole change: `git -C {r['path']} diff {r['base']}`\n"
                     + indent(git(r["path"], "diff", "--stat", r["base"]), "(no changes)"))
        else:
            part += (f"  THIS step's changes: `git -C {r['path']} diff {start}`\n"
                     + indent(git(r["path"], "diff", "--stat", start), "(no changes in this repository)"))
            if start != r["base"]:
                part += (f"  earlier steps, already approved (not this step's scope): "
                         f"`git -C {r['path']} log --oneline {r['base']}..{start}`\n")
        out.append(part)
    return "\n".join(out)


def ledger_text(ledger):
    open_ = [f"- {i} [{f['severity']}] {f['location']}: {f['problem']} (asked fix: {f['fix']})"
             for i, f in ledger.items() if f["status"] == "open"]
    settled = [f"- {i} {f['status']} in round {f.get('settled_in')}: {f['problem']}"
               for i, f in ledger.items() if f["status"] != "open"]
    text = "Open — settle every one of these by its id in `previous_findings`:\n" + ("\n".join(open_) or "none")
    if settled:
        text += "\nAlready settled (do not reopen without new evidence):\n" + "\n".join(settled)
    return text


REVIEW_RULES = """\
You are the independent reviewer in a pair session; another model (the executor) did
the work. You may read any file and run read-only commands (git log/diff/show/status,
grep, cat, ls). You cannot write files or use the network.

Two kinds of evidence exist. The command log is recorded by the harness, not by the
executor: each entry is a command the executor actually ran, with its exit code and the
tail of its output (secrets masked). The evidence file is the executor's own account,
mapping each verification to proof. Trust the log over the account: a claim the log
does not support is not proven. For external operations (registries, cloud APIs) the log
is the proof; say in the criterion's evidence which log entry proves it.

The log also holds the user's own messages and answers ("kind": "user_message" or
"question"): a deviation from the plan counts as decided only if the user decided it
there. The executor's evidence file may answer earlier findings; treat its arguments as
claims, and never lower a finding's severity on a stated rationale alone.

Check:
1. Every acceptance criterion and verification in scope is met and proven by the diff,
   the files or the command log.
2. Nothing outside the scope changed: no side refactors, no unrelated files, no edits to
   the plan. Changes present before the session do not count.
3. No secrets or credentials in the diff, the evidence or the log.
4. Earlier findings: settle every open one by its id in `previous_findings`: addressed,
   not_addressed, or withdrawn (only on new evidence), with a short note. On a re-review
   report new problems only if they are real and in scope; prefer one strong finding over
   several weak ones, and do not reopen settled points.

Approve only if all of this holds. Otherwise request changes. Each finding names the
file or command, the problem and the exact fix. Severity: blocker (wrong or unsafe),
major (a criterion not met or not proven), minor (does not block). Use cannot_verify
only when neither the files nor the log can prove a criterion; it goes to the user. In
`declined`, list anything you chose not to judge, with the reason. Return the JSON object
the schema requires, with an entry in `criteria` for every criterion id listed above.
"""


def log_summary(d, sid):
    path = os.path.join(d, "commands.jsonl")
    n = 0
    try:
        with open(path) as f:
            for line in f:
                try:
                    if sid is None or json.loads(line).get("step") == sid:
                        n += 1
                except ValueError:
                    pass
    except OSError:
        pass
    which = "every entry" if sid is None else f'entries with "step": "{sid}"'
    return f"Command log (recorded by the harness): {path} — {which}, {n} command(s)"


def snapshot(st):
    """The commits and the working-tree content a review is about."""
    return {"heads": heads(st), "trees": trees(st)}


def review_step(d, st, sid, final, user_approved):
    plan = st["plan"]
    if sha256_file(plan) != st["plan_sha256"]:
        die("the plan changed since the session started; ask the user (a changed plan needs a new "
            "Shogun approval and a new session)")
    forged = forged_entries(d)
    if forged:
        die(f"the command log has entries the hooks did not write (lines {', '.join(map(str, forged[:10]))}); "
            f"stop and tell the user")
    scope = "final" if final else sid
    step = st["final"] if final else step_of(st, sid)
    reviews = step["reviews"]
    failed = sum(1 for r in reviews if not r["approved"] and not r.get("discarded"))
    if failed >= MAX_REVIEWS and not user_approved:
        die(f"{failed} reviews without approval. Stop and ask the user how to proceed; if they want another "
            f"review, run it with --user-approved \"<their decision>\" (they confirm it in a permission prompt)")
    for name, r in st["repos"].items():
        fresh = new_changes(st, r)
        if fresh:
            die(f"commit the {'change' if final else 'step'}'s changes in {name} before the review "
                f"(uncommitted: {', '.join(fresh[:5])})")
    ev = os.path.join(d, "evidence", scope + ".md")
    if not os.path.exists(ev) or os.path.getsize(ev) == 0:
        die(f"write the evidence first: {ev} (for each verification: which logged command proves it, and how)")
    text = open(plan).read()
    if final:
        what = "the whole change (final review)"
        required = criteria_in(text)
        carried = sorted(st.get("unverified", {}))
        intro = ("Review the complete change against the plan's goal, every requirement and its acceptance "
                 "criteria, and the plan's end-to-end verification. Every step was approved separately; look "
                 "for gaps between steps, missing criteria and anything outside the plan's scope.\n")
        if carried:
            intro += ("Step reviews could not verify: " + ", ".join(carried) + ". Mark each met only if the log "
                      "or the files now prove it.\n")
        since = {}
    else:
        section = next(s["text"] for s in plan_steps(text) if s["id"] == sid)
        what = f"step {sid} ({step['title']})"
        required = criteria_in(section)
        carried = []
        intro = f"The step, as the plan states it:\n\n{section}\n"
        since = step.get("start") or {}
    ledger = step.setdefault("ledger", {})
    n = len(reviews) + 1
    prompt = (f"Review {what} of the plan at {plan}. Read the plan file for the requirements, criteria and "
              f"context.\n\n{intro}\nCriteria in scope: {', '.join(required) or 'none listed'}\n\n"
              f"Repositories:\n{repos_block(st, since, final)}\n"
              f"Executor's evidence: {ev}\n{log_summary(d, None if final else sid)}\n\n"
              f"Earlier findings in this scope:\n{ledger_text(ledger)}\n\n" + REVIEW_RULES)
    name = f"{scope}-r{n}"
    os.makedirs(os.path.join(d, "reviews"), exist_ok=True)
    with open(os.path.join(d, "reviews", name + ".prompt.md"), "w") as f:
        f.write(prompt)
    attempt = os.path.join(d, "reviews", f"{name}.attempt-{time.time_ns()}.json")
    out = os.path.join(d, "reviews", name + ".json")
    root = os.path.commonpath([r["path"] for r in st["repos"].values()] + [os.path.dirname(plan), d])
    before = snapshot(st)
    st["reviewing"] = {"pid": os.getpid(), "scope": name, "since": now()}
    save_state(d, st)
    try:
        verdict, meta = run_codex(prompt, root, attempt)
    finally:
        st = load_state(d)
        st["reviewing"] = None
        save_state(d, st)
    os.replace(attempt, out)
    step = st["final"] if final else step_of(st, sid)
    reviews, ledger = step["reviews"], step.setdefault("ledger", {})
    entry = {"n": n, "file": out, "verdict": verdict["verdict"], "at": now(), "heads": before["heads"],
             "trees": before["trees"],
             "user_approved": user_approved or None, **meta}
    if snapshot(st) != before:
        reviews.append({**entry, "approved": False, "discarded": "the change moved during the review"})
        save_state(d, st)
        print(f"{what}: review {n} DISCARDED — commits or uncommitted changes moved while the reviewer worked, so "
              f"its verdict is not about the current change. Nothing was approved; review again.")
        return
    ok, needs_user, reasons, unverifiable = judge(verdict, required, ledger, carried, final)
    update_ledger(ledger, verdict, scope, n)
    reviews.append({**entry, "approved": ok and not needs_user, "reasons": reasons})
    if needs_user:
        step["status"] = "needs_user"
        step["unverified"] = unverifiable
    elif ok:
        step["status"] = "approved"
        if final:
            step["heads"], step["trees"] = before["heads"], before["trees"]
        else:
            unv = st.setdefault("unverified", {})
            for c in verdict["criteria"]:
                if c["status"] == "cannot_verify":
                    unv[c["id"]] = {"scope": sid, "evidence": c["evidence"]}
                elif c["status"] == "met":
                    unv.pop(c["id"], None)
            nxt = current_step(st)
            if nxt:
                nxt["start"] = before["heads"]
    else:
        step["status"] = "changes_requested"
    save_state(d, st)
    result = "NEEDS THE USER'S CHECK" if needs_user else ("APPROVED" if ok else "CHANGES REQUESTED")
    print(f"{what}: review {n} -> {result}")
    print("summary: " + verdict["summary"])
    for c in verdict["criteria"]:
        print(f"  criterion {c['id']}: {c['status']} — {c['evidence']}")
    for p in verdict["previous_findings"]:
        print(f"  earlier finding {p['id']} {p['status']}: {p['note']}")
    for i, f in ledger.items():
        if f.get("round") == n:
            print(f"  {i} [{f['severity']}] {f['location']}: {f['problem']}\n      fix: {f['fix']}")
    for x in verdict["declined"]:
        print("  not judged: " + x)
    if needs_user:
        print("NEEDS THE USER: nobody could verify " + ", ".join(unverifiable) + ". Ask the user to check these; "
              f"when they confirm, run `python3 {SCRIPT} confirm \"<what they checked>\"` (they approve it in a "
              "permission prompt).")
    elif not ok:
        print("  open because: " + "; ".join(reasons))
    tokens = meta["usage"]
    print(f"review file: {out} ({meta['seconds']}s" + (f", tokens {tokens}" if tokens else "") + ")")


# ---------------------------------------------------------------- commands


def cmd_start(args):
    plan = os.path.realpath(args.plan)
    text = open(plan).read()
    steps = plan_steps(text)
    if not steps:
        die("no steps (### S-NNN — title) in " + plan)
    receipt = sidecars(plan)[1]
    if not args.unapproved:
        if not os.path.exists(receipt):
            die(f"no approval receipt {receipt}: the plan is not approved; ask the user (only they can allow "
                f"--unapproved)")
        if shutil.which("shogun"):
            v = subprocess.run(["shogun", "verify", plan], capture_output=True, text=True)
            if v.returncode != 0:
                die("shogun verify failed: " + (v.stdout + v.stderr).strip()[-500:])
    names, aliases = plan_repos(text)
    paths = {}
    for spec in args.repo or []:
        k, _, v = spec.partition("=")
        paths[k] = os.path.realpath(os.path.expanduser(v))
    manifest = load_json(sidecars(plan)[2], {}) or {}
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
        repos[name] = {"path": os.path.realpath(p), "alias": alias,
                       "base": git(p, "rev-parse", "HEAD"), "branch": git(p, "rev-parse", "--abbrev-ref", "HEAD"),
                       "preexisting": git(p, "status", "--porcelain"),
                       "preexisting_content": preexisting_state(p)}
    d = os.path.splitext(plan)[0] + ".niten"
    if os.path.exists(d):
        existing = os.path.exists(os.path.join(d, "state.json"))
        if existing and not args.restart:
            die(f"a session already exists in {d}; continue it (attach), or ask the user about --restart")
        # A session starts from an empty directory: nothing prepared beforehand
        # (a log, evidence) can pass as the session's own record.
        archive = d + "." + time.strftime("%Y%m%d%H%M%S")
        os.rename(d, archive)
        unregister(d)
        print(("previous session" if existing else "files left without a session") + " archived: " + archive)
    st = {"plan": plan, "plan_sha256": sha256_file(plan), "approval_sha256": sha256_file(receipt),
          "unapproved": bool(args.unapproved), "started": now(), "cwd": cwd, "repos": repos, "paused": None,
          "steps": [{"id": s["id"], "title": s["title"], "status": "pending", "reviews": [], "start": None}
                    for s in steps],
          "final": {"status": "pending", "reviews": []}}
    st["steps"][0]["start"] = {n: r["base"] for n, r in repos.items()}
    os.makedirs(os.path.join(d, "evidence"), exist_ok=True)
    save_state(d, st)
    register(cwd, d)
    print("niten session started: " + d)
    if not session_id():
        print("WARNING: not inside a Claude Code session, so the hooks do not guard this session until "
              "`niten.py attach --state " + d + "` is run from one")
    for name, r in repos.items():
        print(f"  {name}: {r['path']} at {r['base'][:12]} on {r['branch']}"
              + (" (has pre-existing changes)" if r["preexisting"] else ""))
    for s in st["steps"]:
        print(f"  {s['id']}: {s['title']}")
    print("evidence goes to " + os.path.join(d, "evidence", "<step>.md") + "; commands are logged to "
          + os.path.join(d, "commands.jsonl"))


def cmd_status(args):
    d = state_dir_arg(args)
    st = load_state(d)
    print("session: " + d)
    print("plan: " + st["plan"])
    if sha256_file(st["plan"]) != st["plan_sha256"]:
        print("WARNING: the plan file changed since the session started; reviews are refused")
    if st["paused"]:
        print(f"PAUSED since {st['paused']['since']}: {st['paused']['reason']}")
    if st.get("reviewing"):
        print(f"REVIEWING {st['reviewing']['scope']} since {st['reviewing']['since']}")
    for s in st["steps"]:
        print(f"  {s['id']} [{s['status']}] {s['title']} ({len(s['reviews'])} review(s))")
    print(f"  final [{st['final']['status']}] ({len(st['final']['reviews'])} review(s))")
    cur = current_step(st)
    if cur:
        print("next: " + cur["id"])
    elif st["final"]["status"] == "needs_user":
        print("next: the user checks what the reviewer could not verify, then confirm")
    elif st["final"]["status"] != "approved":
        print("next: final review")
    else:
        moved = moved_since(st, st["final"])
        print("next: delivery" if not moved else "WARNING: commits changed after the final review: "
              + ", ".join(moved) + "; run the final review again")


def cmd_review(args):
    d = state_dir_arg(args)
    st = load_state(d)
    cur = current_step(st)
    if not cur:
        die("every step is approved; run final")
    if cur["id"] != args.step:
        die(f"{cur['id']} is the current step; steps are reviewed in order")
    review_step(d, st, args.step, False, args.user_approved)


def cmd_final(args):
    d = state_dir_arg(args)
    st = load_state(d)
    cur = current_step(st)
    if cur:
        die(f"{cur['id']} is not approved yet; the final review comes after every step")
    review_step(d, st, None, True, args.user_approved)


def cmd_confirm(args):
    """The user checked what the final reviewer could not verify."""
    d = state_dir_arg(args)
    st = load_state(d)
    final = st["final"]
    if final["status"] != "needs_user":
        die("nothing waits for the user's check")
    last = final["reviews"][-1]
    moved = moved_since(st, last)
    if moved:
        die("the change moved since the final review (" + ", ".join(moved) + "); run final again")
    final["status"] = "approved"
    final["heads"], final["trees"] = last["heads"], last["trees"]
    final["user_check"] = {"what": args.what, "criteria": final.get("unverified", []), "at": now()}
    save_state(d, st)
    print("final review completed by the user's check: " + args.what)


def cmd_pause(args):
    d = state_dir_arg(args)
    st = load_state(d)
    st["paused"] = {"reason": args.reason, "since": now()}
    save_state(d, st)
    print("paused until the user answers: " + args.reason)


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
    if not args.abort:
        if st["final"]["status"] != "approved":
            die("the final review has not approved the change; ask the user before --abort")
        moved = moved_since(st, st["final"])
        if moved:
            die("the change differs from what the final review approved (" + ", ".join(moved) + "); run final again")
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


def active(data):
    d = find_active(data.get("cwd") or os.getcwd(), data.get("session_id", ""), by_cwd=False)
    st = load_json(os.path.join(d, "state.json")) if d else None
    return (d, st) if st and not st.get("finished") else (None, None)


def where(st):
    cur = current_step(st)
    return f"{cur['id']} ({cur['title']})" if cur else "final stage"


def alive(pid):
    try:
        os.kill(int(pid), 0)
        return True
    except (OSError, ValueError, TypeError):
        return False


def progress(d, st):
    """What changes when the session moves: reviews, the log, the commits."""
    try:
        log = os.path.getsize(os.path.join(d, "commands.jsonl"))
    except OSError:
        log = 0
    reviews = sum(len(s["reviews"]) for s in st["steps"]) + len(st["final"]["reviews"])
    return hashlib.sha256(json.dumps([reviews, log, heads(st), trees(st)], sort_keys=True).encode()).hexdigest()


def cmd_hook_stop(args):
    data = hook_input()
    d, st = active(data) if data else (None, None)
    if not st or st.get("paused"):
        return
    if st.get("reviewing") and alive(st["reviewing"].get("pid")):
        return  # the review runs in the background and wakes the executor when it ends
    cur = current_step(st)
    if not cur and st["final"]["status"] in ("approved", "needs_user"):
        return
    fp = progress(d, st)
    blocks = st.get("stop_blocks") or {}
    count = blocks.get("count", 0) + 1 if blocks.get("progress") == fp else 1
    st["stop_blocks"] = {"progress": fp, "count": count}
    save_state(d, st)
    if count > MAX_STOP_BLOCKS:
        st["stop_blocks"] = None
        save_state(d, st)
        print(json.dumps({"systemMessage": (f"Niten: the turn ends without progress at {where(st)}, which is not "
                                            f"approved. It needs your attention: answer, or say how to go on.")}))
        return
    if cur:
        reason = (f"Niten session ({d}): step {cur['id']} is {cur['status']}, not approved by the reviewer. "
                  f"Finish the step, commit, write its evidence and run `python3 {SCRIPT} review {cur['id']}`, "
                  f"then fix any findings. If you need the user (access, a decision, missing information), "
                  f"run `python3 {SCRIPT} pause \"<what you need>\"` and ask them.")
    else:
        reason = (f"Niten session ({d}): every step is approved; run the end-to-end verification and the final "
                  f"review `python3 {SCRIPT} final` before you stop.")
    print(json.dumps({"decision": "block", "reason": reason}))


def decision(kind, reason):
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": kind,
        "permissionDecisionReason": reason,
    }}))


def protected_paths(d, st):
    """Files the executor must not change by hand: the session state (except its own
    evidence), the plan and its receipts, Niten's registry and Claude's settings."""
    paths = [d, NITEN_HOME] + sidecars(st["plan"])
    for root in [HOME] + [r["path"] for r in st["repos"].values()]:
        paths += [os.path.join(root, ".claude", name) for name in ("settings.json", "settings.local.json")]
    return paths


def is_protected(path, d, st):
    if not path:
        return False
    path = os.path.realpath(os.path.expanduser(path))
    if within(path, os.path.join(d, "evidence")) and path.endswith(".md"):
        return False
    return any(within(path, p) for p in protected_paths(d, st))


WRITES = re.compile(r">|\btee\b|\bsed\s+-\w*i|\bperl\s+-\w*i|\b(mv|cp|rm|truncate|dd|touch|ln|install|chmod|"
                    r"python3?|node|ruby)\b")
NITEN_CALL = re.compile(r"^\s*python3?\s+\S*niten\.py\s+[^;&|<>`$]*$")


def bash_touches_protected(command, d, st):
    """A shell command that names a protected file and could write it. Plain calls of
    niten.py are how the state changes."""
    if NITEN_CALL.match(command):
        return False
    evidence = os.path.join(d, "evidence") + os.sep
    rest = command.replace(evidence, "").replace(os.path.join(os.path.basename(d), "evidence") + os.sep, "")
    names = [os.path.basename(d)]  # unique: <plan>.niten
    for p in protected_paths(d, st):
        names.append(p)
        if within(p, HOME):
            rel = os.path.relpath(p, HOME)
            names += ["~/" + rel, "$HOME/" + rel, "${HOME}/" + rel]
    return any(n and n in rest for n in names) and bool(WRITES.search(rest))


SENSITIVE = [".aws", ".docker", ".ssh", ".kube", ".config", ".gnupg", ".netrc", ".npmrc", ".pypirc",
             ".gitconfig", ".git-credentials", ".zshrc", ".bashrc", ".profile"]


def outside_workspace(path, data, st):
    path = os.path.realpath(os.path.expanduser(path))
    if any(within(path, os.path.join(HOME, s)) for s in SENSITIVE):
        return True
    roots = [r["path"] for r in st["repos"].values()] + [st_dir(st)]
    roots += ["/tmp", "/private/tmp", "/var/folders", "/private/var/folders", os.path.join(HOME, ".claude", "projects")]
    for extra in (os.environ.get("TMPDIR"), data.get("scratchpad_dir")):
        if extra:
            roots.append(extra)
    return not any(within(path, r) for r in roots)


READ_TOOLS = {"Read": "file_path", "Grep": "path", "Glob": "path", "NotebookRead": "notebook_path"}
HOOK_CALL = re.compile(r"\bniten\.py\b[^|;&]*\bhook-|\bhook-(stop|pretooluse|posttooluse|userprompt)\b")
NITEN_CODE = re.compile(r"niten\.py|skills/niten/scripts|\bimport\s+niten\b|\bfrom\s+niten\s+import\b|"
                        + re.escape(HERE))
# Claude's own directory (settings, transcripts, Niten's registry and the log's key)
# is out of a session's reach: no mention of it, no glob that could expand to a hidden
# name, no option that makes globs match hidden names, no recursive sweep of HOME.
CLAUDE_REACH = re.compile(r"\.claude\b|hook\.key|(^|[\s/=(])\.[^\s/()]*[*?\[]|dotglob|globdots|GLOBIGNORE|"
                          r"\*\([^)]*D[^)]*\)")


def home_sweep(command):
    roots = r"(~|\$HOME|\$\{HOME\}|" + re.escape(HOME) + r")/?"
    return re.search(r"\b(find|grep|egrep|rg|ag|ack|tar|zip|rsync|cp|scp|du|ls|tree)\b[^|;&]*\s" + roots
                     + r"(\s|$|[;&|)])", command)


def reaches_claude_dir(command):
    flat = re.sub(r"[\'\"\\]", "", command)  # .cla''ude, ".claude", \.claude
    return bool(CLAUDE_REACH.search(flat) or home_sweep(flat))


START_OVERRIDE = re.compile(r"\bniten\.py\b[^|;&]*\bstart\b[^|;&]*--(unapproved|restart)\b")
INTERPRETER = re.compile(r"\b(python[0-9.]*|node|ruby|perl|bash|sh|zsh|osascript|exec|eval|source)\b")


def key_mentions():
    rel = os.path.relpath(KEY, HOME)
    return [KEY, "~/" + rel, "$HOME/" + rel, "${HOME}/" + rel, "hook.key"]


def explained(tool_input, label, where_):
    """Ask the user about an important action, with the executor's explanation, or
    refuse it until the executor gives one."""
    explanation = " ".join((tool_input.get("description") or "").split())
    if len(explanation) < MIN_EXPLANATION:
        decision("deny", f"Niten: this is {label}, so the user approves it. Run the same command again with a "
                         f"Bash `description` that tells the user, in one or two sentences, what it does and why "
                         f"the current plan step needs it.")
    else:
        decision("ask", f"Niten {where_}, {label}: {explanation}")


def cmd_hook_pretooluse(args):
    """In a Niten session: only Claude Code runs the hooks and the key that signs the
    log stays unread; delivery only after the final review of the exact commits;
    important actions go to the user with the executor's explanation, even where the
    permission settings would allow them; protected files are not edited by hand.
    Before a session exists, starting one without approval or over an earlier one is
    put to the user. Nothing else happens outside a session."""
    data = hook_input()
    if not data:
        return
    tool = data.get("tool_name", "")
    tool_input = data.get("tool_input") or {}
    command = tool_input.get("command", "") if tool == "Bash" else ""
    d, st = active(data)
    if not st:
        if command and START_OVERRIDE.search(command):
            explained(tool_input, "a Niten override", "(no session yet)")
        return
    if command and (HOOK_CALL.search(command) or any(k in command for k in key_mentions())
                    or (NITEN_CODE.search(command) and INTERPRETER.search(command) and not NITEN_CALL.match(command))):
        decision("deny", "Niten: the hooks and their signing key belong to Claude Code, not to the session. "
                         "Use niten.py's own commands.")
        return
    if command and not NITEN_CALL.match(command) and reaches_claude_dir(command):
        decision("deny", "Niten: Claude's own directory (~/.claude) is out of reach in a session, and so are "
                         "globs over hidden names and sweeps of the home directory. Read project files with the "
                         "Read tool; ask the user if you need something there.")
        return
    if tool in READ_TOOLS:
        path = tool_input.get(READ_TOOLS[tool]) or ""
        real = os.path.realpath(os.path.expanduser(path)) if path else ""
        probe = " ".join(str(tool_input.get(k, "")) for k in ("path", "pattern", "glob", "file_path"))
        if (real and (within(real, NITEN_HOME) or (tool != "Read" and within(KEY, real))))  \
                or (tool != "Read" and reaches_claude_dir(probe)):
            decision("deny", "Niten: the log's signing key and Niten's registry are not readable in a session.")
            return

    if tool in FILE_TOOLS:
        path = tool_input.get(FILE_TOOLS[tool], "")
        if is_protected(path, d, st):
            decision("deny", f"Niten: {path} is part of the session's record, the approved plan or Claude's "
                             f"settings; it is not edited by hand. Use niten.py, or ask the user.")
        elif path and outside_workspace(path, data, st):
            decision("ask", f"Niten {where(st)}: {tool} of {path}, outside the plan's repositories.")
        return

    if tool.startswith("mcp__"):
        if MCP_WRITE.search(tool.split("__")[-1]):
            summary = redact(json.dumps(tool_input, ensure_ascii=False))[:300]
            decision("ask", f"Niten {where(st)}: {tool} changes another system — {summary}")
        return

    if tool != "Bash":
        return
    if bash_touches_protected(command, d, st):
        decision("deny", "Niten: this command would change the session's record, the approved plan or Claude's "
                         "settings. The state changes only through niten.py; ask the user if something is wrong.")
        return
    delivery = any(p.search(command) for p in DELIVERY)
    if delivery:
        final = st["final"]
        if final["status"] != "approved":
            decision("deny", f"Niten session ({d}): delivery (push or pull request) waits for the final review. "
                             f"Approve every step, then run `python3 {SCRIPT} final`.")
            return
        moved = [n for n, h in heads(st).items() if h != final.get("heads", {}).get(n)]
        if moved:
            decision("deny", f"Niten: {', '.join(moved)} changed after the final review; run "
                             f"`python3 {SCRIPT} final` again before delivering.")
            return
    label = important(command) or ("delivery" if delivery else None)
    if label:
        explained(tool_input, label, where(st))


def tail(text):
    text = redact(text or "")
    return text if len(text) <= LOG_TAIL else "…" + text[-LOG_TAIL:]


def log_entry(d, st, entry):
    cur = current_step(st)
    entry = {"at": now(), "step": cur["id"] if cur else "final", **entry}
    entry["sig"] = signature(entry)
    with open(os.path.join(d, "commands.jsonl"), "a") as f:
        f.write(json.dumps(entry, ensure_ascii=False) + "\n")


def cmd_hook_posttooluse(args):
    """Record what the session did, as the reviewer's evidence: every Bash command
    with its exit code and output (also when it failed), and the user's answers to
    the executor's questions."""
    data = hook_input()
    d, st = active(data) if data else (None, None)
    if not st:
        return
    tool = data.get("tool_name")
    tool_input = data.get("tool_input") or {}
    resp = data.get("tool_response")
    failed = data.get("hook_event_name") == "PostToolUseFailure"
    if tool == "AskUserQuestion":
        log_entry(d, st, {"kind": "question", "questions": redact(json.dumps(tool_input, ensure_ascii=False))[:LOG_TAIL],
                          "answers": tail(json.dumps(resp, ensure_ascii=False) if resp is not None else data.get("error", ""))})
        return
    if tool != "Bash":
        return
    if isinstance(resp, dict):
        out, err = resp.get("stdout", ""), resp.get("stderr", "")
        code = resp.get("exit_code", resp.get("exitCode"))
        interrupted = resp.get("interrupted")
    else:
        out, err, code, interrupted = str(resp or ""), "", None, None
    if failed:
        err = (err + "\n" if err else "") + str(data.get("error") or "")
        code = code if code not in (None, 0) else "failed"
    command = tool_input.get("command", "")
    if re.search(r"get-login-password|print-access-token|get-token|\btoken\b", command) and "|" not in command:
        out = "[output withheld: credential]"
    log_entry(d, st, {"kind": "command", "command": redact(command), "description": tool_input.get("description", ""),
                      "exit_code": code, "interrupted": interrupted, "stdout": tail(out), "stderr": tail(err)})


def cmd_hook_userprompt(args):
    """Record the user's message for the reviewer; a pause lasts until the user
    answers, and their message resumes the session."""
    data = hook_input()
    d, st = active(data) if data else (None, None)
    if not st:
        return
    text = data.get("prompt", data.get("prompt_text", ""))
    if text:
        log_entry(d, st, {"kind": "user_message", "text": tail(text)})
    if not st.get("paused"):
        return
    reason = st["paused"]["reason"]
    st["paused"] = None
    save_state(d, st)
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "UserPromptSubmit",
        "additionalContext": (f"Niten: the user answered your question ({reason}); the session is active again, "
                              f"at {where(st)}. Continue the plan with their answer."),
    }}))


# ---------------------------------------------------------------- main


def main():
    ap = argparse.ArgumentParser(prog="niten.py")
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("start")
    p.add_argument("--plan", required=True)
    p.add_argument("--repo", action="append", help="NAME=PATH or repo-N=PATH")
    p.add_argument("--cwd")
    p.add_argument("--restart", action="store_true", help="archive an existing session and begin again")
    p.add_argument("--unapproved", action="store_true", help="run a plan without an approval receipt")
    p.set_defaults(fn=cmd_start)
    for name, fn in [("status", cmd_status), ("resume", cmd_resume)]:
        p = sub.add_parser(name)
        p.add_argument("--state")
        p.set_defaults(fn=fn)
    p = sub.add_parser("review")
    p.add_argument("step")
    p.add_argument("--state")
    p.add_argument("--user-approved", metavar="DECISION", help="another review after the limit, as the user decided")
    p.set_defaults(fn=cmd_review)
    p = sub.add_parser("final")
    p.add_argument("--state")
    p.add_argument("--user-approved", metavar="DECISION", help="another review after the limit, as the user decided")
    p.set_defaults(fn=cmd_final)
    p = sub.add_parser("confirm")
    p.add_argument("what", help="what the user checked")
    p.add_argument("--state")
    p.set_defaults(fn=cmd_confirm)
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
    for name, fn in [("hook-stop", cmd_hook_stop), ("hook-pretooluse", cmd_hook_pretooluse),
                     ("hook-posttooluse", cmd_hook_posttooluse), ("hook-userprompt", cmd_hook_userprompt)]:
        sub.add_parser(name).set_defaults(fn=fn)
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
