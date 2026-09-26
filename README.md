# agentparley

The daemon a user installs on their own server so AgentParley can run commands on it with **no inbound port and no
firewall change**. It dials *out* to the SSH egress service and holds that connection open; the platform never
connects to the box.

This is the **public source** of the daemon — the exact code that runs on your machine, so you can read what you're
installing before the one-liner runs it. The tunnel **wire contract** (`proto/tunnel.proto`) is mirrored from the
AgentParley server, which owns it; a CI check on the server side fails if the two ever drift, so this copy always
matches what the egress speaks.

**Install** is `curl -fsSL https://get.agentparley.ai/tunnel/install.sh | sudo sh`. That script is published from
`packaging/linux/install.sh` in this repo, which stays its source of truth. **Releases** are cross-compiled for
amd64 + arm64 and published to `https://get.agentparley.ai`, where install.sh and the daemon's `update` command
fetch the binary and verify it against its `.sha256` sidecar.

This README covers only what someone running the daemon needs.

## How it fits together

```
this daemon  --outbound gRPC-->  SSH egress service  <--Runtime posts commands here-- AgentParley
     |
     +--device grant / token refresh--> PlatformApi
```

- **`login`** — one-time RFC 8628 device grant: prints a short code, waits for approval in the AgentParley portal,
  then generates this box's Ed25519 identity key and exchanges a one-time enrolment token for a durable refresh
  token. Everything here talks to the PlatformApi only — the egress service is never involved in enrolment.
- **`start`** — the long-running connect loop (what the systemd unit runs). Refreshes the access token, dials the
  egress service, and answers the commands/file operations AgentParley sends down the stream, reconnecting
  automatically on a dropped connection.
- **`status`** — prints whether this box is enrolled and what the local config says.
- **`logout`** — revokes this install server-side (`POST /tunnel/revoke`, authenticated the same way as a token
  refresh) and then deletes the local credentials and key. The local wipe always happens, even if the box cannot
  reach the PlatformApi — a box that can't reach us must still be able to disarm itself.
- **`register`** — finds and registers this box's own local model harnesses (codex, claude, ollama, an
  OpenAI-compatible server) so AgentParley can route completions to them. See "Registering local model harnesses"
  below.
- **`unregister <harness>`** — removes this box's provider for one harness (`register`'s counterpart).
- **`update`** — downloads and installs the latest (or, for rollback, an older) published release, and restarts
  the systemd unit if it's enabled and was active. See "Updating" below.
- **`doctor`** — read-only diagnosis: version, config, service status, connectivity, clock skew, and local model
  providers. See "Diagnosing problems" below.

## Install

```bash
curl -fsSL https://get.agentparley.ai/tunnel/install.sh | sudo sh
sudo -u <run-as-user> agentparley login
sudo systemctl start agentparley
```

No checkout, no flags. `install.sh` detects the machine's CPU (x86-64 or ARM64), downloads the matching binary
from `get.agentparley.ai`, verifies its SHA-256 checksum, creates the run-as user if it doesn't already
exist (defaulting to whichever account ran `sudo`), writes `/etc/agentparley/config.yaml`, creates
`/var/lib/agentparley` (0700, owned by the run-as user), installs the systemd unit, and enables it. It does
NOT run `login` for you — approving a new install is a deliberate, interactive step (open the printed URL, type the
code, click Approve). It is **Linux with systemd only**; the script says so plainly on any other OS.

A piped script has no arguments, so every override is an environment variable:

| Variable | Default | Overrides |
| --- | --- | --- |
| `AGENTPARLEY_USER` | `$SUDO_USER` | the OS account the daemon runs as |
| `AGENTPARLEY_API_SERVER` | `https://services.agentparley.ai/platform` | the Platform API the daemon logs in / refreshes against |
| `AGENTPARLEY_EGRESS_SERVER` | `services.agentparley.ai:443` | the SSH egress service the daemon connects out to |
| `AGENTPARLEY_UPDATE_SERVER` | `https://get.agentparley.ai` | where both install.sh and the daemon's `update` check for new releases; written into `config.yaml` as `update_server` only when overridden |
| `AGENTPARLEY_BINARY` | (unset) | path to a locally built binary — skips the download and checksum entirely, for a dev/checkout install |

Example: `curl -fsSL https://get.agentparley.ai/tunnel/install.sh | sudo AGENTPARLEY_USER=deploy sh`

Re-running the installer on an already-installed box is safe and doubles as a manual update: the binary and
systemd unit are refreshed, `config.yaml` and the box's credentials are left alone. If the service is currently
running, it keeps running the old binary until you `sudo systemctl restart agentparley` — the installer
says so when this applies.

`uninstall.sh` (served the same way, `curl -fsSL https://get.agentparley.ai/tunnel/uninstall.sh | sudo sh`) is the
exact inverse; it does NOT delete the run-as user by default. Set `AGENTPARLEY_DELETE_USER=true` (or pass
`--delete-user` if you downloaded the script first) to also remove the user install.sh created.

## Updating

`sudo agentparley update` downloads and installs the latest published release — the same download, verify
checksum, and atomically swap in the new binary as always — then, if `agentparley.service` is enabled and was
running, restarts it and reports that too:

```
$ sudo agentparley update
updated 1.4.1 -> 1.4.2
restarting agentparley.service...
restarted agentparley.service
```

or, when already current: `already up to date (1.4.2)`. Must run as root. Two flags:

- `--no-restart` installs the update but leaves the running service alone — useful ahead of a planned maintenance
  window, when you don't want the daemon to bounce yet.
- `--from-service` is what the systemd unit's `ExecStartPre` passes — see below. It is the ONLY invocation that
  honors `auto_update: false`, and it implies `--no-restart` (the unit's own `ExecStart` is about to (re)start the
  daemon right after anyway). A human typing `sudo agentparley update` always updates, config setting or not —
  typing the command out is itself the explicit instruction.

Every time the systemd unit starts (including every `Restart=always` restart), it first runs
`agentparley update --from-service` as root, before the daemon itself starts:

- Fetches `<update_server>/latest-version`. If it differs from the version currently installed — not just
  "newer": a deliberately older value is how rollback works — it downloads the matching binary plus its
  `.sha256` sidecar, verifies the checksum, and atomically swaps it into place.
- Any failure anywhere in this — network down, a 404, a bad checksum, disk full — is logged to
  the journal and the check exits 0. This automatic path is fail-open: a broken update check must never stop the
  tunnel from starting on the binary that's already there.
- Set `auto_update: false` in `config.yaml` to turn this off entirely for the automatic path (change-controlled
  environments may want the daemon version to only move when someone explicitly runs `sudo agentparley update`).

`journalctl -u agentparley` shows every update attempt and outcome.

## Diagnosing problems — `agentparley doctor`

Read-only: `doctor` never registers a harness, restarts the service, or writes to disk — it only checks and
reports. Run it any time something looks wrong, or as the last step after a fresh install:

```bash
agentparley doctor                       # checks that don't need root/run_as still run
sudo -u <run_as> agentparley doctor      # the full set, including credentials and local model providers
```

One line per check, in order — `✓` passed, `✗` failed (with a plain-English fix), `–` skipped (not applicable, or
needs a different invoking user to check):

```
✓ version           1.4.2 (up to date)
✓ config            /etc/agentparley/config.yaml
✗ running as        this run is uid 0, not run_as "deploy" — some checks below need: sudo -u deploy agentparley doctor
–  logged in         skipped — run as: sudo -u deploy agentparley doctor
✓ service           enabled and active
✓ api reachable     https://services.agentparley.ai/platform/health
✓ egress reachable  services.agentparley.ai:443
✓ clock             within 1s of server time
–  providers         skipped — run as: sudo -u deploy agentparley doctor
```

Version checks the installed build against `<update_server>/latest-version` (the fix is `sudo agentparley
update`); config checks `/etc/agentparley/config.yaml` exists, parses, and names a real `run_as` user; service
checks `systemctl is-enabled`/`is-active` for `agentparley.service`; api/egress reachable dial the configured
Platform API and SSH egress service within 5s each; clock compares local time against the API response's own
`Date` header (tokens are time-sensitive, so more than 60s of skew is flagged with a `timedatectl set-ntp true`
hint); providers re-runs each already-registered harness's own readiness check (`Detect`) — the same one
`register` uses — without registering anything. Exits 0 if every check passed (skips don't count against it), 1
if anything failed.

## Cutting a release (maintainers)

A release is a git tag of the shape `tunnel-v<semver>`, e.g.:

```bash
git tag tunnel-v1.4.2
git push origin tunnel-v1.4.2
```

This fires `.github/workflows/release.yaml`, which cross-compiles amd64 + arm64 with the version stamped in via
`-ldflags -X .../internal/version.Version=1.4.2`, writes each binary's `.sha256` sidecar, and uploads to the
bucket behind `get.agentparley.ai` —
the versioned artifacts first, `latest-version` last, so a box can never observe a half-published release. Every
release folder (`releases/<version>/`) is kept forever; only `latest-version` moves.

**Rollback** is re-pointing `latest-version` at an older, still-present release folder — not re-running the
pipeline (that builds a NEW release, it doesn't undo one):

```bash
echo "1.4.1" > /tmp/latest-version
aws --profile agentparley --region eu-central-1 s3 cp /tmp/latest-version \
  s3://get.agentparley.ai/latest-version \
  --cache-control "public, max-age=300" --content-type "text/plain"
```

Every box picks up the rollback on its next service start (or within ~5 minutes for a fresh install), including a
box crash-looping on the bad release — `agentparley update --from-service` runs on every `Restart=always`
attempt, so the fleet self-heals without anyone touching individual machines. Watch for a drop in connected
tunnels after a publish (the alert this warrants lives in Grafana, not in this daemon).

## Local dev / testing the release layout

Build both arches with a throwaway version, write their checksums, lay out `latest-version` + `releases/<v>/…` under a temp
directory, and serve it with `python3 -m http.server`; point `AGENTPARLEY_BINARY`/`AGENTPARLEY_UPDATE_SERVER`
at it to drive `install.sh` and `update` against a fake release site without touching the real bucket.

## Why the daemon runs commands as one specific OS user, with no privilege switch

The daemon has no setuid capability and does not attempt one. `config.yaml`'s `run_as` must name the SAME OS user
the daemon process itself runs as — the daemon checks this at startup and refuses to start on a mismatch. This
mirrors sshd's *contract* (resolving the user's shell and setting `SHELL`/`HOME`/`USER`/`LOGNAME`/`PATH` from their
passwd entry) without attempting sshd's *privilege separation* (which needs root and a setuid path this daemon
deliberately doesn't have).

## Configuration — `/etc/agentparley/config.yaml`

```yaml
server:
  api: https://services.agentparley.ai/platform # Platform API — login/enrol/refresh
  egress: services.agentparley.ai:443   # SSH egress service — the long-lived Connect stream
run_as: deploy
read_only: false        # true denies every write_file/delete_file
allow_commands: []       # empty = every command allowed (subject to deny_commands below)
deny_commands: []        # patterns; deny always beats allow
enabled: true            # false denies EVERY operation
max_concurrent_operations: 8   # optional; how many operations this box runs at once (default 8 if unset)
auto_update: true        # optional; false turns off the automatic update ExecStartPre entirely (default true)
update_server: https://get.agentparley.ai   # optional; where `agentparley update` checks for new releases

# Local model harnesses (see "Registering local model harnesses" below). Every key here is optional — `register`
# auto-discovers what it can; set these only to override or to supply what discovery can't determine on its own.
allow_harnesses: []   # empty = all four harnesses considered (subject to detection succeeding on the box)
deny_harnesses: []    # e.g. ["codex"] to keep the codex harness off even if installed — deny always beats allow
harnesses:
  codex:
    command: /home/deploy/.npm-global/bin/codex   # optional override; `register` finds this itself otherwise
  claude:
    command: claude                # optional override; defaults to "claude", auto-discovered by `register`
  ollama:
    url: http://localhost:11434    # this is the built-in default; only needed to override
  openai-compatible-local:
    url: http://localhost:8000     # optional; `register` also tries LM Studio/vLLM/llama.cpp's well-known ports
    models:
      - id: my-local-model
        label: My local model
        context_window_tokens: 32768   # required whenever the server doesn't expose its context window
```

`policy` is checked before every single operation, on this box, regardless of what the platform's own account-level
allowlists already decided — it is the box owner's own lever, independent of AgentParley's. `allow_commands`/
`deny_commands` are matched against the RAW command text the agent asked for, never against the wrapped shell
invocation the daemon actually executes. Each pattern is matched against the FULL command line, where `*` matches
any run of characters (including `/` — a command line has no path-segment structure, so `rm -rf /*` matches
`rm -rf /etc`); everything else in the pattern must match literally. `allow_harnesses`/`deny_harnesses` use the same
glob matching, against the harness name (`codex`, `claude`, `ollama`, `openai-compatible-local`).

## Registering local model harnesses

`agentparley register` (no argument) is how you expose this box's own local models — a signed-in codex or
claude CLI, a running Ollama, or an OpenAI-compatible server (LM Studio, vLLM, llama.cpp) — to AgentParley as
Providers your agents can pick. Run it once as the box's `run_as` user (`sudo -u <run_as> agentparley
register`, or plain `agentparley register` if you're already that user) any time you install or update a
local harness; it is safe to re-run.

It scans all four harnesses in order and prints one line per harness:

```
✓ codex                    registered — Codex (ChatGPT account default)
✓ ollama                   registered — 3 models
✗ claude                   installed but not ready: claude CLI version "2.0.9" is older than the minimum 2.1.207 ...
– openai-compatible-local  not found
```

`✓` registered, `✗` found but not ready (a real problem — an old CLI version, a signed-out account, a server
returning malformed models), `–` not found or excluded by `allow_harnesses`/`deny_harnesses` on this box — the
ordinary case for whichever of the four you don't run. The command exits non-zero only when NOTHING registered and
at least one harness genuinely failed; a box with no local model providers at all exits 0.

`register <harness>` does the same for just that one harness. `unregister <harness>` removes it again.

**Why config.yaml is usually unnecessary:** a codex/claude CLI installed under your own login shell (nvm,
`~/.npm-global`, homebrew) is invisible to `sudo -u <run_as> agentparley register` and to the systemd
service itself — both run with a stripped-down PATH that never sees where an interactive terminal finds it.
`register` works around this by asking the `run_as` user's own login shell where the binary lives (and what PATH it
needs, e.g. for a `codex`/`claude` installed as a `#!/usr/bin/env node` script) and remembers the answer — no
config.yaml edit, no daemon restart. An explicit `harnesses.<name>.command`/`.url` in config.yaml always overrides
whatever `register` found.

## State this daemon owns on disk

| Path | Contents | Mode |
| --- | --- | --- |
| `/etc/agentparley/config.yaml` | policy config | 0644 |
| `/var/lib/agentparley/credentials.json` | refresh token + Ed25519 key — durable identity, survives a reboot on purpose | 0600, in a 0700 dir |
| `/var/lib/agentparley/harnesses.json` | what `register` auto-discovered per harness (a CLI's absolute path + PATH, or a local server's URL) — merged in under config.yaml, never overriding an explicit value there | 0600, in a 0700 dir |

Everything below is deliberately **NOT** on persistent disk — a captured session environment can contain secrets
(an `AWS_SECRET_ACCESS_KEY`, a database password), so both the ledger and the shell state it describes live under a
tmpfs root that a machine reboot clears (a daemon *process* restart, unlike a reboot, still resumes — tmpfs survives
that): `/dev/shm/agentparley-$(id -u)`, falling back to `$XDG_RUNTIME_DIR/agentparley` and then `$HOME/.agentparley`
only when neither tmpfs option is available.

| Path (relative to the resolved tmpfs root) | Contents | Mode |
| --- | --- | --- |
| `ledger/` | which AgentParley session ids this daemon has already served (so a restart still reports "fresh" correctly for a truly-new session, and "resumed" for one it already saw) | 0700 |
| `sessions/{sessionId}-{generation}/` | the actual per-session shell state (working directory, exported environment, command history) — `generation` is `Session.ShellStateGeneration`, currently always 0 | written entirely by the wrapped shell text the platform sends — this daemon never reads or writes these files itself, except to delete the whole directory once its ledger entry ages out |
