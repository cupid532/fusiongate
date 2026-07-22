# Security Policy

FusionGate handles API credentials and should be treated as security-sensitive infrastructure.

## Supported version

Security fixes are applied to the latest release and the current `main` branch.

## Reporting a vulnerability

Do not open a public issue containing credentials, exploit details, or private deployment information. Use GitHub's private vulnerability reporting feature when enabled, or contact the repository owner privately.

Include affected versions, reproduction steps, impact, and any suggested mitigation. Revoke any credential that may have been exposed before sending a report.

## Deployment guidance

- Keep `FUSIONGATE_MASTER_KEY` and the administrator password outside Git.
- Use HTTPS and restrict access to the management console.
- Keep Docker, Caddy, and the host operating system updated.
- Back up the SQLite database and master key together, and encrypt the backup.
