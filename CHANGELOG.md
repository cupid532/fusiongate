# Changelog

## V1.35

- Drop the `route_policies` table and every write that maintained it. Per-model strategies predate the global routing strategy, and no request path has ever read the table, so model create/rename/delete and discovery were paying transaction cost to feed a dead schema. Migration removes the table from existing databases, and the legacy-schema test now asserts the drop.

## V1.34

- Cap the in-memory provider-key cooldown at 10 minutes. A 401/403/429 used to adopt the upstream `Retry-After` without any bound, so a single 429 answering `Retry-After: 86400` silently benched a healthy channel for a day; the cooldown lives only in process memory, so the console kept showing the provider as healthy while every request skipped it.
- Let `auth_expired` providers recover on their own. The scheduler used to exclude them permanently, so a refreshed OAuth token never returned to rotation until an external import rewrote the provider status. The immediate circuit breaker that every 401/403 already triggers now throttles retries instead, and the half-open probe promotes the provider back to healthy on its first success.

## V1.33

- Harden console and authentication boundaries: apply a restrictive Content Security Policy, derive login rate-limit identity from the trusted client-IP parser instead of a forged leading `X-Forwarded-For` value, and keep provider credential/decryption details out of public model-resolution errors.
- Make smart round-robin fair by rotating provider groups rather than expanded API-key routes. A channel with three keys now receives the same first-attempt share as a channel with one key, while its keys remain contiguous for request-local failover.
- Clear stale per-key cooldown state after edits, re-enabling, deletion, provider runtime resets, successful model discovery and successful manual tests, so repaired keys return to service immediately.
- Add deterministic fleet regressions for 40 providers × 3 keys with 32 concurrent schedulers, 30 broken providers before a healthy fallback, 200 independent model cursors, provider-level round-robin fairness and cooldown lifecycle cleanup.
- Record validated reasoning strength (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`) in the request ledger and requests API, and show a compact intensity-tinted chip beside the model in the console.
- Refine provider and model-route ordering without removing drag or move controls: names now align cleanly as self-describing links without a duplicate domain or external-arrow decoration, while hover/focus reveals consistent SVG grip and caret controls.
- Move add/edit API-channel configuration into a focused, frosted modal instead of extending the page. Add dialog semantics, Escape/backdrop dismissal and focus restoration, plus spring motion with a reduced-motion fallback.
- Raise the remaining navigation-label, dark-table-header and light-sidebar-status contrast tokens above WCAG AA while preserving the single semantic theme-token layer, and apply restrained Apple-style spacing, blur and motion polish across both themes.

## V1.32

- Always render the version with two decimal digits. The old rule dropped insignificant trailing hundredths, so a major bump from `V1.29` displayed as `V1.3` and read like a downgrade in the console sidebar. `V1.30` now stays `V1.30`.
- Normalise the two historical headings that used one decimal digit (`V1.1` and `V1.3`) so the changelog reads monotonically.
- Add `TestVersionUsesTwoDecimalDigits`, an integer-hundredths round-trip check, and a changelog test that fails when a heading skips the format or when the current version has no entry.

## V1.31

- Split the admin console into three embedded assets: `ui/index.html` (shell), `ui/app.css` (styles and theme tokens) and `ui/app.js` (behaviour). It was one 285 KB file holding a 78 KB stylesheet and a 165 KB script with 259 functions in a single `<script>` block.
- Serve the stylesheet and script from `/ui/app.css` and `/ui/app.js`, requested with the running version so a matching response can be cached indefinitely and an upgrade invalidates it. Only those two names are served, so the path is not a file server.
- Keep the pre-paint theme script inline in the shell, so switching themes still cannot flash the wrong one.

The binary is still a single self-contained executable: the assets are embedded as a directory rather than a single file.

## V1.30

- Extract the upstream call that the Anthropic and Gemini chat conversions shared. The two functions were 73% identical, including a 28-line verbatim block covering transport errors, retryable statuses, client-error forwarding and body decoding, so a fix to one could silently miss the other.
- Render both conversions through one `writeChatCompletion` helper instead of duplicating the OpenAI response shape and cost settlement.
- Split `providerByID` into a path router plus `providerUpdate` and `providerDelete`. It was a single 305-line function that both dispatched sub-resources and implemented provider CRUD.

`proxyUpstream` is deliberately left whole. It is long but strictly linear, and its failover invariants — above all that a second upstream response is never spliced in after the first byte reaches the client — are easier to verify in one place than spread across helpers that pass a context, a cancel function and a start timer between them.

## V1.29

- Stop trusting the first `X-Forwarded-For` entry for the request ledger's client address. Proxies append the peer they saw, so the leading entry is whatever the caller chose to send; the gateway now scans the chain from the right and takes the first public address, skipping both trusted local hops and injected entries.
- Apply the API key rate limit as a sliding window. The fixed window let a caller spend the whole limit just before a boundary and again just after, delivering twice the configured rate.
- Scan comma-separated permission and capability lists in place. Model permission checks run on every request and now allocate nothing.

## V1.28

- Maintain each API key's spend as a running total on the key instead of summing its whole request ledger. Admission previously ran that aggregate on every request from a budgeted key, over rows that only accumulate for a year.
- Stop refunding budget when retention prunes old rows. Because spend was derived from the ledger, a budget silently regained capacity once its rows passed the one-year cutoff; the running total no longer does.
- Seed the running total from existing ledger history on upgrade, so budgets carry their spend across the migration.
- Read the same total for the console's key list and active-key count rather than recomputing two correlated subqueries.

## V1.27

### Request-path database work

- Queue request-ledger writes on a single writer goroutine instead of writing them inline. The first-byte callback runs inside the response read path, so its `UPDATE` previously delayed the first byte the client received by however long SQLite took to accept it. Ledger statements are now applied in FIFO order behind the attempt's `INSERT`, addressed by `request_id`.
- Give reads their own connection pool. WAL allows readers to run concurrently with the writer, but every query shared the one writer connection, so opening the usage and cost dashboard queued its year-wide aggregates ahead of live gateway traffic.
- Set `synchronous=NORMAL` on the writer. Under WAL a commit still survives a process crash; only host power loss can lose the most recent transactions, and it removes an fsync from every request-path write.
- Move request-ledger retention off the request path onto a daily loop. It previously re-checked the cutoff on every gateway request.
- Report `ledger_writes_queued`, `ledger_queue_waits` and `ledger_write_errors` from the admin metrics endpoint.

The ledger is now eventually consistent: a full queue blocks rather than dropping, because a dropped row would also drop the cost it carries, and shutdown drains the queue before closing the database.

## V1.26

### Dead code removal

- Remove the unregistered `routePolicies` admin handler, the unused `routeStrategy` helper and the dead `Route.Strategy` field. They were the last Go remnants of the abandoned per-model routing strategy, which the global strategy setting replaced.
- Remove the `doCodexQuotaRequest` and `fetchCodexAccountQuota` wrappers; every caller already used the `...ViaNode` variants directly.
- Remove 22 orphaned CSS class groups left behind when the per-model strategy editor and the model-health overview were taken out of the console, plus two unreachable compound selectors.
- Remove the `openAPIHealthPicker` and `renderProviderSelect` functions, which nothing referenced.
- Remove 123 stylesheet declarations that a later rule with the identical selector already overrode, the last residue of the two stacked theme generations. Verified by resolving every `(selector, property)` pair before and after: no computed value changes.
- Replace a manual append loop with a variadic append in `modelMetadata`.

Verified with `gofmt`, `go vet ./...`, `staticcheck ./...`, `go test ./...`, and a syntax check of both embedded scripts. `staticcheck` is clean apart from 11 `ST1005` reports on error strings that begin with proper nouns (`Codex`, `Shadowsocks`, `Trojan`, `Responses`), which Go's convention explicitly permits.

## V1.25

- Never limit concurrency for a budgeted API key. A budget is an accounting limit, so requests are admitted whenever the budget still has headroom regardless of how many are already in flight, and `budget_request_inflight` is gone. Only an exhausted budget stops a key.

## V1.24

### Routing and scheduling

- Score one entry per provider in adaptive selection. A provider exposing several API keys resolved to several routes and accumulated its weight once per route, so a three-key provider took roughly three times the traffic of an equally weighted single-key peer.
- Scope the smooth weighted round-robin accumulator per public model. A single global accumulator let a busy model permanently bias the traffic distribution of a quiet model that happened to share an upstream.
- Send the half-open recovery probe to a provider as soon as its cooldown elapses under adaptive routing. The recovering provider still carries its failure penalty, so it could previously lose every scoring round to healthy peers and never be retried.
- Floor the consecutive-failure penalty so a provider below the circuit threshold cannot be scored out of the rotation entirely.
- Stop ranking a never-probed provider above one whose reachability was confirmed.
- Clear per-model rotation state on any routing strategy change, not only when switching to smart round robin.

### API keys and budgets

- Serve concurrent requests for a budgeted API key. A budget is an accounting limit, but admission held a per-key lock for the whole request, so every budgeted key was effectively limited to one in-flight request and the second concurrent request was rejected with `budget_request_inflight`. (V1.25 removes the remaining concurrency cap entirely.)
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

## V1.10

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
