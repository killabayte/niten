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
    printf '%s\\n' "$@" > "$out.argv"
    cat > "$out.stdin"
    echo '{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":2}}'
    E='"previous_findings":[],"declined":[]'
    case "${FAKE_VERDICT:-approve}" in
    approve) echo '{"verdict":"approve","summary":"ok","criteria":[{"id":"R-001.C1","status":"met","evidence":"log 1"}],"findings":[],'"$E"'}' > "$out" ;;
    major) echo '{"verdict":"approve","summary":"fine","criteria":[],"findings":[{"severity":"major","location":"f:1","problem":"wrong","fix":"right"}],'"$E"'}' > "$out" ;;
    unmet) echo '{"verdict":"approve","summary":"fine","criteria":[{"id":"R-001.C1","status":"not_met","evidence":"none"}],"findings":[],'"$E"'}' > "$out" ;;
    changes) echo '{"verdict":"request_changes","summary":"no","criteria":[],"findings":[{"severity":"minor","location":"f:1","problem":"style","fix":"rename"}],'"$E"'}' > "$out" ;;
    open) echo '{"verdict":"approve","summary":"ok","criteria":[],"findings":[],"previous_findings":[{"finding":"style","status":"not_addressed","note":"still there"}],"declined":["performance"]}' > "$out" ;;
    unverifiable) echo '{"verdict":"approve","summary":"ok","criteria":[{"id":"R-002.C1","status":"cannot_verify","evidence":"registry digest not in the log"}],"findings":[],'"$E"'}' > "$out" ;;
    badshape) echo '{"verdict":"approve","summary":"ok","criteria":[{"id":"R-001.C1","status":"probably"}],"findings":[],'"$E"'}' > "$out" ;;
    noledger) echo '{"verdict":"approve","summary":"ok","criteria":[],"findings":[]}' > "$out" ;;
    garbage) echo 'not json' > "$out" ;;
    fail) echo 'boom' >&2; exit 3 ;;
    esac
    """)

WHY = "Push the mirrored image to the team registry, because this step mirrors the base images"


def git(path, *args):
    return subprocess.run(["git", "-C", path, "-c", "user.email=t@t", "-c", "user.name=t", *args],
                          check=True, capture_output=True, text=True).stdout.strip()


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
        with open(os.path.join(self.plans, "DEMO-1.approval.json"), "w") as f:
            f.write("{}\n")
        self.codex = os.path.join(self.tmp, "fakecodex")
        with open(self.codex, "w") as f:
            f.write(FAKE_CODEX)
        os.chmod(self.codex, 0o755)
        self.state = os.path.join(self.plans, "DEMO-1.niten")

    def tearDown(self):
        subprocess.run(["rm", "-rf", self.tmp])

    def env(self, **extra):
        e = {k: v for k, v in os.environ.items() if not k.startswith(("NITEN_", "CLAUDE_"))}
        e.update(HOME=self.home, NITEN_CODEX=self.codex, CLAUDE_CODE_SESSION_ID="sess-1",
                 PATH="/usr/bin:/bin:/usr/sbin:/sbin")  # no shogun on PATH
        e.update(extra)
        return e

    def run_niten(self, *args, stdin=None, cwd=None, **env):
        return subprocess.run([sys.executable, SCRIPT, *args], input=stdin, capture_output=True, text=True,
                              cwd=cwd or self.ws, env=self.env(**env))

    def ok(self, *args, **env):
        r = self.run_niten(*args, **env)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r.stdout

    def start(self, *extra, **env):
        return self.ok("start", "--plan", self.plan, *extra, **env)

    def evidence(self, step, text="V-001: log entry 1 shows exit 0\n"):
        with open(os.path.join(self.state, "evidence", step + ".md"), "w") as f:
            f.write(text)

    def hook(self, kind, session="sess-1", **data):
        data.setdefault("session_id", session)
        data.setdefault("cwd", self.ws)
        r = self.run_niten("hook-" + kind, stdin=json.dumps(data))
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout) if r.stdout.strip() else None

    def pre(self, tool, session="sess-1", **tool_input):
        out = self.hook("pretooluse", session=session, tool_name=tool, tool_input=tool_input)
        return out["hookSpecificOutput"] if out else None

    def bash(self, command, description="", session="sess-1"):
        return self.pre("Bash", session=session, command=command, description=description)

    def state_json(self):
        with open(os.path.join(self.state, "state.json")) as f:
            return json.load(f)

    def commit(self, repo, name, text="x"):
        path = os.path.join(self.ws, repo)
        with open(os.path.join(path, name), "w") as f:
            f.write(text)
        git(path, "add", name)
        git(path, "commit", "-q", "-m", name)
        return git(path, "rev-parse", "HEAD")

    def approve_all_steps(self):
        for step in ("S-001", "S-002"):
            self.evidence(step)
            self.ok("review", step)


class StartTest(Base):
    def test_repositories_from_the_working_directory(self):
        out = self.start()
        self.assertIn("S-001: First step", out)
        st = self.state_json()
        self.assertEqual(st["repos"]["app"]["path"], os.path.join(self.ws, "app"))
        self.assertEqual(st["repos"]["infra"]["alias"], "repo-2")
        self.assertEqual(st["steps"][0]["start"]["app"], st["repos"]["app"]["base"])

    def test_repositories_from_flags_manifest_and_workspace(self):
        other = os.path.join(self.tmp, "elsewhere", "app2")
        os.makedirs(other)
        git(other, "init", "-q")
        git(other, "commit", "-q", "--allow-empty", "-m", "x")
        with open(os.path.join(self.plans, "DEMO-1.manifest.json"), "w") as f:
            json.dump({"repos": [{"id": "repo-2", "root": os.path.join(self.ws, "infra")}]}, f)
        self.ok("start", "--plan", self.plan, "--repo", "repo-1=" + other, cwd=self.tmp)
        st = self.state_json()
        self.assertEqual(st["repos"]["app"]["path"], other)
        self.assertEqual(st["repos"]["infra"]["path"], os.path.join(self.ws, "infra"))

    def test_missing_repository_and_second_start_are_refused(self):
        r = self.run_niten("start", "--plan", self.plan, cwd=self.tmp)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("repository app: not found", r.stderr)
        self.start()
        r = self.run_niten("start", "--plan", self.plan)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("already exists", r.stderr)

    def test_restart_archives_the_previous_session(self):
        self.start()
        self.evidence("S-001")
        out = self.start("--restart")
        self.assertIn("previous session archived", out)
        archived = [n for n in os.listdir(self.plans) if n.startswith("DEMO-1.niten.")]
        self.assertEqual(len(archived), 1)
        self.assertTrue(os.path.exists(os.path.join(self.plans, archived[0], "evidence", "S-001.md")))

    def test_an_unapproved_plan_needs_an_explicit_flag(self):
        os.remove(os.path.join(self.plans, "DEMO-1.approval.json"))
        r = self.run_niten("start", "--plan", self.plan)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not approved", r.stderr)
        self.start("--unapproved")
        self.assertTrue(self.state_json()["unapproved"])


class ReviewTest(Base):
    def setUp(self):
        super().setUp()
        self.start()

    def test_review_needs_evidence_order_and_a_clean_tree(self):
        r = self.run_niten("review", "S-001")
        self.assertIn("write the evidence first", r.stderr)
        self.assertNotEqual(r.returncode, 0)
        r = self.run_niten("review", "S-002")
        self.assertIn("S-001 is the current step", r.stderr)
        r = self.run_niten("final")
        self.assertIn("not approved yet", r.stderr)
        self.evidence("S-001")
        with open(os.path.join(self.ws, "app", "new.txt"), "w") as f:
            f.write("x")
        r = self.run_niten("review", "S-001")
        self.assertIn("commit the step's changes in app", r.stderr)

    def test_preexisting_changes_are_not_the_steps(self):
        with open(os.path.join(self.ws, "app", "untracked.txt"), "w") as f:
            f.write("x")
        self.start("--restart")
        self.evidence("S-001")
        self.ok("review", "S-001")
        with open(os.path.join(self.state, "reviews", "S-001-r1.prompt.md")) as f:
            self.assertIn("untracked.txt", f.read())

    def test_only_a_clean_approve_approves(self):
        self.evidence("S-001")
        for verdict in ("major", "unmet", "open"):
            out = self.ok("review", "S-001", FAKE_VERDICT=verdict)
            self.assertIn("CHANGES REQUESTED", out, verdict)
            self.assertEqual(self.state_json()["steps"][0]["status"], "changes_requested")
        r = self.run_niten("review", "S-001")
        self.assertIn("3 reviews without approval", r.stderr)
        out = self.ok("review", "S-001", "--user-approved", "one more review after the fix")
        self.assertIn("review 4 -> APPROVED", out)
        st = self.state_json()
        self.assertEqual(st["steps"][0]["status"], "approved")
        self.assertEqual(st["steps"][0]["reviews"][-1]["user_approved"], "one more review after the fix")
        with open(os.path.join(self.state, "reviews", "S-001-r4.prompt.md")) as f:
            prompt = f.read()
        self.assertIn("round 1 [major] f:1: wrong", prompt)  # every earlier finding is in the ledger

    def test_a_minor_finding_does_not_block_and_the_ledger_carries_it(self):
        self.evidence("S-001")
        out = self.ok("review", "S-001", FAKE_VERDICT="changes")
        self.assertIn("CHANGES REQUESTED", out)
        out = self.ok("review", "S-001", FAKE_VERDICT="open")
        self.assertIn("earlier finding not_addressed: style", out)
        self.assertIn("not judged: performance", out)
        self.assertIn("CHANGES REQUESTED", out)

    def test_reviewer_failure_leaves_the_step_open(self):
        self.evidence("S-001")
        for verdict, message in (("garbage", "no valid verdict"), ("badshape", "no valid verdict"),
                                 ("noledger", "no valid verdict"), ("fail", "reviewer failed")):
            r = self.run_niten("review", "S-001", FAKE_VERDICT=verdict)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn(message, r.stderr)
        self.assertEqual(self.state_json()["steps"][0]["status"], "pending")

    def test_the_reviewer_is_isolated_and_its_usage_recorded(self):
        self.evidence("S-001")
        self.ok("review", "S-001")
        with open(os.path.join(self.state, "reviews", "S-001-r1.json.argv")) as f:
            argv = f.read().splitlines()
        for flag in ("--ignore-user-config", "--ignore-rules", "mcp_servers={}", "plugins={}", "agents.enabled=false",
                     "read-only"):
            self.assertIn(flag, argv)
        review = self.state_json()["steps"][0]["reviews"][0]
        self.assertEqual(review["usage"], {"input_tokens": 5, "output_tokens": 2})
        self.assertIsNone(self.state_json()["reviewing"])

    def test_each_step_is_reviewed_against_its_own_start(self):
        first = self.commit("app", "one.txt")
        self.evidence("S-001")
        self.ok("review", "S-001")
        st = self.state_json()
        self.assertEqual(st["steps"][1]["start"]["app"], first)
        self.commit("app", "two.txt")
        self.evidence("S-002")
        self.ok("review", "S-002")
        with open(os.path.join(self.state, "reviews", "S-002-r1.prompt.md")) as f:
            prompt = f.read()
        self.assertIn(f"diff {first}`", prompt)
        self.assertIn("two.txt", prompt)
        this_step = prompt.split("THIS step's changes")[1].split("earlier steps")[0]
        self.assertNotIn("one.txt", this_step)
        self.assertIn("earlier steps, already approved", prompt)

    def test_a_changed_plan_stops_the_reviews(self):
        self.evidence("S-001")
        with open(self.plan, "a") as f:
            f.write("\n- weakened criterion\n")
        r = self.run_niten("review", "S-001")
        self.assertIn("the plan changed", r.stderr)
        self.assertIn("plan file changed", self.ok("status"))

    def test_what_nobody_could_verify_goes_to_the_user(self):
        self.approve_all_steps()
        self.evidence("final")
        out = self.ok("final", FAKE_VERDICT="unverifiable")
        self.assertIn("NEEDS THE USER", out)
        self.assertIn("R-002.C1", out)
        self.assertEqual(self.state_json()["final"]["status"], "needs_user")
        self.assertIsNone(self.hook("stop"))  # the turn ends so the user can check
        out = self.bash("git push", WHY)
        self.assertEqual(out["permissionDecision"], "deny")
        out = self.bash(f"python3 {SCRIPT} confirm 'checked the digest in the registry console'", WHY)
        self.assertEqual(out["permissionDecision"], "ask")
        self.ok("confirm", "checked the digest in the registry console")
        st = self.state_json()
        self.assertEqual(st["final"]["status"], "approved")
        self.assertEqual(self.bash("git push", WHY)["permissionDecision"], "ask")

    def test_full_session(self):
        self.approve_all_steps()
        r = self.run_niten("finish")
        self.assertIn("final review has not approved", r.stderr)
        r = self.run_niten("final")
        self.assertIn("final.md", r.stderr)
        self.evidence("final")
        self.assertIn("APPROVED", self.ok("final"))
        self.assertIn("next: delivery", self.ok("status"))
        self.ok("finish")
        with open(os.path.join(self.home, ".claude", "niten", "active.json")) as f:
            self.assertEqual(json.load(f), [])
        self.assertIsNone(self.bash("git push", WHY))  # the hooks are inert after finish


class HookTest(Base):
    def setUp(self):
        super().setUp()
        self.start()

    def test_stop_blocks_only_the_sessions_own_unapproved_step(self):
        out = self.hook("stop")
        self.assertEqual(out["decision"], "block")
        self.assertIn("S-001", out["reason"])
        self.assertIsNone(self.hook("stop", session="another"))

    def test_stop_lets_go_after_repeated_blocks_without_progress(self):
        for _ in range(3):
            self.assertEqual(self.hook("stop")["decision"], "block")
        out = self.hook("stop")
        self.assertNotIn("decision", out)
        self.assertIn("needs your attention", out["systemMessage"])
        self.assertEqual(self.hook("stop")["decision"], "block")  # the count starts again
        self.hook("posttooluse", tool_name="Bash", tool_input={"command": "make"}, tool_response={"stdout": ""})
        for _ in range(3):
            self.assertEqual(self.hook("stop")["decision"], "block")  # progress resets the count

    def test_stop_waits_for_a_review_running_in_the_background(self):
        st = self.state_json()
        sleeper = subprocess.Popen(["sleep", "30"])
        try:
            st["reviewing"] = {"pid": sleeper.pid, "scope": "S-001-r1", "since": "now"}
            with open(os.path.join(self.state, "state.json"), "w") as f:
                json.dump(st, f)
            self.assertIsNone(self.hook("stop"))
        finally:
            sleeper.kill()
            sleeper.wait()
        self.assertEqual(self.hook("stop")["decision"], "block")  # a dead review does not count

    def test_a_pause_lasts_until_the_user_answers(self):
        self.ok("pause", "need a cloud login")
        self.assertIsNone(self.hook("stop"))
        self.assertIsNone(self.hook("userprompt", session="another", prompt="hi"))
        out = self.hook("userprompt", prompt="done, logged in")
        self.assertIn("need a cloud login", out["hookSpecificOutput"]["additionalContext"])
        self.assertIsNone(self.state_json()["paused"])
        self.assertIsNotNone(self.hook("stop"))
        self.assertIsNone(self.hook("userprompt", prompt="next"))
        with open(os.path.join(self.state, "commands.jsonl")) as f:
            texts = [json.loads(l).get("text") for l in f]
        self.assertEqual(texts, ["done, logged in", "next"])  # the user's words are on record

    def test_delivery_needs_the_final_review_of_the_same_commits(self):
        out = self.bash("git -C app push origin HEAD", WHY)
        self.assertEqual(out["permissionDecision"], "deny")
        self.assertIn("final review", out["permissionDecisionReason"])
        self.assertIsNone(self.bash("git push", WHY, session="another"))
        self.approve_all_steps()
        self.assertIn("final review", self.hook("stop")["reason"])
        self.evidence("final")
        self.ok("final")
        out = self.bash("git -C app push origin HEAD", WHY)
        self.assertEqual(out["permissionDecision"], "ask")
        self.assertIn("final stage", out["permissionDecisionReason"])
        self.assertIsNone(self.hook("stop"))
        self.commit("app", "late.txt")
        out = self.bash("git -C app push origin HEAD", WHY)
        self.assertEqual(out["permissionDecision"], "deny")
        self.assertIn("changed after the final review", out["permissionDecisionReason"])

    def test_important_commands_are_put_to_the_user_with_an_explanation(self):
        for cmd in ("docker push registry/x:1", "aws --region r ecr batch-delete-image --repository-name x",
                    "aws s3 cp f s3://b/f", "terraform apply", "kubectl -n x delete pod y", "rm -rf build",
                    "curl -X POST https://api/x", "ssh host uptime",
                    "aws ecr get-login-password | docker login --password-stdin registry",
                    f"python3 {SCRIPT} finish --abort"):
            out = self.bash(cmd)
            self.assertEqual(out["permissionDecision"], "deny", cmd)
            self.assertIn("description", out["permissionDecisionReason"], cmd)
            self.assertEqual(self.bash(cmd, "push it")["permissionDecision"], "deny", cmd)
            out = self.bash(cmd, WHY)
            self.assertEqual(out["permissionDecision"], "ask", cmd)
            self.assertIn("S-001 (First step)", out["permissionDecisionReason"], cmd)
            self.assertIn(WHY, out["permissionDecisionReason"], cmd)
            self.assertIsNone(self.bash(cmd, WHY, session="another"), cmd)

    def test_read_only_commands_pass_without_a_question(self):
        for cmd in ("aws ecr describe-images --repository-name x", "aws --profile p ecr list-images --repository-name x",
                    "aws sts get-caller-identity", "aws s3 ls s3://b", "docker pull --platform linux/amd64 img",
                    "docker build -t x .", "docker buildx imagetools inspect img", "terraform plan", "kubectl get pods",
                    "git diff --stat", "curl -s https://api/x", "rm -f tmp.txt", "grep -rn pullrequests .",
                    f"python3 {SCRIPT} status", f"python3 {SCRIPT} review S-001"):
            self.assertIsNone(self.bash(cmd), cmd)

    def test_the_ask_list_can_be_extended(self):
        os.makedirs(os.path.join(self.home, ".claude", "niten"), exist_ok=True)
        with open(os.path.join(self.home, ".claude", "niten", "config.json"), "w") as f:
            json.dump({"ask": [r"\bmake\s+deploy\b"]}, f)
        out = self.bash("make deploy", "Deploy the change to the test environment as the step requires")
        self.assertEqual(out["permissionDecision"], "ask")
        self.assertIn("ask list", out["permissionDecisionReason"])

    def test_the_record_the_plan_and_the_settings_are_not_edited_by_hand(self):
        state = os.path.join(self.state, "state.json")
        for path in (state, os.path.join(self.state, "commands.jsonl"), self.plan,
                     os.path.join(self.plans, "DEMO-1.approval.json"),
                     os.path.join(self.home, ".claude", "niten", "active.json"),
                     os.path.join(self.home, ".claude", "settings.json")):
            self.assertEqual(self.pre("Write", file_path=path, content="{}")["permissionDecision"], "deny", path)
            self.assertEqual(self.pre("Edit", file_path=path)["permissionDecision"], "deny", path)
        self.assertIsNone(self.pre("Write", file_path=os.path.join(self.state, "evidence", "S-001.md")))
        for cmd in (f"echo '{{}}' > {state}", f"cd {self.state} && python3 -c 'open(\"state.json\",\"w\")'",
                    f"sed -i '' s/pending/approved/ {state}", f"cp /tmp/x {self.plan}"):
            self.assertEqual(self.bash(cmd, WHY)["permissionDecision"], "deny", cmd)
        for cmd in ("echo '{}' > ~/.claude/settings.json", "sed -i '' s/a/b/ $HOME/.claude/settings.local.json"):
            self.assertEqual(self.bash(cmd, WHY)["permissionDecision"], "deny", cmd)
        for cmd in (f"cat {state}", f"cat > {self.state}/evidence/S-001.md <<'EOF'\nok\nEOF",
                    f"python3 {SCRIPT} pause 'need a login'",
                    "sed -i '' s/a/b/ app/config/settings.json", "echo '{}' > app/state.json"):
            self.assertIsNone(self.bash(cmd), cmd)

    def test_writes_outside_the_repositories_and_mcp_changes_are_asked(self):
        self.assertIsNone(self.pre("Write", file_path=os.path.join(self.ws, "app", "Dockerfile")))
        self.assertIsNone(self.pre("Write", file_path="/tmp/niten-scratch.txt"))
        out = self.pre("Write", file_path=os.path.join(self.home, ".aws", "config"))
        self.assertEqual(out["permissionDecision"], "ask")
        out = self.pre("mcp__tracker__create_issue", summary="Deploy failed", token="abc123secretvalue")
        self.assertEqual(out["permissionDecision"], "ask")
        self.assertNotIn("abc123secretvalue", out["permissionDecisionReason"])
        self.assertIsNone(self.pre("mcp__tracker__get_issue", key="X-1"))
        self.assertIsNone(self.pre("Read", file_path=self.plan))

    def test_commands_are_logged_with_secrets_masked(self):
        self.hook("posttooluse", tool_name="Bash",
                  tool_input={"command": "docker push reg/x:1", "description": WHY},
                  tool_response={"stdout": "digest: sha256:abc", "stderr": "", "exit_code": 0})
        self.hook("posttooluse", tool_name="Bash", tool_input={"command": "aws ecr get-login-password"},
                  tool_response={"stdout": "A" * 200, "stderr": "", "exit_code": 0})
        self.hook("posttooluse", tool_name="Bash", tool_input={"command": "env"},
                  tool_response={"stdout": "AWS_KEY=AKIAABCDEFGHIJKLMNOP password=hunter2", "stderr": ""})
        self.hook("posttooluse", session="another", tool_name="Bash", tool_input={"command": "ls"},
                  tool_response={"stdout": "x"})
        self.hook("posttooluse", hook_event_name="PostToolUseFailure", tool_name="Bash",
                  tool_input={"command": "pytest"}, error="Exit code 1: 2 failed")
        self.hook("posttooluse", tool_name="AskUserQuestion", tool_input={"questions": [{"question": "Reuse repo?"}]},
                  tool_response={"answers": {"Reuse repo?": "Yes"}})
        with open(os.path.join(self.state, "commands.jsonl")) as f:
            entries = [json.loads(l) for l in f]
        self.assertEqual(len(entries), 5)
        self.assertEqual(entries[3]["exit_code"], "failed")
        self.assertIn("2 failed", entries[3]["stderr"])
        self.assertEqual(entries[4]["kind"], "question")
        self.assertIn("Yes", entries[4]["answers"])
        self.assertEqual(entries[0]["step"], "S-001")
        self.assertEqual(entries[0]["exit_code"], 0)
        self.assertIn("sha256:abc", entries[0]["stdout"])
        self.assertNotIn("AAAA", entries[1]["stdout"])
        self.assertNotIn("AKIAABCDEFGHIJKLMNOP", entries[2]["stdout"])
        self.assertNotIn("hunter2", entries[2]["stdout"])
        self.evidence("S-001")
        self.ok("review", "S-001")
        with open(os.path.join(self.state, "reviews", "S-001-r1.prompt.md")) as f:
            self.assertIn("5 command(s)", f.read())

    def test_hooks_never_fail_on_bad_input(self):
        for kind in ("stop", "pretooluse", "posttooluse", "userprompt"):  # PostToolUseFailure shares posttooluse
            r = self.run_niten("hook-" + kind, stdin="not json")
            self.assertEqual(r.returncode, 0)
            self.assertEqual(r.stdout, "")

    def test_attach_moves_the_session_to_a_new_claude_session(self):
        self.assertIsNone(self.hook("stop", session="sess-2"))
        self.ok("attach", "--state", self.state, CLAUDE_CODE_SESSION_ID="sess-2")
        self.assertIsNotNone(self.hook("stop", session="sess-2"))
        self.assertIsNone(self.hook("stop", session="sess-1"))


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.home = os.path.realpath(tempfile.mkdtemp())
        self.settings = os.path.join(self.home, ".claude", "settings.json")
        os.makedirs(os.path.dirname(self.settings))
        self.original = {"model": "x", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "other"}]}]}}
        with open(self.settings, "w") as f:
            json.dump(self.original, f)

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
        commands = {e: [h["command"] for g in groups for h in g["hooks"]] for e, groups in data["hooks"].items()}
        self.assertIn("other", commands["Stop"])
        for event, sub in (("Stop", "hook-stop"), ("PreToolUse", "hook-pretooluse"),
                           ("PostToolUse", "hook-posttooluse"), ("PostToolUseFailure", "hook-posttooluse"),
                           ("UserPromptSubmit", "hook-userprompt")):
            self.assertTrue(any(c.endswith(sub) and "niten.py" in c for c in commands[event]), event)
        self.assertEqual(data["hooks"]["PreToolUse"][0]["matcher"], "*")
        self.assertEqual(data["hooks"]["PostToolUse"][0]["matcher"], "Bash|AskUserQuestion")
        self.assertIn("already up to date", self.sh())
        self.assertEqual(self.load(), data)
        self.sh("uninstall")
        self.assertFalse(os.path.lexists(link))
        self.assertEqual(self.load(), self.original)


if __name__ == "__main__":
    unittest.main()
