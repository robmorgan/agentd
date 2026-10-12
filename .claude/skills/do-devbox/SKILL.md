---
name: do-devbox
description: Spin up, list or tear down a DigitalOcean droplet running agentd, paired with this machine so `agent --host NAME` works at once. Use when the user wants a remote devbox, a cloud machine for agents, to try agentd remotely, or to delete such a droplet.
allowed-tools:
  - Bash
  - Read
  - AskUserQuestion
---

# DigitalOcean devbox

`scripts/do-devbox.sh` in this repository does the work. This skill decides what
to pass it, gets the user's go-ahead for anything that costs money or destroys
state, and reports the result. Read the script's header (`scripts/do-devbox.sh
--help`) for what each step does; do not reimplement the steps by hand.

## Before running anything

1. Check the tools: `doctl account get` (if it fails, the user must run
   `! doctl auth init` themselves) and `agent --version`.
2. Show what exists: `scripts/do-devbox.sh ls` and `agent host ls --no-probe`.

## create

```sh
scripts/do-devbox.sh create NAME [--region SLUG] [--size SLUG] [--user NAME] [--ref REF | --local] [--no-claude]
```

- Creating a droplet bills the user's account. Unless the user already gave the
  name, region and size and asked you to go ahead, confirm them with
  AskUserQuestion first, quoting the hourly price
  (`doctl compute size list --format Slug,PriceHourly`).
- Defaults: region `sgp1` (`DO_REGION`), size `s-2vcpu-4gb` (`DO_SIZE`), the
  local user name, and `main` from GitHub. Smaller than 4 GB gets swap added so
  libghostty still builds; it is slow but works.
- Source: `--ref REF` builds a branch, tag or commit that is pushed to GitHub.
  Use `--local` to test this checkout's working tree, including uncommitted
  changes, for example before a branch is pushed.
- NAME: lowercase letters, digits and dashes. It becomes the droplet's name and
  the host name in `agent host ls`; the script refuses a name either already
  uses.
- It takes 5 to 10 minutes, mostly the build. Run it in the background with
  output to a log in the job's tmp directory, and watch for lines starting with
  `==>` or `error:`; it ends with `==> Ready`.
- Afterwards, check `agent hosts` shows the host online. Then tell the user
  that Claude Code on the droplet still needs signing in, in the first session
  they start there (`agent --host NAME new --cwd '~' NAME`, then attach).

If create fails partway, the droplet may exist without being paired. Read the
log, fix the cause, then either `destroy` and create again, or finish the
remaining steps by hand: `agentd remote enable IP:7433 --yes` and
`agentd remote pair` on the droplet (from a login shell, `bash -lc`), then
`agent host add NAME IP:7433 --code CODE` here.

## destroy

```sh
scripts/do-devbox.sh destroy NAME [--yes]
```

It deletes the droplet and every session on it, which cannot be undone. Pass
`--yes` only after the user confirmed deleting that droplet in this
conversation. It only ever touches droplets tagged `agentd`; never delete other
droplets in the account, even if the user's wording is ambiguous; ask instead.

## Notes

- The `agentd` cloud firewall (SSH and UDP 7433 in) is shared by every
  droplet tagged `agentd` and is left in place after `destroy`; it is free.
- The daemon on the droplet starts at boot (systemd user unit with lingering);
  sessions do not survive a reboot.
- Remote access listens on the droplet's public address. Only paired keys can
  connect; `agentd remote list` on the droplet shows them.
