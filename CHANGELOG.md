# Changelog

## V1.24

### Routing and scheduling

- Score one entry per provider in adaptive selection. A provider exposing several API keys resolved to several routes and accumulated its weight once per route, so a three-key provider took roughly three times the traffic of an equally weighted single-key peer.
- Scope the smooth weighted round-robin accumulator per public model. A single global accumulator let a busy model permanently bias the traffic distribution of a quiet model that happened to share an upstream.
- Send the half-open recovery probe to a provider as soon as its cooldown elapses under adaptive routing. The recovering provider still carries its failure penalty, so it could previously lose every scoring round to healthy peers and never be retried.
- Floor the consecutive-failure penalty so a provider below the circuit threshold cannot be scored out of the rotation entirely.
- Stop ranking a never-probed provider above one whose reachability was confirmed.
- Clear per-model rotation state on any routing strategy change, not only when switching to smart round robin.

### API keys and budgets

- Serve concurrent requests for a budgeted API key. A budget is an accounting limit, but admission held a per-key lock for the whole request, so every budgeted key was effectively limited to one in-flight request and the second concurrent request was rejected with `budget_request_inflight`. Admission now allows up to eight in-flight requests per key and narrows to one only after 90% of the budget is spent, which keeps the unavoidable overshoot small.
- Skip the request-ledger cost aggregate during authentication for keys without a budget, and stop authenticating budgeted keys twice per request. Both queries ran on the single SQLite connection shared by all traffic.

### Admin console theming

- Replace the per-component `html[data-theme="light"]` overrides with a single semantic token layer. 40 component override rules collapsed into token declarations, so restyling the console at scale now means editing one block instead of hunting hard-coded values across a 300 KB stylesheet.
- Give the sidebar and its controls their own `--sidebar-*` / `--nav-*` tokens. Deriving that permanently dark scope from the global `--text` / `--muted` tokens, which flip per theme, is what forced the override pile in the first place.
- Remove the superseded first-generation theme: two fully overridden variable blocks and 17 dead rules.
- Fix light-theme elements still rendering in the abandoned warm cream palette inside an otherwise cool theme, including toasts, stat cards, form panels, neutral badges, icon buttons and row hovers.
- Collapse five near-identical emphasis text shades into one `--text-strong` token.
- Document the token layer in the stylesheet and add a regression test that fails if a per-component theme override is reintroduced.

## V1.14

- Increase the default streaming first-output timeout from 12 seconds to 30 seconds so slow-starting Claude inference requests are not failed over prematurely.
- Keep the timeout configurable with `FUSIONGATE_STREAM_START_TIMEOUT` and document the new default.

## V1.13

- Remove the fixed 30-second upstream response-header timeout so long-running Codex Responses requests honor each provider's configured timeout.

## V1.12

- Filter Codex's internal `X-OpenAI-Internal-Codex-Responses-Lite` header before forwarding non-transparent OpenAI-compatible requests.
- Add regression coverage to ensure internal Codex transport metadata is not passed to compatible upstreams.

## V1.11

- Update provider and model-route ordering immediately after drag, move-up, or move-down actions.
- Roll back optimistic ordering changes and reload server state when persistence fails.
- Render ordinary providers from their persisted global `sort_order` so the UI matches the saved order without a refresh.

## V1.1

- Bound upstream failover attempts to prevent retry storms and expose the attempt count on bounded failures.
- Add a global request admission limit with `Retry-After` overload responses.
- Make streaming start and idle timeouts configurable.
- Add adaptive routing penalties for provider and health-check failures.
- Rate-limit API key `last_used_at` writes to reduce SQLite write contention.
- Add optional CORS origin allowlisting and an authenticated admin runtime metrics endpoint.

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
