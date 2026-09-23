# px0

A browser-based code reader optimized for reviewing changes made by humans and AI coding agents. px0 reads; edits are made by a harness or a person, never authored by px0 itself.

## Language

### Workspaces

**Workspace**:
The directory px0 serves and indexes: the argument given on the command line, or the temporary checkout of a pull request.
_Avoid_: project, root folder

**PR Checkout**:
A process-scoped worktree of a pull request's head, created at launch and removed when px0 exits.
_Avoid_: PR clone, PR folder

### Handing work to agents

**Harness**:
A coding agent CLI already installed on the machine (Claude Code, Gemini CLI, Codex, …) that px0 launches to make changes.
_Avoid_: agent (ambiguous), tool, bot

**Headless Dispatch**:
Launching a harness non-interactively with a single composed prompt anchored to a line range, then reloading what it changed once it exits. Documented publicly as "Mode 1".
_Avoid_: agent edit (when the distinction from a Harness Session matters), inline edit

**Sidecar**:
Running a harness in a terminal outside px0 while px0 watches the same Workspace. Documented publicly as "Mode 2".
_Avoid_: external mode, supervision mode

**Terminal Session**:
An interactive pseudo-terminal hosted by the px0 process and shown in the browser, running in the Workspace.
_Avoid_: console, shell tab

**Harness Session**:
A Terminal Session started to run the selected Harness interactively. Unlike a Headless Dispatch it keeps its conversation across many instructions.
_Avoid_: agent chat, live agent

**Shell Session**:
A Terminal Session running the user's login shell rather than a Harness.

**Reference**:
A `path:L1-L2` pointer to lines in the Workspace, the form produced by Copy Ref and handed to a Harness Session.
_Avoid_: link, anchor (in user-facing text)
