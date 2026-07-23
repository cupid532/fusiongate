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
## OAuth credential handling

- Codex / Claude browser authorization uses PKCE S256, a cryptographically random state, a 15-minute in-memory session, and one-time session consumption. Grok uses the official device authorization flow.
- CLIProxyAPI / sub2api JSON is parsed only in authenticated administrator endpoints. Import preview is masked and does not return Access Token, Refresh Token, ID Token, or the original JSON. Multiple files may be selected, but no account is selected or imported by default.
- Credential export requires an explicit sensitive-data acknowledgement, accepts only existing OAuth providers, is limited to 200 records, and returns `no-store` download responses. Exported JSON is never written to application logs or browser storage.
- The complete OAuth credential object is encrypted at rest with AES-256-GCM. Token endpoint response bodies, imported JSON, Authorization headers, and request/response content are not written to application logs.
- Refresh occurs shortly before expiry. Rotated Refresh Tokens are stored atomically with the refreshed credential; failures expose only a generic status and allow normal provider failover.
- Back up the database and master key together. Anyone with both can decrypt all stored upstream credentials. Revoke provider-side sessions immediately if either may have been exposed.
- Import only credentials for accounts you own or are authorized to manage. FusionGate does not support password collection, Cookie extraction, session hijacking, or bypassing provider access controls.
