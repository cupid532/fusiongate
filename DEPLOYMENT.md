# Production deployment

The production bundle uses Docker Compose and Caddy. Caddy terminates TLS and proxies requests to FusionGate over an isolated Docker network; the application container is not published directly on the host.

## Two deployment models

There are two supported ways to run FusionGate, and they are operated differently. Pick one per host and know which one you are on — `ls /opt/fusiongate/.fusiongate-install` tells you.

| | Installer-managed | Self-managed |
|---|---|---|
| Set up by | `deploy/install.sh` | hand-written Compose + existing host Caddy |
| Marker file | `/opt/fusiongate/.fusiongate-install` exists | absent |
| Compose file | `/opt/fusiongate/app/deploy/compose.production.yml` | `/opt/fusiongate/docker-compose.yml` |
| Env file | `/opt/fusiongate/config/compose.env` | `/opt/fusiongate/config/fusiongate.env` |
| Caddy | container, managed by the bundle | host service, shared with other sites |
| Upgrades | `fusiongatectl update` | [`deploy/deploy-from-origin.sh`](#self-managed-upgrades) |

Everything below applies to both models except the sections marked otherwise. **`fusiongatectl` requires the installer-managed layout** and will refuse to run without the marker file — on a self-managed host, use `deploy/deploy-from-origin.sh` and plain `docker compose` instead.

## Quick navigation

- [Requirements](#requirements)
- [Installation](#installation)
- [Operations](#operations)
- [Self-managed upgrades](#self-managed-upgrades)
- [OAuth production notes](#oauth-production-notes)
- [Firewall](#firewall)
- [Restore](#restore)
- [Post-install checklist](#post-install-checklist)

> Production rule: expose only HTTPS through Caddy. Keep FusionGate port `8787`, SQLite data, and secret source files private.

## Requirements

- Debian 12 or Ubuntu 22.04/24.04
- A server with at least 2 vCPU, 2 GB RAM, and 10 GB free disk when the quality detector is enabled
- A DNS A record, and optionally an AAAA record, pointing to the server
- Inbound TCP 80 and TCP/UDP 443
- Outbound HTTPS access to GitHub, Docker Hub, Docker's apt repository, certificate authorities, configured upstream providers, and the Codex / Claude OAuth authorization and token endpoints when those integrations are enabled

Do not place Cloudflare or another proxy in front of the first installation until Caddy has successfully obtained a certificate, unless its TLS mode is configured correctly.

## Installation

Review and execute the installer:

```bash
curl -fsSLo install.sh https://raw.githubusercontent.com/cupid532/fusiongate/main/deploy/install.sh
less install.sh
sudo bash install.sh
```

For non-interactive provisioning, pass values through a root-only environment or secret manager rather than storing them in shell history:

```bash
sudo -E env \
  FUSIONGATE_DOMAIN=ai.example.com \
  FUSIONGATE_ADMIN_PASSWORD_FILE=/run/secrets/bootstrap-password \
  bash install.sh
```

The installer itself prompts securely by default. `FUSIONGATE_ADMIN_PASSWORD_FILE` in the example above is intended for provisioning wrappers; when invoking the installer directly, set `FUSIONGATE_ADMIN_PASSWORD` in a protected process environment or use the prompt.

## Installed paths

| Path | Purpose |
|---|---|
| `/opt/fusiongate/app` | Managed application source and Compose definition (installer-managed only) |
| `/opt/fusiongate/releases/<sha>` | Immutable `git archive` export used as the build context (self-managed) |
| `/opt/fusiongate/last-deployed-commit` | Commit SHA of the running image, written after a verified deploy |
| `/opt/fusiongate/last-rollback-image` | Image tag the previous deploy can be reverted to |
| `/opt/fusiongate/config` | Root-only configuration and secret source files |
| `/opt/fusiongate/data` | SQLite database and WAL files, owned by container UID 10001 |
| `/opt/fusiongate/quality-detector-data` | Quality-detector SQLite sessions and reports, owned by container UID 10002; API keys are not persisted |
| `/opt/fusiongate/caddy-data` | TLS certificates and Caddy state |
| `/var/backups/fusiongate` | Backups created by `fusiongatectl backup` |
| `/usr/local/bin/fusiongatectl` | Operations command |

## Operations

```bash
sudo fusiongatectl status
sudo fusiongatectl logs 300
fusiongatectl health
sudo fusiongatectl restart
sudo fusiongatectl update
sudo fusiongatectl backup
```

An update downloads the configured repository and Git ref, replaces only the managed application source, rebuilds the image, and preserves configuration, secrets, database files, and Caddy state.

The optional quality-detector sidecar shares the FusionGate network namespace and binds only to loopback. Production Compose should set `FUSIONGATE_QUALITY_DETECTOR_URL=http://127.0.0.1:18789` and `FUSIONGATE_QUALITY_DETECTOR_BASE_URL=http://127.0.0.1:8787/v1`; do not publish port `18789`. Recreating the FusionGate container replaces that network namespace, so always recreate `fusiongate` and `quality-detector` together. `fusiongatectl restart` and `fusiongatectl update` enforce this, and the container health check verifies both loopback services from FusionGate's current namespace.

After every update, verify both process readiness and the public TLS endpoint:

```bash
fusiongatectl health
curl -fsS https://ai.example.com/readyz
sudo fusiongatectl logs 100
```

Backups briefly stop the FusionGate application container to produce a consistent archive. The archive contains the database and encryption key and must be protected like production credentials.

## Self-managed upgrades

On a self-managed host, upgrades go through `deploy/deploy-from-origin.sh`. Its purpose is to make one property true at all times: **the running process is a commit that exists on `origin/main`.**

```bash
# deploy whatever origin/main currently points at
sudo /root/work/fusiongate-ui-strategy/deploy/deploy-from-origin.sh

# check every precondition without building anything
sudo FUSIONGATE_DRY_RUN=1 .../deploy/deploy-from-origin.sh

# roll back to an earlier pushed commit
sudo .../deploy/deploy-from-origin.sh 8bf7d9d
```

The script refuses to proceed — it does not warn and continue — when:

- the checkout has uncommitted changes, so what you are looking at is not what would ship;
- the requested commit is not an ancestor of `origin/main`, i.e. it was never pushed;
- `internal/fusiongate/version.go` still reports the version that is already running, which per [AGENTS.md](AGENTS.md) means the bump was forgotten.

What it then does:

1. `git archive` the exact commit into `releases/<short-sha>/`. The build context therefore contains only committed content — a stray local file cannot be baked into an image even by accident.
2. Build with `org.opencontainers.image.revision` / `.version` set from that commit, and assert the resulting labels match before the image is allowed near traffic.
3. Tag the outgoing image `fusiongate:rollback-<timestamp>`.
4. Recreate `fusiongate` **and** `quality-detector` together, then wait for both container health checks.
5. On health failure, automatically restore the rollback image and abandon the deploy.
6. Verify `/healthz` reports the intended `revision` and `version`, then record the commit in `last-deployed-commit`.
7. Prune old rollback images, candidate images, release exports, and build cache.

### Verifying parity at any time

```bash
# what the host is running
curl -fsS https://api.codelee.de/healthz | jq '{version, revision}'

# what GitHub has
git -C /root/work/fusiongate-ui-strategy fetch origin --quiet
git -C /root/work/fusiongate-ui-strategy rev-parse origin/main
```

The `revision` and `origin/main` values must be identical. If they are not, the host is running something GitHub does not have (or vice versa) and the next deploy should reconcile it.

> The build context is a throwaway export, not a checkout. Do not edit
> `/opt/fusiongate/app` or `/opt/fusiongate/releases/*` and expect it to
> survive — all source changes belong in the git checkout, committed and
> pushed.

## OAuth production notes

Codex and Claude browser authorization use the official CLI-compatible `localhost` redirect URIs. The production server does **not** need to expose ports 1455 or 54545: after authorization, copy the full localhost callback URL from the browser address bar and paste it into the FusionGate management console. Grok uses device authorization and does not require a callback port. Pending authorization sessions are held in memory for 15 minutes and can be consumed only once.

Imported OAuth credentials are encrypted with `FUSIONGATE_MASTER_KEY`. Before every update or migration, back up the following together:

- `/opt/fusiongate/data/fusiongate.db` (and any `-wal` / `-shm` files when the service is running);
- `/opt/fusiongate/config` or the secret source containing `FUSIONGATE_MASTER_KEY`;
- `/opt/fusiongate/app` and the active Compose definition.

Never paste OAuth JSON into shell history, deployment logs, issue trackers, or chat. Use the authenticated management page over HTTPS. Batch export files contain complete Access / Refresh / ID Tokens; keep them out of source control and delete or encrypt them after migration. Only import or export accounts you own or are authorized to administer.

## Firewall

If UFW is enabled:

```bash
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp
sudo ufw status
```

Do not expose port 8787 publicly. In the production Compose definition it is only visible inside the Docker network.

## Restore

1. Stop services with `sudo fusiongatectl stop`.
2. Keep the current installation as a rollback copy.
3. Extract the backup into `/opt/fusiongate`, preserving ownership and permissions.
4. Ensure `/opt/fusiongate/data` is owned by UID/GID `10001:10001`.
5. Start services and verify `/healthz`.

```bash
sudo chown -R 10001:10001 /opt/fusiongate/data
sudo fusiongatectl start
fusiongatectl health
```

The database cannot decrypt saved upstream credentials without the matching master key.

## Post-install checklist

- Store the generated or chosen administrator password in a password manager; the installer displays a generated password only once.
- Add Providers, routes, and downstream keys; never reuse an administrator password as an API key.
- Verify failover with a test route before production traffic.
- Schedule encrypted off-host backups.
- Enable host security updates and monitor disk usage.
- Review Caddy and FusionGate logs without collecting request bodies or credentials.
