# Production deployment

The production bundle uses Docker Compose and Caddy. Caddy terminates TLS and proxies requests to FusionGate over an isolated Docker network; the application container is not published directly on the host.

## Requirements

- Debian 12 or Ubuntu 22.04/24.04
- A server with at least 1 vCPU, 1 GB RAM, and 10 GB free disk
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
| `/opt/fusiongate/app` | Managed application source and Compose definition |
| `/opt/fusiongate/config` | Root-only configuration and secret source files |
| `/opt/fusiongate/data` | SQLite database and WAL files, owned by container UID 10001 |
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

Backups briefly stop the FusionGate application container to produce a consistent archive. The archive contains the database and encryption key and must be protected like production credentials.

## OAuth production notes

Codex and Claude browser authorization use the official CLI-compatible `localhost` redirect URIs. The production server does **not** need to expose ports 1455 or 54545: after authorization, copy the full localhost callback URL from the browser address bar and paste it into the FusionGate management console. Grok uses device authorization and does not require a callback port. Pending authorization sessions are held in memory for 15 minutes and can be consumed only once.

OAuth credentials imported from CLIProxyAPI or sub2api are encrypted with `FUSIONGATE_MASTER_KEY`. Before every update or migration, back up the following together:

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
