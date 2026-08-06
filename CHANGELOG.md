# Changelog

## V1.09 - 2026-08-06

### Health and failover

- Separate model-list reachability from real generation health. A successful `/models` request is reported as `reachable` and no longer marks a provider as generation-healthy.
- Remove the background circuit-recovery probe that could reopen a circuit based only on `/models` success.
- Recover an open circuit only through the next real business request after cooldown, with one half-open probe at a time.
- Add per-provider `health_check_enabled` controls for API and OAuth providers. Disabled providers receive no background or manual health probes, while real traffic continues to contribute to failure statistics and circuit breaking.
- Keep manual model checks as real, content-validating generation probes and preserve compatibility with existing databases and provider backups.
- Bound non-streaming upstream response-body reads by the request context timeout so failover can proceed after a stalled upstream response.

### Client compatibility

- Preserve reasoning effort forwarding for OpenAI Responses and Codex Responses requests.
- Keep discovered reasoning levels and defaults visible in model metadata without forcing a runtime reasoning value.

### Documentation and verification

- Document the OAuth background health-check interval and concurrency environment variables.
- Verified with `go test ./...`, targeted `go test -race`, `go vet ./...`, production binary build, and embedded UI JavaScript syntax checks.
