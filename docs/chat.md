# `shepherd chat`: the shell escape

`shepherd chat` is the terminal front desk. Besides talking to the desk and running slash
commands (`/help` lists them), a line that starts with `!` runs as a shell command.

## What it does

| You type | What runs |
| --- | --- |
| `!<command>` | `<command>` in the workspace root |
| `!@<lane> <command>` | `<command>` in that open lane's worktree |
| `!@<repo>/<lane> <command>` | the same, when the lane name is used in more than one repo |

- **Who runs it:** the chat client itself, on your machine, with your shell (`$SHELL -c`,
  `/bin/sh` if it is unset; `cmd /C` on Windows). It is never sent to the desk. The desk
  still has no shell: it runs with `--disallowedTools Edit Write Bash`, and this does not
  change that.
- **Lanes:** the lane is looked up through the daemon among the workspace's open lanes.
  An unknown or closed lane is an error and nothing runs; an ambiguous name lists the
  `repo/lane` forms to use.
- **Output:** stdout, stderr and the exit code appear in the thread as a `$` entry, drawn
  differently from the desk's and agents' messages. A running command's latest output shows
  above the input. Output is redacted like other displayed text. The thread shows the
  last 30 lines; the whole output (up to 1 MiB) is in the Ctrl-O transcript.
- **Ctrl-C** stops the running command (and what it started), not the chat. With nothing
  running, Ctrl-C quits as before.
- **The desk is told:** the command, its directory, its exit code and the tail of its
  output (the last 20 lines, redacted) are put at the top of **your next message to the
  desk**, as context marked as something the person ran, so the desk can act on the
  result. A command does not start a desk turn of its own. Up to the last five commands are
  carried; each is sent once.

## What it does not do

- **Interactive programs** (editors, pagers, anything that reads the terminal) are not
  supported. The command gets no stdin, and runs with `PAGER=cat` and `GIT_PAGER=cat`.
- One command runs at a time; a second `!` while one runs is refused.
- Nothing here is in the web UI.
