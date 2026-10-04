# Recorded PTY streams

Raw output of real programs, recorded under a real PTY at 120x40, gzip-compressed. The worker's
tests replay them into the shadow terminal, cut at many points (inside escape sequences and
UTF-8 characters too), and check that a terminal restored from the reattach snapshot matches
(`TestSnapshotRestoresRecordedStreams`). They also seed `FuzzSnapshotRestore` and the snapshot
benchmarks.

| Stream | Program | What it does |
|---|---|---|
| `claude.vt.gz` | Claude Code 2.1 | Starts in a fresh folder, opens `/help`, closes it, quits with Ctrl-C twice. Alternate screen, synchronized output, truecolor, kitty keyboard queries. Nothing is sent to the model. |
| `codex.vt.gz` | Codex 0.160 | Starts, opens the slash command popup, the `/model` picker, closes it and quits with Ctrl-C. Alternate screen, synchronized output. Nothing is sent to the model. |
| `shell.vt.gz` | bash 3.2, `--norc` | A colored prompt, `ls -G`, `git log --graph --color` on a small repository with a merge, SGR styles, line editing with Ctrl-U, arrows and backspace, history recall, 60 lines of output, a wrapping line, Ctrl-L, `exit`. |
| `vim.vt.gz` | Vim 9.1, `--clean` | Opens a Go file, jumps to the end and back, searches, inserts a line, scrolls with Ctrl-D/Ctrl-U, `:split`, `:vsplit`, moves between windows, `:qa!`. Alternate screen, scrolling regions. |
| `nvim.vt.gz` | Neovim 0.12, `--clean` | The same as Vim. Truecolor, synchronized output. |
| `go-test.vt.gz` | `go test -v` | A throwaway module with passing, failing, skipped and panicking tests and subtests. |
| `clang.vt.gz` | Apple clang 21 | `-fsyntax-only -fcolor-diagnostics` on a generated C file: 750 errors and 500 warnings with notes, about 500 KB of colored diagnostics. |

## Re-recording

`record.go` in the parent directory records them. From the repository root:

```sh
go run internal/worker/testdata/record.go                 # all of them
go run internal/worker/testdata/record.go -only vim -v    # one, printing the screen after each step
```

It needs libghostty-vt (`PKG_CONFIG_PATH` as in the `Makefile`), and the programs on `PATH`.
Each program runs in a scratch directory, `/tmp/agentd-demo`, which is created and removed for each
recording. The recorder feeds the output through libghostty-vt as the worker does, so terminal
queries are answered and each scripted step can wait for text on the screen; keys are typed one at
a time. The shell, editors and clang get a minimal environment with `HOME` set to the scratch
directory, and the git history uses a fixed demo identity and date.

Claude Code and Codex run with the recording user's real home directory (they need their
settings and login), but only `HOME`, `USER`, `LOGNAME`, `PATH`, `SHELL`, `TMPDIR` and terminal
variables from the environment. Codex's update check is turned off (left on, it offers to upgrade
itself with Homebrew, and the scripted Enter accepts) and Claude Code's auto-updater is disabled.
Claude Code asks whether to trust the scratch directory on its first run there, which the script
accepts; that records the directory in its settings.

## Privacy

The recorder replaces identifying strings with placeholders of the same length (letters become
`x`, digits `0`), so cursor positions and wrapping are unchanged: the user's name, login and home
directory, the host name, the global git name and email, any `-scrub` strings, and UUIDs. It then
refuses to write a stream that still contains one of them. Before committing a re-recorded stream,
also look through it yourself, for example:

```sh
gunzip -c internal/worker/testdata/streams/claude.vt.gz | grep -a -o -i -E '.{0,30}(@|/Users/|/home/|token|account).{0,30}'
```

Depending on how it is logged in, Claude Code's welcome screen can show the account's email and
organization; these recordings show only the billing type. `/status` and Codex's `/status` show
account details, so the scripts stay away from them.
