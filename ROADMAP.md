# Roadmap

This roadmap is based on the current codebase and the gaps between the implemented runtime and the product described in `README.md` and `ARCHITECTURE.md`.

## Principles

- Prioritize closing doc/code mismatches quickly.
- Build on the current session runtime instead of replacing it.
- Add new product concepts only when they have a storage model, protocol surface, and CLI story.
- Keep the daemon reliable first; richer supervision features come after that.

## Phase 0: Correctness And Documentation

These are the highest-leverage items because they reduce confusion immediately and make the current project easier to use.

### 1. Align the docs with the actual CLI and data model

Current implementation is session-based, but the docs describe a task-based product with commands that do not exist yet.

Work:

- Update `README.md` examples from `agent run` / `agent ls` to current commands, or mark them as planned.
- Remove or clearly mark the no-args TUI as not implemented.
- Rename task-oriented language to session-oriented language where appropriate until the task model exists.
- Fix `agent send` vs `agent send-input` naming inconsistency.

Done when:

- A new user can follow the README without hitting missing commands or missing UI.

### 2. Align path documentation with actual runtime behavior

`ARCHITECTURE.md` says socket placement depends on environment variables, but the code currently uses `~/.agentd`.

Work:

- Either implement configurable runtime path discovery, or simplify the docs to match current behavior.
- Document exactly what lives under `~/.agentd/`.

Done when:

- The documented filesystem layout matches `AppPaths::discover()`.

### 3. Make current limitations explicit

The daemon loses live PTY reattach/send capability after restart. This is already partially documented and should be surfaced more clearly.

Work:

- Document restart behavior in README command sections, not only in the limitations section.
- Clarify degraded/local mode behavior in the CLI docs.

Done when:

- Users understand which commands require a live compatible daemon and which commands still work from persisted state.

## Phase 1: Harden The Existing Session Runtime

Before adding bigger product concepts, the current session runtime should be made more complete and easier to supervise.

### 4. Improve session inspection UX

Current `sessions` output is minimal and does not match the supervision experience described in the README.

Work:

- Expand `agent sessions` output with task text, timestamps, exit status, and worktree info.
- Add optional machine-readable output for scripting.
- Add filtering for running, failed, and recovered sessions.

Done when:

- Users can supervise several sessions from the CLI without opening `status` for each one.

### 5. Improve event structure

Events exist, but they are generic JSON records without stronger product semantics.

Work:

- Define daemon event types formally.
- Add structured fields for level, source, category, and summary.
- Document which events are emitted automatically by the daemon.

Done when:

- Event consumers can build consistent UIs without guessing at payload shapes.

### 6. Strengthen lifecycle recovery behavior

Current restart handling marks running sessions as `unknown_recovered` if the process is gone, but runtime state is otherwise in-memory only.

Work:

- Audit all lifecycle transitions for create, fail, exit, kill, and restart.
- Add tests for daemon restart, orphaned processes, and stale metadata.
- Decide whether `unknown_recovered` is a terminal state or a temporary reconciliation result.

Done when:

- Session state transitions are predictable and tested.

## Phase 2: Close The Product/CLI Gap

This phase makes the user-facing interface match the intended product pitch.

### 7. Add a task-friendly CLI layer

The current CLI exposes raw session primitives. The README describes a simpler interface.

Work:

- Introduce `agent run` as a user-facing alias or replacement for `create`.
- Introduce `agent ls` as a user-facing alias or replacement for `sessions`.
- Decide whether “task name” is separate from prompt text.
- Add a first-class detach command only if it materially improves UX over `Ctrl-]`.

Done when:

- The README examples map directly to real commands.

### 8. Add a basic no-args dashboard or remove that promise

The README currently promises a TUI when `agent` is run without arguments. There is no TUI code in the repo.

Work:

- Decide whether the project should ship a TUI in the near term.
- If yes, build a minimal dashboard for listing sessions, opening logs, and attaching.
- If no, remove the claim and keep the CLI focused.

Done when:

- The no-args behavior is intentional and documented.

## Phase 3: Introduce Real Product Concepts

This is where the codebase moves from a durable PTY/session runtime to the fuller supervision model described in the README.

### 9. Add a first-class Task model

Right now “task” is only a string stored on `SessionRecord`.

Work:

- Create a `tasks` table and shared types.
- Define task lifecycle separately from session lifecycle.
- Decide whether one task owns many sessions or one primary session plus spawned sub-agents.

Done when:

- Tasks are identifiable objects with metadata, status, and relationships to sessions.

### 10. Add Thread tracking

Threads are described in the README but do not exist in protocol or storage.

Work:

- Define thread records and relationships to tasks/sessions.
- Store prompts, tool calls, intermediate outputs, and final results in a queryable form.
- Decide what comes from daemon instrumentation vs agent-side instrumentation.

Done when:

- The system can reconstruct an agent reasoning timeline beyond raw PTY logs.

### 11. Add Artifact storage and indexing

Artifacts are currently implied, not modeled.

Work:

- Define artifact types such as commit, patch, file, test result, and screenshot.
- Add durable artifact metadata and filesystem storage conventions.
- Expose artifact listing and retrieval in the protocol and CLI.

Done when:

- Users can inspect outputs without scraping logs or worktrees manually.

## Phase 4: Attention And Supervision

This phase implements the main product differentiator described in the README.

### 12. Add an attention model

The README describes `info`, `notice`, and `action`, but the code has no attention state.

Work:

- Define attention levels and transitions.
- Allow daemon events and agent-appended events to raise attention.
- Add APIs to list sessions/tasks ordered by attention.
- Add CLI affordances for “needs attention now”.

Done when:

- Users can supervise by exception instead of tailing logs.

### 13. Add summaries for supervision

Events are too low-level on their own.

Work:

- Add concise rolling summaries per session/task.
- Surface latest meaningful activity, blocked reasons, and last user action required.

Done when:

- A user can understand current state without opening full logs.

## Phase 5: Multi-Agent Orchestration

The README describes tasks that may spawn one or more agents. The current daemon launches one configured agent per session.

### 14. Add task-to-session relationships

Work:

- Decide whether spawned agents are child sessions, sibling sessions, or thread-level entities.
- Add parent/child relationships and provenance metadata.
- Ensure cleanup, logs, events, and worktrees remain understandable for grouped work.

Done when:

- One task can supervise multiple related agent executions coherently.

### 15. Add coordination primitives

Work:

- Support session-to-session input or handoff workflows beyond raw `source_session_id`.
- Add structured cross-session events for delegation and completion.
- Define how shared artifacts are attached back to the parent task.

Done when:

- Multi-agent work is visible as a coordinated unit, not a set of unrelated sessions.

## Suggested Order

Recommended implementation order:

1. Phase 0 documentation corrections.
2. Phase 1 runtime hardening.
3. Phase 2 CLI/product alignment.
4. Phase 3 task/thread/artifact data model.
5. Phase 4 attention model.
6. Phase 5 multi-agent orchestration.

## Immediate Next Steps

If the goal is to ship useful progress quickly, the next three concrete changes should be:

1. Update `README.md` and `ARCHITECTURE.md` so they match the current session-based implementation.
2. Improve `agent sessions` output so the current CLI is actually usable for supervision.
3. Design the first persistent schema addition for either `tasks` or `artifacts` before adding more commands.
