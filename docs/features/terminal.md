# Integrated Terminal & Harness Sessions

px0 has a terminal pane under the editor. It runs your shell, or your coding harness interactively, in the workspace, so you can review code and talk to the agent changing it without leaving the browser.

---

## Overview & Core Purpose

The inline composer (`Alt+E`, see [Editing with Coding Agents](agent-editing.md)) starts a fresh, one-shot harness run for every instruction. That is quick for a single fix, but the harness forgets everything between runs and cannot ask you anything.

A **Harness Session** is the harness's own interactive interface (Claude Code, Gemini CLI, Codex, …) running inside px0. It keeps its conversation across instructions, can ask clarifying questions, and shows its work. While one is open, `Alt+E` on a selection sends that selection's reference (`path:line`) straight into the session. You type the instruction there, and the files it changes reload in place above the pane.

A **Shell Session** is your login shell in the workspace, for `go test`, `git log` and the rest. In PR review, both kinds start in the PR's checkout.

---

## How to Use It

1. Press **``Ctrl+` ``** (or click **Terminal** in the status bar) to open the pane.
2. Click **+ claude** (it shows the harness picked in the footer) to start a Harness Session, or **+ Shell** for a shell. You can run up to 4 sessions, shown as tabs.
3. Select code in the source or diff view and press **`Alt+E`**. The reference is pasted into the harness session and the keyboard moves there. Nothing is sent until you press Enter.
4. Type your instruction and press Enter. As the harness edits files, open tabs, git badges and diffs update in place.

Sessions belong to the px0 process, not the page. Reloading the page, or opening px0 in a second tab, shows the same sessions with their recent output. They end when you close their tab (`×`) or stop px0.

---

## Keyboard

| Shortcut | Where | Action |
| :--- | :--- | :--- |
| ``Ctrl+` `` | Anywhere | Show or hide the terminal pane |
| `Alt+E` | Code selected | Send the selection's reference to the harness session, or open the inline composer when none is running |
| `Ctrl+Shift+C` / `Ctrl+Shift+V` | Terminal (Linux, Windows) | Copy / paste |
| `Cmd+C` / `Cmd+V` | Terminal (macOS) | Copy / paste |

Keys typed in the terminal go to the program running there: `Ctrl+W`, `Ctrl+D`, `Ctrl+F`, `Escape` and the rest do not trigger px0 shortcuts. On a Mac, `Cmd` shortcuts still work. The Command Palette also has **Terminal: Toggle**, **Terminal: New Harness Session** and **Terminal: New Shell**.

---

## Security

The terminal runs programs as you, so px0 only offers it to your own machine:

- It is available only when px0 is bound to loopback, which is the default `-host 127.0.0.1`. With `-host 0.0.0.0` (or in the Docker image) the pane explains that it is off.
- The URL px0 opens and prints carries a one-time access token, which your browser swaps for a cookie and removes from the address bar. A page opened some other way, such as a bookmark or another program on the machine, cannot use the terminal. If that happens, open the printed URL again.

---

## Configuration

- **Integrated Terminal** (`terminal.enabled`, default `true`): offer the terminal pane. It takes effect on the next start.
- **CLI flag** `px0 -no-terminal`: turn the terminal off for one run.

The pane's height is remembered per browser. The Harness Session uses the harness and model picked in the footer or the git panel, the same choice the inline composer uses.

Supported on Linux and macOS.

For the design (pseudo-terminals without CGO, the multiplexed stream, access control), see [Integrated Terminal Internals](../internals/terminal.md).
