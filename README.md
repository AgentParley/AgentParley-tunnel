# agentparley-tunnel

The daemon a user installs on their own server so AgentParley can run commands on it with **no inbound port and no
firewall change**. It dials *out* to the SSH egress service and holds that connection open; the platform never
connects to the box.

This is the **public source** of the daemon — the exact code that runs on your machine, so you can read what you're
installing before the one-liner runs it. The tunnel **wire contract** (`proto/tunnel.proto`) is mirrored from the
AgentParley server, which owns it; a CI check on the server side fails if the two ever drift, so this copy always
matches what the egress speaks.

**Releases** are cut by tagging `tunnel-v<semver>`; CI cross-compiles, signs each binary with the release key, and
publishes to `https://tunnel-app.agentparley.ai`, where `install.sh` and the daemon's self-update fetch them.

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

## Install

```bash
curl -fsSL https://tunnel-app.agentparley.ai/install.sh | sudo sh
sudo -u <run-as-user> agentparley-tunnel login
sudo systemctl start agentparley-tunnel
```

No checkout, no flags. `install.sh` detects the machine's CPU (x86-64 or ARM64), downloads the matching binary
from `tunnel-app.agentparley.ai`, verifies its SHA-256 checksum, creates the run-as user if it doesn't already
exist (defaulting to whichever account ran `sudo`), writes `/etc/agentparley-tunnel/config.yaml`, creates
`/var/lib/agentparley-tunnel` (0700, owned by the run-as user), installs the systemd unit, and enables it. It does
NOT run `login` for you — approving a new install is a deliberate, interactive step (open the printed URL, type the
code, click Approve). It is **Linux with systemd only**; the script says so plainly on any other OS.

A piped script has no arguments, so every override is an environment variable:

| Variable | Default | Overrides |
| --- | --- | --- |
| `AGENTPARLEY_TUNNEL_USER` | `$SUDO_USER` | the OS account the daemon runs as |
| `AGENTPARLEY_TUNNEL_API_SERVER` | `https://app.agentparley.ai` | the PlatformApi the daemon logs in / refreshes against |
| `AGENTPARLEY_TUNNEL_EGRESS_SERVER` | `ssh-tunnel.agentparley.ai:443` | the SSH egress service the daemon connects out to |
| `AGENTPARLEY_TUNNEL_UPDATE_SERVER` | `https://tunnel-app.agentparley.ai` | where both install.sh and the daemon's self-update check for new releases; written into `config.yaml` as `update_server` only when overridden |
| `AGENTPARLEY_TUNNEL_BINARY` | (unset) | path to a locally built binary — skips the download and checksum entirely, for a dev/checkout install |

Example: `curl -fsSL https://tunnel-app.agentparley.ai/install.sh | sudo AGENTPARLEY_TUNNEL_USER=deploy sh`

Re-running the installer on an already-installed box is safe and doubles as a manual update: the binary and
systemd unit are refreshed, `config.yaml` and the box's credentials are left alone. If the service is currently
running, it keeps running the old binary until you `sudo systemctl restart agentparley-tunnel` — the installer
says so when this applies.

`uninstall.sh` (hosted the same way, `curl -fsSL https://tunnel-app.agentparley.ai/uninstall.sh | sudo sh`) is the
exact inverse; it does NOT delete the run-as user by default. Set `AGENTPARLEY_TUNNEL_DELETE_USER=true` (or pass
`--delete-user` if you downloaded the script first) to also remove the user install.sh created.

## Automatic updates

Every time the systemd unit starts (including every `Restart=always` restart), it first runs
`agentparley-tunnel self-update` as root, before the daemon itself starts:

- Fetches `<update_server>/latest-version`. If it differs from the version currently installed — not just
  "newer": a deliberately older value is how rollback works — it downloads the matching binary plus its
  `.sha256` and `.sig` sidecars, verifies BOTH the checksum and an Ed25519 signature (public key baked into the
  binary at build time), and atomically swaps it into place.
- Any failure anywhere in this — network down, a 404, a bad checksum, a bad signature, disk full — is logged to
  the journal and the check exits 0. Self-update is fail-open: a broken update check must never stop the tunnel
  from starting on the binary that's already there.
- Set `auto_update: false` in `config.yaml` to turn this off entirely (change-controlled environments may want
  the daemon version to only move when someone explicitly re-runs `install.sh`).

`journalctl -u agentparley-tunnel` shows every self-update attempt and outcome.

## Cutting a release (maintainers)

A release is a git tag of the shape `tunnel-v<semver>`, e.g.:

```bash
git tag tunnel-v1.4.2
git push origin tunnel-v1.4.2
```

This fires `.woodpecker/tunnel-release.yaml`, which cross-compiles amd64 + arm64 with the version stamped in via
`-ldflags -X .../internal/version.Version=1.4.2`, signs both binaries (`release/sign`, Ed25519, key from the
`tunnel_release_signing_key` Woodpecker secret), and uploads to the bucket behind `tunnel-app.agentparley.ai` —
the versioned artifacts first, `latest-version` last, so a box can never observe a half-published release. Every
release folder (`releases/<version>/`) is kept forever; only `latest-version` moves.

**Rollback** is re-pointing `latest-version` at an older, still-present release folder — not re-running the
pipeline (that builds a NEW release, it doesn't undo one):

```bash
echo "1.4.1" > /tmp/latest-version
aws --profile personal --region us-east-1 s3 cp /tmp/latest-version \
  s3://agentparley-tunnel-releases-970652200122/latest-version \
  --cache-control "public, max-age=300" --content-type "text/plain"
```

Every box picks up the rollback on its next service start (or within ~5 minutes for a fresh install), including a
box crash-looping on the bad release — `self-update` runs on every `Restart=always` attempt, so the fleet
self-heals without anyone touching individual machines. Watch for a drop in connected tunnels after a publish (the
alert this warrants lives in Grafana, not in this daemon).

### The signing key ceremony (one-time, or on a planned rotation)

```bash
cd ssh-tunnel-apps
go run ./release/sign -keygen
```

Prints a private and a public key. The private key goes into the `tunnel_release_signing_key` Woodpecker secret
**and** into a password manager as an offline backup — losing both copies means no future release can be trusted
by an existing install; every customer would have to re-run `install.sh` (checksum-only trust) to pick up a new
key. The public key is baked into `internal/selfupdate/publickey.go` and shipped in every subsequent build.
`infrastructure/set-tunnel-release-secret.sh` mints the separate AWS publish credentials
(`tunnel_release_aws_key_id` / `tunnel_release_aws_secret`) but does not touch the signing key — that stays a
manual, one-time step. All three release secrets must be scoped to **tag events only** in the Woodpecker UI, so
the push-to-master deploy pipeline never has access to them.

Planned key rotation (not a compromise): publish one transition release whose binary embeds the NEW public key
but is still signed with the OLD key, so the fleet updates under old-key trust before CI switches secrets — no
reinstall needed. A *compromised* key has no such path; recovery is customers re-running `install.sh`.

## Local dev / testing the release layout

Build both arches with a throwaway version, sign them, lay out `latest-version` + `releases/<v>/…` under a temp
directory, and serve it with `python3 -m http.server`; point `AGENTPARLEY_TUNNEL_BINARY`/`AGENTPARLEY_TUNNEL_UPDATE_SERVER`
at it to drive `install.sh` and `self-update` against a fake release site without touching the real bucket.

## Why the daemon runs commands as one specific OS user, with no privilege switch

The daemon has no setuid capability and does not attempt one. `config.yaml`'s `run_as` must name the SAME OS user
the daemon process itself runs as — the daemon checks this at startup and refuses to start on a mismatch. This
mirrors sshd's *contract* (resolving the user's shell and setting `SHELL`/`HOME`/`USER`/`LOGNAME`/`PATH` from their
passwd entry) without attempting sshd's *privilege separation* (which needs root and a setuid path this daemon
deliberately doesn't have).

## Configuration — `/etc/agentparley-tunnel/config.yaml`

```yaml
server:
  api: https://app.agentparley.ai       # PlatformApi — login/enrol/refresh
  egress: ssh-tunnel.agentparley.ai:443 # SSH egress service — the long-lived Connect stream
run_as: deploy
read_only: false        # true denies every write_file/delete_file
allow_commands: []       # empty = every command allowed (subject to deny_commands below)
deny_commands: []        # patterns; deny always beats allow
enabled: true            # false denies EVERY operation
max_concurrent_operations: 8   # optional; how many operations this box runs at once (default 8 if unset)
auto_update: true        # optional; false turns off the self-update ExecStartPre entirely (default true)
update_server: https://tunnel-app.agentparley.ai   # optional; where self-update checks for new releases
```

`policy` is checked before every single operation, on this box, regardless of what the platform's own account-level
allowlists already decided — it is the box owner's own lever, independent of AgentParley's. `allow_commands`/
`deny_commands` are matched against the RAW command text the agent asked for, never against the wrapped shell
invocation the daemon actually executes. Each pattern is matched against the FULL command line, where `*` matches
any run of characters (including `/` — a command line has no path-segment structure, so `rm -rf /*` matches
`rm -rf /etc`); everything else in the pattern must match literally.

## State this daemon owns on disk

| Path | Contents | Mode |
| --- | --- | --- |
| `/etc/agentparley-tunnel/config.yaml` | policy config | 0644 |
| `/var/lib/agentparley-tunnel/credentials.json` | refresh token + Ed25519 key — durable identity, survives a reboot on purpose | 0600, in a 0700 dir |

Everything below is deliberately **NOT** on persistent disk — a captured session environment can contain secrets
(an `AWS_SECRET_ACCESS_KEY`, a database password), so both the ledger and the shell state it describes live under a
tmpfs root that a machine reboot clears (a daemon *process* restart, unlike a reboot, still resumes — tmpfs survives
that): `/dev/shm/agentparley-$(id -u)`, falling back to `$XDG_RUNTIME_DIR/agentparley` and then `$HOME/.agentparley`
only when neither tmpfs option is available.

| Path (relative to the resolved tmpfs root) | Contents | Mode |
| --- | --- | --- |
| `ledger/` | which AgentParley session ids this daemon has already served (so a restart still reports "fresh" correctly for a truly-new session, and "resumed" for one it already saw) | 0700 |
| `sessions/{sessionId}-{generation}/` | the actual per-session shell state (working directory, exported environment, command history) — `generation` is `Session.ShellStateGeneration`, currently always 0 | written entirely by the wrapped shell text the platform sends — this daemon never reads or writes these files itself, except to delete the whole directory once its ledger entry ages out |
