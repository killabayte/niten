"""Offline tests of niten.py and install.sh: temporary HOME, git repositories and a
scripted codex that writes a verdict. No model is called."""

import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPT = os.path.join(ROOT, "skills", "niten", "scripts", "niten.py")
INSTALL = os.path.join(ROOT, "install.sh")

PLAN = textwrap.dedent("""\
    ---
    title: DEMO-1 — demo
    repos:
        - app
        - infra
    ---

    ## Inputs and versions

    - repo-1: `app` at abc
    - repo-2: `infra` at def

    ## Steps

    ### S-001 — First step

    - Objective: one

    Verification:

    - V-001 (command, repo-1): ok

    ### S-002 — Second step

    - Objective: two

    ## End-to-end verification

    - all good
    """)

FAKE_CODEX = textwrap.dedent("""\
    #!/bin/sh
    out=""; prev=""
    for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
    cat > "$out.stdin"
    case "${FAKE_VERDICT:-approve}" in
    approve) echo '{"verdict":"approve","summary":"ok","criteria":[{"id":"R-001.C1","status":"met","evidence":"diff"}],"findings":[]}' > "$out" ;;
    major) echo '{"verdict":"approve","summary":"fine","criteria":[],"findings":[{"severity":"major","location":"f:1","problem":"wrong","fix":"right"}]}' > "$out" ;;
    unmet) echo '{"verdict":"approve","summary":"fine","criteria":[{"id":"R-001.C1","status":"not_met","evidence":"none"}],"findings":[]}' > "$out" ;;
    changes) echo '{"verdict":"request_changes","summary":"no","criteria":[],"findings":[{"severity":"minor","location":"f:1","problem":"style","fix":"rename"}]}' > "$out" ;;
    garbage) echo 'not json' > "$out" ;;
    fail) echo 'boom' >&2; exit 3 ;;
    esac
    """)


def git(path, *args):
    subprocess.run(["git", "-C", path, "-c", "user.email=t@t", "-c", "user.name=t", *args],
                   check=True, capture_output=True)


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = os.path.realpath(tempfile.mkdtemp())
        self.home = os.path.join(self.tmp, "home")
        self.ws = os.path.join(self.tmp, "ws")
        self.plans = os.path.join(self.tmp, "plans")
        for d in (self.home, self.ws, self.plans):
            os.makedirs(d)
        for r in ("app", "infra"):
            os.makedirs(os.path.join(self.ws, r))
            git(os.path.join(self.ws, r), "init", "-q")
            git(os.path.join(self.ws, r), "commit", "-q", "--allow-empty", "-m", "init")
        self.plan = os.path.join(self.plans, "DEMO-1.md")
        with open(self.plan, "w") as f:
            f.write(PLAN)
        self.codex = os.path.join(self.tmp, "fakecodex")
        with open(self.codex, "w") as f:
            f.write(FAKE_CODEX)
        os.chmod(self.codex, 0o755)
        self.state = os.path.join(self.plans, "DEMO-1.niten")

    def tearDown(self):
        subprocess.run(["rm", "-rf", self.tmp])

    def env(self, **extra):
        e = {k: v for k, v in os.environ.items() if not k.startswith(("NITEN_", "CLAUDE_"))}
        e.update(HOME=self.home, NITEN_CODEX=self.codex, CLAUDE_CODE_SESSION_ID="sess-1")
        e.update(extra)
        return e

    def run_niten(self, *args, stdin=None, cwd=None, **env):
        return subprocess.run([sys.executable, SCRIPT, *args], input=stdin, capture_output=True, text=True,
                              cwd=cwd or self.ws, env=self.env(**env))

    def start(self, *extra, **env):
        r = self.run_niten("start", "--plan", self.plan, *extra, **env)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r

    def evidence(self, step, text="ran: true (exit 0)\n"):
        with open(os.path.join(self.state, "evidence", step + ".md"), "w") as f:
            f.write(text)

    def hook(self, kind, session="sess-1", **data):
        data.setdefault("session_id", session)
        data.setdefault("cwd", self.ws)
        r = self.run_niten("hook-" + kind, stdin=json.dumps(data))
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout) if r.stdout.strip() else None

    def state_json(self):
        with open(os.path.join(self.state, "state.json")) as f:
            return json.load(f)


class StartTest(Base):
    def test_repositories_from_the_working_directory(self):
        out = self.start().stdout
        self.assertIn("S-001: First step", out)
        st = self.state_json()
        self.assertEqual(st["repos"]["app"]["path"], os.path.join(self.ws, "app"))
        self.assertEqual(st["repos"]["infra"]["alias"], "repo-2")

    def test_repositories_from_flags_manifest_and_workspace(self):
        other = os.path.join(self.tmp, "elsewhere", "app2")
        os.makedirs(other)
        git(other, "init", "-q")
        git(other, "commit", "-q", "--allow-empty", "-m", "x")
        with open(os.path.join(self.plans, "DEMO-1.manifest.json"), "w") as f:
            json.dump({"repos": [{"id": "repo-2", "root": os.path.join(self.ws, "infra")}]}, f)
        self.start("--repo", "repo-1=" + other, cwd=self.tmp)
        st = self.state_json()
        self.assertEqual(st["repos"]["app"]["path"], other)
        self.assertEqual(st["repos"]["infra"]["path"], os.path.join(self.ws, "infra"))
        r = self.run_niten("start", "--plan", self.plan, "--restart", cwd=self.tmp, NITEN_WORKSPACE=self.ws)
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_missing_repository_and_second_start_are_refused(self):
        r = self.run_niten("start", "--plan", self.plan, cwd=self.tmp)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("repository app: not found", r.stderr)
        self.start()
        r = self.run_niten("start", "--plan", self.plan)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("already exists", r.stderr)

    def test_preexisting_changes_are_recorded_and_shown_to_the_reviewer(self):
        with open(os.path.join(self.ws, "app", "untracked.txt"), "w") as f:
            f.write("x")
        self.start()
        self.assertIn("untracked.txt", self.state_json()["repos"]["app"]["preexisting"])
        self.evidence("S-001")
        self.assertEqual(self.run_niten("review", "S-001").returncode, 0)
        with open(os.path.join(self.state, "reviews", "S-001-r1.prompt.md")) as f:
            self.assertIn("untracked.txt", f.read())


class ReviewTest(Base):
    def setUp(self):
        super().setUp()
        self.start()

    def test_review_needs_evidence_and_order(self):
        r = self.run_niten("review", "S-001")
        self.assertIn("write the evidence first", r.stderr)
        self.assertNotEqual(r.returncode, 0)
        r = self.run_niten("review", "S-002")
        self.assertIn("S-001 is the current step", r.stderr)
        r = self.run_niten("final")
        self.assertIn("not approved yet", r.stderr)

    def test_only_a_clean_approve_approves(self):
        self.evidence("S-001")
        for verdict in ("major", "unmet", "changes"):
            r = self.run_niten("review", "S-001", FAKE_VERDICT=verdict)
            self.assertIn("CHANGES REQUESTED", r.stdout, verdict)
            self.assertEqual(self.state_json()["steps"][0]["status"], "changes_requested")
        r = self.run_niten("review", "S-001")
        self.assertIn("review 4 -> APPROVED", r.stdout)
        self.assertEqual(self.state_json()["steps"][0]["status"], "approved")
        with open(os.path.join(self.state, "reviews", "S-001-r4.prompt.md")) as f:
            self.assertIn("style", f.read())  # previous findings are shown to the reviewer

    def test_reviewer_failure_leaves_the_step_open(self):
        self.evidence("S-001")
        for verdict, message in (("garbage", "no valid verdict"), ("fail", "reviewer failed")):
            r = self.run_niten("review", "S-001", FAKE_VERDICT=verdict)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn(message, r.stderr)
        self.assertEqual(self.state_json()["steps"][0]["status"], "pending")

    def test_full_session(self):
        self.evidence("S-001")
        self.run_niten("review", "S-001")
        self.evidence("S-002")
        self.assertIn("APPROVED", self.run_niten("review", "S-002").stdout)
        r = self.run_niten("finish")
        self.assertIn("final review has not approved", r.stderr)
        self.assertIn("APPROVED", self.run_niten("final").stdout)
        self.assertIn("next: delivery", self.run_niten("status").stdout)
        self.assertEqual(self.run_niten("finish").returncode, 0)
        with open(os.path.join(self.home, ".claude", "niten", "active.json")) as f:
            self.assertEqual(json.load(f), [])


class HookTest(Base):
    def setUp(self):
        super().setUp()
        self.start()

    def test_stop_blocks_only_the_sessions_own_unapproved_step(self):
        out = self.hook("stop")
        self.assertEqual(out["decision"], "block")
        self.assertIn("S-001", out["reason"])
        self.assertIsNone(self.hook("stop", session="another"))

    def test_stop_allows_a_paused_session(self):
        self.run_niten("pause", "need a cloud login")
        self.assertIsNone(self.hook("stop"))
        self.run_niten("resume")
        self.assertIsNotNone(self.hook("stop"))

    def bash(self, command, description="", session="sess-1"):
        out = self.hook("pretooluse", session=session, tool_name="Bash",
                        tool_input={"command": command, "description": description})
        return out["hookSpecificOutput"] if out else None

    def test_push_waits_for_the_final_review(self):
        explained = "Push the work branch so the user can open a pull request for review"
        out = self.bash("git -C app push origin HEAD", explained)
        self.assertEqual(out["permissionDecision"], "deny")
        self.assertIn("final review", out["permissionDecisionReason"])
        for cmd in ("git status", "gh pr list"):
            self.assertIsNone(self.bash(cmd), cmd)
        self.assertIsNone(self.hook("pretooluse", tool_name="Write", tool_input={"file_path": "x"}))
        self.assertIsNone(self.bash("git push", explained, session="another"))
        for step in ("S-001", "S-002"):
            self.evidence(step)
            self.run_niten("review", step)
        self.assertIn("run the final review", self.hook("stop")["reason"])
        self.run_niten("final")
        out = self.bash("git -C app push origin HEAD", explained)
        self.assertEqual(out["permissionDecision"], "ask")
        self.assertIn("final stage", out["permissionDecisionReason"])
        self.assertIsNone(self.hook("stop"))

    def test_important_commands_are_put_to_the_user_with_an_explanation(self):
        why = "Push the mirrored uv image to the team registry, because this step mirrors the base images"
        for cmd in ("docker push registry/x:1", "aws --region r ecr batch-delete-image --repository-name x",
                    "aws s3 cp f s3://b/f", "terraform apply", "kubectl -n x delete pod y", "rm -rf build",
                    "curl -X POST https://api/x", "ssh host uptime",
                    "aws ecr get-login-password | docker login --password-stdin registry"):
            out = self.bash(cmd)
            self.assertEqual(out["permissionDecision"], "deny", cmd)
            self.assertIn("description", out["permissionDecisionReason"], cmd)
            self.assertEqual(self.bash(cmd, "push it").get("permissionDecision"), "deny", cmd)
            out = self.bash(cmd, why)
            self.assertEqual(out["permissionDecision"], "ask", cmd)
            self.assertIn("S-001 (First step)", out["permissionDecisionReason"], cmd)
            self.assertIn(why, out["permissionDecisionReason"], cmd)
            self.assertIsNone(self.bash(cmd, why, session="another"), cmd)

    def test_read_only_commands_pass_without_a_question(self):
        for cmd in ("aws ecr describe-images --repository-name x", "aws --profile p ecr list-images --repository-name x",
                    "aws sts get-caller-identity", "aws s3 ls s3://b", "docker pull --platform linux/amd64 img",
                    "docker build -t x .", "docker buildx imagetools inspect img", "terraform plan", "kubectl get pods",
                    "git diff --stat", "curl -s https://api/x", "rm -f tmp.txt", "grep -rn push ."):
            self.assertIsNone(self.bash(cmd), cmd)

    def test_the_ask_list_can_be_extended(self):
        os.makedirs(os.path.join(self.home, ".claude", "niten"), exist_ok=True)
        with open(os.path.join(self.home, ".claude", "niten", "config.json"), "w") as f:
            json.dump({"ask": [r"\bmake\s+deploy\b"]}, f)
        out = self.bash("make deploy", "Deploy the change to the test environment as the step requires")
        self.assertEqual(out["permissionDecision"], "ask")
        self.assertIn("ask list", out["permissionDecisionReason"])

    def test_hooks_never_fail_on_bad_input(self):
        for kind in ("stop", "pretooluse"):
            r = self.run_niten("hook-" + kind, stdin="not json")
            self.assertEqual(r.returncode, 0)
            self.assertEqual(r.stdout, "")

    def test_attach_moves_the_session_to_a_new_claude_session(self):
        self.assertIsNone(self.hook("stop", session="sess-2"))
        r = self.run_niten("attach", "--state", self.state, CLAUDE_CODE_SESSION_ID="sess-2")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIsNotNone(self.hook("stop", session="sess-2"))
        self.assertIsNone(self.hook("stop", session="sess-1"))


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.home = os.path.realpath(tempfile.mkdtemp())
        self.settings = os.path.join(self.home, ".claude", "settings.json")
        os.makedirs(os.path.dirname(self.settings))
        with open(self.settings, "w") as f:
            json.dump({"model": "x", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "other"}]}]}}, f)

    def tearDown(self):
        subprocess.run(["rm", "-rf", self.home])

    def sh(self, *args):
        env = {k: v for k, v in os.environ.items() if k != "CLAUDE_CONFIG_DIR"}
        env["HOME"] = self.home
        r = subprocess.run(["sh", INSTALL, *args], capture_output=True, text=True, env=env)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r.stdout

    def load(self):
        with open(self.settings) as f:
            return json.load(f)

    def test_install_is_idempotent_and_uninstall_restores(self):
        self.sh()
        link = os.path.join(self.home, ".claude", "skills", "niten")
        self.assertEqual(os.path.realpath(link), os.path.realpath(os.path.join(ROOT, "skills", "niten")))
        data = self.load()
        commands = [h["command"] for g in data["hooks"]["Stop"] for h in g["hooks"]]
        self.assertIn("other", commands)
        self.assertTrue(any("niten.py\" hook-stop" in c for c in commands))
        self.assertEqual(data["hooks"]["PreToolUse"][0]["matcher"], "Bash")
        self.assertIn("already up to date", self.sh())
        self.assertEqual(self.load(), data)
        self.sh("uninstall")
        self.assertFalse(os.path.lexists(link))
        self.assertEqual(self.load(), {"model": "x", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "other"}]}]}})


if __name__ == "__main__":
    unittest.main()
