#!/bin/sh
# Install or remove the Niten skill for Claude Code.
#
#   ./install.sh            link skills/niten into ~/.claude/skills/niten and register the hooks
#   ./install.sh uninstall  remove the link and the hooks
#
# The skill is linked, not copied, so pulling this repository updates it. The hooks
# (Stop, PreToolUse, PostToolUse, PostToolUseFailure, UserPromptSubmit) go into
# ~/.claude/settings.json; a backup is written before any change. They do nothing unless a Niten session is active
# in the Claude Code session that triggers them.
# CLAUDE_CONFIG_DIR is honoured in place of ~/.claude.
set -eu

repo=$(cd "$(dirname "$0")" && pwd -P)
config=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
link=$config/skills/niten
script=$link/scripts/niten.py
action=${1:-install}

command -v python3 >/dev/null || { echo "install: python3 is required" >&2; exit 1; }
command -v git >/dev/null || { echo "install: git is required" >&2; exit 1; }

hooks() { # hooks add|remove
	python3 - "$config/settings.json" "$script" "$1" <<'EOF'
import json, os, shutil, sys, time
path, script, mode = sys.argv[1:4]
data = {}
if os.path.exists(path):
    with open(path) as f:
        data = json.load(f)
before = json.dumps(data, sort_keys=True)
hooks = data.setdefault("hooks", {})
mark = "/skills/niten/scripts/niten.py"

def strip(event):
    groups = []
    for g in hooks.get(event, []):
        g = dict(g)
        g["hooks"] = [h for h in g.get("hooks", []) if mark not in h.get("command", "")]
        if g["hooks"]:
            groups.append(g)
    if groups:
        hooks[event] = groups
    else:
        hooks.pop(event, None)

events = {  # event: (matcher, niten.py command)
    "Stop": (None, "hook-stop"),
    "PreToolUse": ("*", "hook-pretooluse"),
    "PostToolUse": ("Bash|AskUserQuestion", "hook-posttooluse"),
    "PostToolUseFailure": ("Bash|AskUserQuestion", "hook-posttooluse"),
    "UserPromptSubmit": (None, "hook-userprompt"),
}
for event in events:
    strip(event)
if mode == "add":
    for event, (matcher, sub) in events.items():
        group = {"hooks": [{"type": "command", "command": 'python3 "%s" %s' % (script, sub), "timeout": 10}]}
        if matcher:
            group = {"matcher": matcher, **group}
        hooks.setdefault(event, []).append(group)
if not hooks:
    data.pop("hooks", None)
if json.dumps(data, sort_keys=True) == before:
    print("hooks: already up to date in " + path)
    sys.exit(0)
if os.path.exists(path):
    backup = path + ".bak-niten-" + time.strftime("%Y%m%d%H%M%S")
    shutil.copy2(path, backup)
    print("hooks: backup " + backup)
os.makedirs(os.path.dirname(path), exist_ok=True)
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
os.replace(tmp, path)
print("hooks: " + ("registered in " if mode == "add" else "removed from ") + path)
EOF
}

case $action in
install)
	mkdir -p "$config/skills"
	if [ -L "$link" ]; then
		rm "$link"
	elif [ -e "$link" ]; then
		echo "install: $link exists and is not a link; move it away first" >&2
		exit 1
	fi
	ln -s "$repo/skills/niten" "$link"
	echo "skill: $link -> $repo/skills/niten"
	hooks add
	echo "Start a new Claude Code session to pick up the skill and the hooks."
	;;
uninstall)
	if [ -L "$link" ]; then
		rm "$link"
		echo "skill: removed $link"
	fi
	hooks remove
	;;
*)
	echo "usage: $0 [install|uninstall]" >&2
	exit 2
	;;
esac
