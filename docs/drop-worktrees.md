# Dropping worktree management from agentd

Status: accepted 2026-09-28. All five stages are implemented on branch
`go-port`.

## Decision

`agentd` stops owning git worktrees. A session has a working directory (`cwd`)
and a command. Whether that directory is a git worktree, a plain checkout, or
not a repository at all is the caller's business.

Worktree creation, branch naming, merge, discard, and cleanup move out of the
daemon and into the layer that starts the agent: the user, a shell wrapper, a
skill, or instructions given to the coding agent itself.

## Why

- The product is durable, reconnectable, observable agent sessions. Worktree
  lifecycle is the part of the codebase furthest from that and carries the most
  policy: branch naming, `IntegrationPolicy`, `ApplyState`, merge preview,
  auto-commit on exit, dirty/ahead counts. None of it needs to exist in Go.
- Git is currently mandatory. Session creation resolves a repo root and fails
  otherwise, so a shell or an agent in a scratch directory cannot be supervised.
- The remote story gets simpler. `agent --host devbox run --cwd /srv/repo` needs
  the daemon to know a directory, not to manage git state on the remote box.
- Coding agents already create worktrees well when asked. Repo-specific
  conventions belong in a skill, not guessed by the daemon.

## What is removed

Daemon behaviour (Rust today, not to be ported to Go):

- `git::create_worktree`, `remove_worktree`, `preview_merge`, `commit_all`,
  auto-commit and "mergeable" finalisation on session exit
- `App::create_worktree`, `cleanup_worktree`, `apply_session`,
  `discard_session`, `diff_session`
- `refresh_commit_state` (dirty/ahead counts on `ls`)
- `<runtime-root>/worktrees/`

Protocol (Rust v32 has these; Go v33 will not):

| kind | message |
|------|---------|
| 4 | CreateWorktreeRequest |
| 5 | CleanupWorktreeRequest |
| 19 | ApplySessionRequest |
| 20 | DiscardSessionRequest |
| 10 | DiffSessionRequest |
| 106 | WorktreeResponse |
| 107 | DiffResponse |

Session record fields: `repo_path`, `repo_name`, `base_branch`, `branch`,
`worktree`, `integration_policy`, `integration_state` / `apply_state`,
`dirty_count`, `ahead_count`, `has_commits`, `has_pending_changes`.
`SessionEnded` loses `apply_state`, `has_commits`, `branch`, `worktree`.

CLI commands: `merge`, `accept`, `discard`, `worktree create|cleanup`, `diff`.
`ls` and `status` lose the branch, dirty and ahead columns.

Environment injected into the agent: `AGENTD_WORKTREE`, `AGENTD_BRANCH`.

## What replaces it

- `session.Record.Cwd`. `agent new` accepts `--cwd DIR` (the CLI has no `agent run`),
  defaulting to the caller's current directory. The default session name is
  derived from the basename of `cwd` instead of the repo name.
- Session names are unique per daemon. The branch-existence check in
  `unique_session_id` goes away.
- `AGENTD_CWD` is injected. `AGENTD_WORKSPACE` is kept as an alias with the
  same value for one release so existing agent instructions keep working.
- Worktree recipe, documented in README:

  ```sh
  git worktree add -b agent/auth-refactor ../wt/auth-refactor main
  agent new --cwd ../wt/auth-refactor auth-refactor
  ```

  A skill or wrapper can package this. `agentd` does not.

## What is deliberately kept as a seam

`agent diff` and "does this session have uncommitted work" are part of the
attention story and do not need ownership. A later, read-only observation
feature can run `git status --porcelain` or `git diff` in `cwd` when `cwd` is
inside a repository. That is why `cwd` is a first-class column rather than
something derived from a path. The `DiffSessionRequest` kind number is left
unused so it can be reintroduced with a `cwd`-based meaning later. Nothing
here is implemented in this plan.

## Sequencing

The Go worker currently runs under the Rust daemon at protocol v32 via
`AGENTD_WORKER_BIN`. Changing the session record or `SessionEnded` breaks that
bridge. The bridge is a development stepping stone, so the removal is done as
the first commit of the Go daemon milestone and the bridge is retired with it.
The Rust daemon is left untouched as the behavioural reference.

### Stage 1. Docs and roadmap (done)

- ROADMAP.md Phase 1: drop "Port worktree lifecycle management", remove
  `worktrees` from the Go daemon box, note the decision.
- ARCHITECTURE.md: session creation no longer allocates a branch or worktree;
  remove `worktrees/` from the runtime root layout.
- README.md: replace "its own git worktree and branch" with the recipe above,
  update the injected environment list, remove `agent diff`/`merge`/`discard`
  from the capabilities list.
- CLAUDE.md: remove `worktrees` from the Go package sketch and the "preserve
  and strengthen" list, keep `diffs` and `artifacts`.

### Stage 2. Go data model and protocol (v33) (done)

- `go/internal/session`: add `Cwd`, delete the git fields, `WorktreeRecord`,
  `Diff`, `IntegrationPolicy`, `ApplyState`, `BranchNameFromSessionID`.
- `go/internal/protocol`: bump to v33, delete the seven kinds above and the
  matching `Request`/`Response` fields, add `Cwd` to `CreateSession`, shrink
  `SessionEnded`. Update `protocol_test.go` fixtures.
- `go/internal/db`: new schema version. Migration copies `worktree` into `cwd`
  when the row has one, else `workspace`, and drops the git columns.
- `go/internal/paths`: remove `WorktreesDir` and `WorktreePath`.
- `go/internal/worker` and `cmd/agentd session-worker`: replace
  `--repo-root/--worktree/--branch` with `--cwd`; set `cmd.Dir = cwd`; inject
  `AGENTD_CWD` and the `AGENTD_WORKSPACE` alias. Update `worker_test.go`.
- Rust daemon stops being able to spawn the Go worker: the worker rejects the
  old flags and exits 2, which the Rust daemon records as a failed session.
  The Rust side is left untouched.

### Stage 3. Go daemon `agentd serve` (done)

Already the next milestone. With Stage 2 done, `create_session` is: validate
`cwd` exists, insert record, spawn worker. No git binary required. This stage
is where `run`, `ls`, `attach`, `kill` land in Go.

### Stage 4. Rust `agent` CLI on v33 (done)

- Remove `Merge`, `Accept`, `Discard`, `Worktree`, `Diff` commands.
- Add `--cwd` to `New`/`Run`.
- `session_display.rs`: drop branch, dirty, ahead columns and styles.
- `local.rs` fallback mode: drop `remove_worktree_if_present` and
  `worktree_dirty_count`, or delete local mode entirely if the Go daemon makes
  it unnecessary. Decide when Stage 3 is stable.
- Bump the CLI's protocol expectation to v33. The Rust daemon is no longer a
  valid peer for the CLI after this point.

### Stage 5. Retire Rust daemon code (existing Stage G) (done)

`git.rs`, the worktree paths in `app.rs`, `session_worker.rs`, and the 22
`app.rs` unit tests that exercise merge/discard go with it. Not before
Stage 4 is shipped and the Go daemon passes the attach/detach/reattach suite.

Done by deleting the whole `crates/agentd` crate, the `vendor/ghostty`
submodule (the Go worker builds its own pinned libghostty-vt), the
`third_party/libghostty-vt*` Rust bindings and their `[patch.crates-io]`
entries, `.cargo/config.toml`, and the workspace dependencies only the Rust
daemon used.

## Tests

Removed with the feature: `app.rs` tests for apply, discard, mergeable
finalisation, `refresh_commit_state`; `git.rs` tests; CLI tests for `merge`,
`discard`, `worktree`, `diff`.

Added or changed:

- `agent new --cwd` in a non-git directory succeeds.
- `agent new` with a missing `cwd` fails before a worker is spawned and the
  record ends in the failed state.
- Worker test asserts `AGENTD_CWD` and `cmd.Dir`.
- Protocol fixture test asserts v32 frames for the removed kinds are rejected
  as unknown kinds, not silently accepted.
- DB migration test: a v7 row with a worktree path migrates to `cwd`.

## Risks

- Two agents started in the same checkout will now step on each other. This is
  the user's responsibility and must be said plainly in the README.
- Existing users of `agent merge` lose the auto-commit and fast-forward
  behaviour. They get `git` instead. No compatibility shim.
- Agent instructions that read `AGENTD_BRANCH` break. Only `AGENTD_WORKSPACE`
  is aliased.
- The Rust daemon and Rust CLI diverge in protocol version between Stage 2 and
  Stage 4. Keep that window short; do not release from it.

## Deferred

- Read-only git observation over `cwd` (`agent diff`, dirty indicator in `ls`).
- Any `agent run --worktree` sugar in the CLI. If it comes back it is a CLI
  convenience that calls `git worktree add` then `run --cwd`, never daemon
  state.
