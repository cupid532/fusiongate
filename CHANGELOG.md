# Changelog

## V1.53

- Merge the separate key-management modal into the channel-editing form as a tabbed layout. The provider edit form now shows three tabs — connection info, key management (with per-key model checkboxes, discovery and testing), and runtime settings. The channel-list row now has one primary action button (channel settings) that opens this unified editor, eliminating modal-hopping.

- Add a dedicated `opencode` channel type for routing traffic through OpenCode (Zen and Go), making OpenCode models available on Hermes via FusionGate without wiring the OpenCode key into Hermes itself. OpenCode's upstream is OpenAI-compatible, so the new type reuses the same chat-completions forwarding, model discovery (`/v1/models`), Bearer authentication, health probes, quality-detector support, and OpenRouter/OpenAI-compatible responses bridge as the existing `openai_compatible` type. It is selectable in the provider form (default base URL `https://opencode.ai/zen/v1`; point it at `.../zen/go/v1` for open models) and eligible for both codex_oauth-style Responses bridging and Anthropic-Messages-to-Chat bridge translation.
- Register the `opencode` type in every provider-type switch across admin validation, gateway routing, model discovery, upstream auth, health checks, and quality detection, so it is created, edited, discovered, health-checked, and load-balanced exactly like an OpenAI-compatible upstream.


## V1.52

- Fix the upstream-channel page. The provider form section was closed one tag too early in index.html, which let the channel-list table and the health-check panel escape the page providers section and rest under the document body. On screen the page header was followed by a large empty area, and you had to scroll down to reach the channels. Restoring the missing closing tag keeps the table-card and the health-check panel inside the providers section, so the channels now appear right below the page head.


## V1.51

- Fix the provider-channel form layout: add the missing `.field > span` label styling that had disappeared, causing label text to render with default browser spacing and making the form look broken. Widen the modal to 980px, increase form input height to 50px, bump interior form padding, and re-size the section step-badges.
- All `.provider-form-section` form-grids are now locked to 12 columns regardless of viewport, and span-4 fields keep one-third row width with proper fallbacks.

## V1.50

- Fix collapsed channel form fields. The provider form was caught between the generic narrow-screen form-grid rule (6 columns at max-width 1180px) and conflicting span overrides, which made priority, default model, default egress, and every advanced/balance field unpredictable. The provider form now always uses a 12-column grid regardless of viewport, span-4 fields take a third of the row on wide screens and half at mid-width, and the whole connection + runtime card layout is stable.


## V1.49

- Rebuild the add/edit channel “运行设置” card with generous full-width rows: priority, default model and default egress each take a quarter of the form, notes span the full width, and every advanced field (weight, concurrency, timeout, thresholds, circuit breaker, health checks) is now a quarter-width input instead of the cramped sixth-width boxes. The manual balance card uses the same quarter-width layout for the balance and the five category multipliers.
- Fix the root cause of the collapsed runtime settings: the `span-5` column class used by “渠道默认模型” and “渠道默认出口” was never defined in the stylesheet, so those two fields fell back to auto width while the section still read as broken. They now use a defined quarter-width class and the field height in the whole form is bumped to 48px for a steadier visual rhythm.
- Add helper text under the advanced and balance inputs so each runtime setting explains itself instead of appearing as a row of tiny unexplained boxes.


## V1.48

- Make the Base URL and first API Key fields full-width and taller in the add/edit channel form, so the connection inputs no longer feel like cramped half-row boxes. The channel name and provider type share one row, the API key gets two-thirds width with the key label beside it, and the URL/key inputs use a larger 50px height with wider padding and a softer focus ring.
- Add the missing `span-8` column rule and a narrow-screen fallback so the credential row never collapses on smaller viewports.

## V1.47

- Refresh the console toward a blue glassmorphism visual language: softer borders, translucent frosted surfaces, rounded grouped cards, blue primary actions, and a lighter, airier light theme — while keeping the same two-token theme layer and dark sidebar.
- Group the add/edit channel form into two clear cards (“连接信息” and “运行设置”) with a step badge, wider focus rings, and a sticky glassy action bar so the page no longer feels cramped or cluttered.
- Restore a direct “渠道设置” edit button on each channel row. The previous layout renamed the key manager button to “渠道设置” and hid the real edit button, which made it impossible to change an upstream API address without deleting and re-adding the channel.
- Show the exact reason when model discovery fails after creating a channel. The panel stays open in edit mode, lists every failed key with its upstream error, and offers a one-click “重试识别模型”; changing the API address and saving again now updates the same channel instead of requiring a re-add.
- Include per-key discovery errors in the create and re-run discovery responses so both paths can render one clear error per credential card.
- Add inline form error banners for save-time failures such as rejected upstream URLs, so the reason stays visible instead of flashing in a toast.

## V1.46

- Add an independent per-provider “本周期累计” cost cycle in the console. Channels without a configured balance start accumulating from the upgrade moment, OAuth credential files each accumulate on their own, and the running total is stored in its own table so ledger retention pruning can never shrink it.
- Restart the local cycle whenever a balance is saved (the balance baseline semantics stay unchanged), and reset local totals on balance additions, official quota rollovers, and redeemed reset cards — `request_ledger` history (“用量与费用”) is never rewritten.
- Detect official Codex quota period rollovers from the stable upstream `reset_at` window marker: the first observation records it, a later change resets the local cycle, and a window derived from `reset_after_seconds` is ignored because it moves on every refresh. Redeeming a GPT reset card now also zeroes the local cycle immediately.
- Seed existing manual-balance providers from their current baseline on upgrade so the new cycle matches the period the balance card already reports; every other provider starts tracking at upgrade time.

## V1.45

- Fix quality-detector disconnection after recreating the FusionGate container. The detector shares FusionGate's network namespace, so leaving the old sidecar running binds it to the removed container namespace even though Docker still reports the sidecar itself as healthy.
- Verify both `/readyz` and the configured detector status from inside FusionGate's current network namespace. Docker health, `fusiongatectl health`, and installer deployment checks now fail when the console would otherwise show “detector disconnected”.
- Make `fusiongatectl restart` recreate FusionGate and the quality-detector together, and document the same requirement for custom deployment procedures.

## V1.44

- Send normalized OpenAI Chat Completions, Responses, and Anthropic Messages text-generation requests upstream as streams even when the downstream client requested JSON. FusionGate now consumes SSE internally and rebuilds the original non-streaming response, preserving text, tool calls, stop reasons, usage, public model names, and existing client contracts.
- Keep true streaming clients on the low-latency passthrough path, retain transparent providers and non-text endpoints unchanged, and fall back to the existing JSON transforms when an OpenAI-compatible upstream ignores `stream=true`.
- Apply stream idle protection to internally buffered responses, retain provider-scale startup tolerance for non-streaming callers, cap buffered output at 32 MiB, and remove the production Caddy template's fixed 130-second response-header deadline for long JSON responses.

## V1.43

- Normalize `cache_control` prompt-cache markers so a `ttl="1h"` block never follows a shorter one. Claude Code mixes one-hour and five-minute cache markers in the same request, and Anthropic upstreams reject that ordering with `a ttl='1h' cache_control block must not come after a ttl='5m' cache_control block`, which surfaced as a bare HTTP 400 on every interactive session routed through an OpenAI-compatible Claude channel.
- Walk the markers in the order the upstream processes them — tools, then system, then the remaining messages — including OpenAI-compatible bodies, whose system-role messages are lifted into the Anthropic system array before the rest of the conversation.
- Resolve violations by downgrading the offending block to `5m` rather than promoting the earlier blocks, so a request never caches anything for longer than the client asked for. Compliant requests, and requests without any `cache_control`, are forwarded byte-for-byte as before, and transparent passthrough providers are untouched.

## V1.42

- Upgrade quality detection from a generic gateway-key probe to a one-time targeted run. Administrators now choose the declared GPT-5.6 model, the exact upstream channel, and the exact channel credential/API-key card before starting a low, medium, or high detector preset.
- Keep real upstream credentials inside FusionGate. The detector receives only a short-lived synthetic token that is accepted from loopback, restricted to one model, route, provider and provider-key card, capped by time and request count, and removed when the run finishes or is stopped.
- Attach the locked FusionGate target to live status and final reports so results remain attributable to the selected channel, key hint and upstream model, without placing the underlying secret in the browser, detector report or sidecar storage.

## V1.41

- Remove deprecated OpenAI Chat sampling fields `temperature` and `top_p` when an OpenAI-compatible channel targets a Claude 5 family model. Sub2API accepts the same request for Claude Sonnet 4.6 but returns HTTP 400 for Sonnet 5 with either field, so compatible clients can now use Sonnet 5 without changing their request defaults.
- Preserve those sampling fields for older Claude and non-Claude models, and leave native Anthropic Messages requests untouched.

## V1.40

- Add an administrator-only “质量检测” module directly below the request ledger. It runs the frozen low, medium, or high presets from `chen-006/gpt56_api_detector` against the current FusionGate Sol, Terra, or Luna route, and shows request cost estimates, live progress, stop controls, verdict summaries, failed evidence, limitations, and the raw JSON report.
- Keep the detector isolated as a loopback-only sidecar instead of copying its unlicensed source or executing its frontend inside the FusionGate administrator origin. The control API fixes the target to the current gateway, accepts only the three official presets and three GPT-5.6 model names, never persists the supplied FusionGate API key, disables raw request retention, and remains protected by the existing administrator session and CSRF checks.
- Pin sidecar builds to detector `4.0.1`, upstream commit `c0035f9695406ca0ebd00899e9c080294f894412`, and a verified source archive SHA-256. The production topology shares FusionGate's network namespace, exposes no detector port, includes Node.js for native Codex probes, and stores only detector sessions and reports in a dedicated volume.

## V1.39

- Stop repeatedly attempting model auto-discovery for expired externally managed OAuth credentials. FusionGate cannot rotate refresh tokens owned by CLIProxy, CPA, Sub2API, or equivalent source runtimes, so those cards now stay in the actionable “等待凭据更新” state until the source credential is imported again.
- Persist `expired/auth_expired` when an externally managed access token is found expired, and enforce the same skip rule in both the admin console queue and the backend batch endpoint so stale browser state cannot trigger another warning.

## V1.38

- Replace the usage-and-cost quick ranges with rolling 1-hour, 1-day, 7-day, and 30-day windows. Exact `from` and `to` timestamps now drive every shortcut instead of rounding the shorter ranges back to the start of a calendar day.
- Keep 30 days as the default, retain custom ranges, and label the daily UTC chart with the selected rolling window.

## V1.37

- Rework the admin console for phone-sized screens with 44 px touch targets, calmer two-column action layouts, single-column fallbacks on narrow devices, horizontally scrollable filters and tables, and viewport-safe full-width dialogs.
- Make the mobile navigation deterministic and accessible: add a full-screen dismiss backdrop, lock background scrolling, synchronize `aria-expanded` and labels, and close it after navigation, on Escape, or when the viewport changes.
- Finish the phone touch-target pass by turning provider balance links and provider/model ordering arrows into visible 44 px controls, while hiding drag handles that mobile browsers cannot operate reliably.

## V1.36

- Accept Claude Code interactive-session context entries that arrive as `messages[].role = "system"` and preserve their order when bridging Anthropic Messages requests to OpenAI-compatible Chat Completions. Claude Code 2.1.226 adds this dynamic context only in the interactive REPL, so print-mode probes succeeded while real terminal sessions failed locally with HTTP 400 before FusionGate contacted an upstream provider.
- Add a regression test covering the static top-level Anthropic system prompt, a later dynamic system message, and interactive-only tool schemas in the same request.

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
