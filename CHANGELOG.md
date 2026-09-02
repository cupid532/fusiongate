# Changelog

## V2.74

- Space the Codex window label after 受限于 and in the over-limit warning. The hourly labels begin with a digit, so "受限于5 小时限制" ran together as one token.

## V2.73

- Align the Codex auth cards in a grid row. The grid stretched each card's wrapper to a common height but the card inside kept its natural height, so a Free account (one window row) stopped about 60px short of a Team account (two window rows) and their 参与调用 footers sat at different heights. Cards now fill the row height with the footer anchored to the bottom.
- Add a 思考强度 column to the request ledger, showing the upstream's own identifiers — minimal / low / medium / high / xhigh / max — untranslated, since those are the exact values used in API requests and provider docs. Higher-cost efforts are tinted; rows with no reported effort show a muted dash rather than fabricated data.

## V2.72

- Tidy the Codex plan subtitle: a single-window account now reads its full label ("每月限制") instead of the clipped "月窗口", and the two-window form spaces correctly as "5 小时 + 周 双限制".

## V2.71

- Name the Codex rate-limit windows in the auth-file card by their actual period, derived from `limit_window_seconds`: Plus and Team now read "5 小时限制" and "每周限制" instead of the meaningless "主窗口" / "次窗口", and a Free account reads "每月限制".
- Headline the window that is actually closest to its limit. The remaining-quota figure previously came from `remaining_quota`, which the backend derives from the primary window alone — on Plus and Team that is the 5-hour window, so a nearly-exhausted weekly allowance was hidden behind a full bar moments after the 5-hour window rolled over. The tighter of the two windows is now shown and labelled, and marked 「当前瓶颈」 in the per-window list.
- Give every window its own live reset countdown and elapsed-window percentage; previously only the primary window had one, leaving the weekly window — the one worth planning around — with no timer at all.
- Warn explicitly when the binding window is at or above 90% used, naming the window and when it resets.
- Show the plan as a readable badge (Free / Plus / Team / Pro …) with the limit shape it implies, rather than the bare lowercase identifier from the API.

## V2.70

- Reduce the sidebar footer to the version number. The gateway-health badge added in V2.67 read the providers list and, with 100+ channels configured, sat permanently on messages like "86 个渠道不稳定" — accurate but alarming and not actionable from the sidebar. Per-channel status stays on the 上游渠道 page, where every row already carries its own badge.

## V2.69

- Fix the dialog close button rendering at the bottom-left instead of the top-right in dialogs that override the content layout to a flex column (the per-Key model management dialog). V2.67 positioned it with the grid-only `justify-self-end`, which is inert in a flex column, so the zero-height wrapper fell to the end of the column. It is now first in DOM order and aligned with flex, staying pinned top-right in both layouts while still not scrolling away in tall dialogs.

## V2.68

- Add a 全部模型 / 已选模型 filter to the per-Key model management dialog, each showing a live count, so a Key's current selection can be reviewed on its own instead of being hunted for among every discovered model. The filter composes with the search box, and an empty selection is distinguished from an empty search result.

## V2.67

- Surface expired sessions instead of failing silently: any 401/403 now returns the console to the login screen with an explanation, rather than leaving a fully drawn UI whose every request failed behind it.
- Report every failed read and write. A single mutation- and query-cache error handler covers all of the console's mutations, replacing the handful of ad-hoc `onError` callbacks; failed table loads render a distinct error state with a retry instead of falling through to the "no data" empty state.
- Replace all native `confirm()` and `alert()` dialogs with themed in-app equivalents, and gate clearing the request ledger behind type-to-confirm.
- Return true ledger totals from `/api/admin/requests`: `total`, `limit`, `truncated`, and a `totals` aggregate computed over the whole filtered range. The console's summary tiles previously summed only the returned page, reporting the newest 100 rows as if they were the entire selection.
- Declare `color-scheme` per theme so native selects, number inputs, scrollbars, and focus rings follow the dark console instead of staying light.
- Resolve the theme before first paint via an inline script, removing the full-viewport light flash on every load, and stop freezing the OS preference into local storage on first mount.
- Fetch upstream balances only for channels that have one configured, cutting a fan-out of one request per channel down to the few that return data.
- Derive the sidebar's gateway status from live channel health instead of hard-coding "网关运行正常".
- Fix drag-to-reorder landing on the wrong side of the drop target when dragging downwards, and show where the row will land.
- Keep the dialog close button reachable in tall scrolling dialogs, and restore spacing between stacked dialog buttons on narrow screens.
- Debounce the request-ledger search so typing issues one query per pause rather than one per keystroke.
- Check the response status before saving an export, so an expired session no longer writes an error body to disk as a `.json` download.
- Open the channel editor from a row's name; the base URL stays as an explicit external link.
- Route unknown location hashes to the dashboard instead of rendering a blank page.
- Add `autocomplete`/`name` to the admin password field so password managers can save and fill it.
- Close the mobile navigation drawer on Escape, mark the active nav item with `aria-current`, and take the full-screen scrim out of the tab order.

## V2.57

- Fetch all paginated discovery results with deduplication, loop protection, cancellation, and bounded model counts.
- Make long per-Key model lists independently scrollable in the model-management dialog.
- Distinguish discovered, Key-enabled, publicly routed, and currently routable models so existing MoAPI routes are visible without misrepresenting Key selections.

## V2.56

- Separate public model routing from per-Key model management with explicit fallback and allowlist policies, including strict empty allowlists.
- Add transactional channel-level model-management saves and shared Key-model matching for gateway routing and health-check target expansion.
- Add a Key-aware model management dialog with per-Key discovery, draft changes, bulk save, model health details, and clear compatibility-mode warnings.

## V2.55

- Keep per-Key Select all and Select none actions while removing the health-based bulk selection shortcut.
- Add an independent “已选视图” toggle for every Key, showing only currently selected models without changing their selection state, with a one-click return to the full candidate list.

## V2.54

- Correct legacy auto-selection by clearing previously generated per-Key model selections once, so existing OpenRouter and other catalog inventories require explicit administrator choices.
- Distinguish candidate, selected, and health-verified model counts in every Key summary.
- Add per-Key bulk actions for Select all, Select none, and Select only health-verified models.

## V2.53

- Make per-Key discovery results a manual selection inventory: newly discovered models are visible but disabled by default, so only explicitly checked models participate in routing.
- Make discovery and model-management actions visibly labeled in channel editing, and show discovery errors inside the Key model manager instead of only exposing an icon.

## V2.52

- Complete per-Key model inventory controls: discovery no longer silently removes manually added or previously retained models, and every inventory row now has an explicit Delete model action.
- Add a dedicated backend `remove_models` operation so deleting a model removes both its Key inventory entry and its model health record without affecting other Keys.

## V2.51

- Remove the phantom undeletable Key row from channel editing: existing channels now render exactly their persisted Keys, and empty new-Key rows appear only after clicking Add Key.
- Allow every unsaved Key row to be removed, including the only row, while still requiring at least one actual Key before creating a new API-key channel.

## V2.50

- Restore per-Key model discovery and model-management actions directly inside channel editing, while keeping the full collapsible model inventory in the Key manager.
- Label Key cost multipliers explicitly and show them in Key summaries instead of presenting an unexplained numeric field.
- Clarify that channel balance-category multipliers convert recorded costs into balance consumption after the selected Key's cost multiplier has been applied.

## V2.49

- Treat migrated legacy credentials as ordinary `Key 1` cards that can be renamed or deleted, including when they are the final Key in a channel.
- Expose independent egress selection for every existing and newly added Key directly in channel settings, supporting inherited routing, direct connections, or a specific IP-pool node.
- Add independent per-Key cost multipliers, apply them to estimated and upstream-reported request costs, and preserve them in provider backups.

## V2.48

- Add collapsible per-Key model inventories to keep large model lists compact, with independent discovery, manual model addition, enablement, and health details for every upstream Key.
- Let API-key channels add multiple named Keys directly during channel creation or editing, while showing existing masked Keys in the same form.
- Route channel model management into the Key-aware workflow so model discovery and selection no longer merge credentials with different capabilities.

## V2.47

- Isolate every attributable upstream failure to the selected Provider Key, including Key-specific IP-pool transport errors, timeouts, and 5xx responses, so one broken Key or egress node cannot open the whole channel circuit.
- Persist Provider Key cooldown deadlines and restore them after process restarts; successful requests and operator edits clear the durable cooldown together with in-memory state.

## V2.46

- Preserve each provider Key model's enabled/disabled state across provider backup export and import; older backups without the field continue to restore models as enabled.
- Enforce the 500-Key-per-provider ceiling atomically in SQLite, preventing concurrent create requests from exceeding the application limit.

## V2.45

- Complete per-Key isolation for multi-Key upstream channels: each Key now has independent health-check participation, persistent Key-by-model health results, discovered model inventory, and effective IP-pool egress without affecting sibling credentials.
- Attribute every upstream request attempt to the selected provider Key using its stored name and masked hint; request-ledger search and the console expose this safe attribution without persisting plaintext credentials.
- Expand the Key management console with independent enablement and health controls, model discovery and testing actions, model-level health details, and effective egress visibility while preserving the provider health switch as the channel-wide master control.

## V2.44

- Increase the CI race-suite timeout to accommodate slower hosted runners and prevent valid release builds from failing at the previous ten-minute limit.

## V2.43

- Fix usage heatmap aggregation across mixed upstream models and use full dates for cross-month columns and tooltips.
- Make quality-detector persistence failures stop jobs instead of allowing them to be reported as completed.
- Batch route Provider Key selection through one request-local reader-pool query while preserving existing routing behavior.
- Run frontend lint, tests, and builds in CI, and generate fresh Vite assets when building production Docker images.

## V2.42

- Add one-click copy controls with visible success feedback for the overview Base URL and revealed downstream API keys, including a fallback for browsers without the modern Clipboard API.
- Fix the overview cost card to format micro-dollar values exactly once and align the channel-health total with enabled, non-archived channels.
- Improve compact-screen key creation layout and add accessible labels to filtering and routing controls.
- Remove unused UI exports and duplicate frontend response types, and resolve the request-ledger memoization lint warning.

## V2.41

- Replace the provider balance progress-bar percentage label with the remaining balance amount while preserving the existing usage-based progress bar.

## V2.40

- Fix request-ledger live elapsed badges to subtract the request creation timestamp instead of rendering a Unix timestamp-sized value.

## V2.39

- Break the request-ledger token column into granular parts (input, cached, reasoning, output) instead of a single total.
- Show live per-second elapsed clocks for running requests, distinguishing waiting-for-first-byte from streaming, timed against the server clock.
- Flag running rows that have outrun every plausible completion window as suspected-stalled.
- Reconcile orphaned ledger rows: startup closes rows left open by a previous process, and a periodic sweep force-closes rows older than two hours.

## V2.38

- Treat failover as exhaustive by default: a request now tries its entire in-request candidate plan before returning failure instead of stopping at a fixed 15-attempt cap. Larger channel fleets simply get more attempts; termination behavior, statuses and response headers for explicitly configured caps are unchanged. Set `FUSIONGATE_MAX_FAILOVER_ATTEMPTS=N` (N >= 1) to re-enable the optional fuse cap.

## V2.37

- Rename the routing strategy labels in the console to describe how each new request picks its starting channel (fixed by priority, fixed by configured order, rotating across channels, or adaptive weighted scoring) instead of overloading "failover" - all four strategies share identical in-request failover and circuit protection, so the old names obscured the real difference and `ordered_round_robin` never actually rotated.
- Share one label/help source of truth (`ROUTING_STRATEGY_LABELS` / `ROUTING_STRATEGY_HELP`) between the upstream-channels page and the model-routing page; the channels selector now shows an inline explanation of the selected strategy instead of unlabeled options.

## V2.36


- Make model aliases and their canonical model share the same smart-round-robin cursor and adaptive weight state while continuing to preserve the client-requested model name in responses and request accounting.
- Rebuild the model-routing console around canonical failover groups: every card now combines call aliases and channel members, shows configured versus currently schedulable routes, explains channel/route/circuit exclusions, and mirrors the configured strategy order.
- Add fast `/<model>` call-alias creation for both canonical and upstream model names plus custom aliases directly inside each model group, with inline enable/delete controls. Route creation can select an existing failover group, and existing routes can be moved interactively between groups without changing their upstream model IDs.
- Expose provider priority, configured position, archive state and circuit cooldown through the admin route response so the console can explain the real scheduling state instead of treating an enabled route on a disabled channel as available.
- Rename the routing strategy labels in the console to distinguish fixed-start sequential failover from request-rotating distribution, and allow the strategy to be inspected and changed directly on the model-routing page.

## V2.35

- Rework the usage heatmap to carry the full token breakdown (input / cached / output / reasoning) plus real cost per model-day cell, and let the console switch the heatmap coloring between total, input, cached, output, and cost.
- Fix the heatmap tooltip showing a hardcoded zero cost; it now renders the precise input / cached / output split and the real cost for each cell.
- Replace the single-color daily token trend with a stacked input / cached / output bar so usage is no longer reduced to a single token figure.
- Show the input / cached / output breakdown on model, key, and provider ranking rows instead of a bare total-token count.
- Fix a stale-closure issue in health-check job polling and stabilize the quality-detector target list to avoid needless re-memoization.
- Remove the dead `completedResponsesSSE` helper and normalize error strings to lowercase-leading per Go conventions.

## V2.34

- Generalize protocol selection with provider-level `auto` and `fixed` policies plus an ordered `protocol_preference` (`responses`, `messages`, `chat`).
- Discover native Responses support for Anthropic-compatible API-key providers during model discovery and persist `protocol:responses` on imported routes only after a successful generation probe.
- Preserve the route capability as the safe runtime contract: Responses and Chat clients use native upstream Responses only when discovery or an operator has declared support; native Messages clients remain on `/v1/messages`.
- Retire stale learned Responses support when an upstream returns an explicit protocol/endpoint error, preventing repeated probes on later requests.
- Include protocol policy and preference in provider APIs and provider backup/export imports, with validation and safe defaults.

## V2.33

- Add route-level `protocol:responses` opt-in for Anthropic-compatible providers that expose both `/v1/messages` and `/v1/responses`.
- OpenAI Responses clients now use the provider's native Responses endpoint while native Anthropic Messages clients keep using `/v1/messages`.
- OpenAI Chat clients on opted-in routes are bridged through upstream Responses, avoiding broken or slow Chat Completions compatibility endpoints.
- Keep ordinary Anthropic providers safe by excluding them from Responses routing unless the route explicitly declares support.

## V2.32

- Make normalized OpenAI-compatible channels Responses-first: `/v1/responses` now tries the upstream Responses endpoint before falling back on the same route to Chat Completions only when Responses fails before downstream output is committed. Authentication and rate-limit errors do not trigger the redundant protocol retry.
- Return `X-FusionGate-Upstream-Protocol: responses|chat` so clients and operators can verify which upstream protocol ultimately served a Responses request.

## V2.31

- Manage the request ledger by capacity instead of a fixed row limit. The console now shows current vs. configured usage (in MB, default 100, adjustable 1 MB – 10 GB) with a progress bar, an editable cap that trims immediately when lowered, one-click clear, and JSON export that honors the current time-range and status filters.
- Back the feature with new endpoints: `GET/PUT /api/admin/ledger` (status + capacity cap), `POST /api/admin/ledger/clear`, and `GET /api/admin/ledger/export`. The periodic retention loop now also trims the oldest rows whenever estimated on-disk usage exceeds the cap.

## V2.30

- Allow importing credentials from a local `.json` file in the auth-files import dialog instead of only pasting. A file picker loads the JSON into the editor, keeps the pasted-text fallback, and validates the file extension.

## V2.29

- Rework the console around a heavier operational-analytics suite. The usage page is now a multi-view analyzer with Overview / Trends / Model / Key / Provider / Heatmap tabs, per-dimension ranking with one-click drill-down, and a model-by-day usage heatmap backed by a new `heatmap` capability on the `/api/admin/token-usage` endpoint (top models by token volume, log-scaled).
- Add a grouped view to the request ledger with an at-a-glance summary strip (requests, successes, failures, tokens and cost) plus per-model groupings and share bars.
- Add a provider-health and aggregate-status panel to the dashboard, surfacing health rating, per-state channel counts, and 24h request/failure totals.
- Add global console actions to the top bar: refresh-all (invalidates every active query), a link to the GitHub repository, and a running version badge.
- Introduce reusable console primitives: `EmptyState`, `SegmentedTabs`, `StatCard`, and a lightweight `Heatmap` component.

## V2.19

- Extend the unified `/v1` entry point to cover the full agent modality set: text-to-speech (`/v1/audio/speech`), speech-to-text transcriptions (`/v1/audio/transcriptions`, multipart passthrough) and vector embeddings (`/v1/embeddings`). All three reuse the existing capability-routing and failover pipeline (`audio_speech` / `audio_transcribe` / `embedding` route capabilities), so circuit breakers, half-open recovery, IP-pool egress and the request ledger apply unchanged.
- Add an `allow_audio` per-Key permission alongside `allow_images`; keys without it receive `403 audio_not_allowed` on both audio endpoints. Embeddings remain governed by the existing model allow/deny lists.

## V2.18

- Rebuild quality detection into a selectable, batchable workflow. Administrators can filter GPT-5.6 models, channels, and per-channel keys, then run a single target or a queued batch of up to 100 targets sequentially through the frozen detector presets; each target is re-resolved before it runs, a per-item failure continues the queue, and the batch can be cancelled at any time.
- Persist a redacted 24-hour quality-detection history (batches and per-target verdicts, errors, and reports) in SQLite. Reports are stored only when they declare `auth_values_persisted=false` and pass recursive key/secret redaction plus a size cap; upstream keys, route tokens, and detector session tokens are never written.
- Keep the existing single-target start/status/report/stop endpoints compatible while routing them through the same targeted-token safety boundary (loopback-only `POST /v1/responses`, exact model/route/channel/key binding, no failover).

## V2.17

- Adapt the console for mobile: the navigation collapses into a hamburger-triggered drawer, page headers and action bars stack instead of overflowing, dialogs and form grids resize to fit narrow screens, and the login layout drops to a single column with the brand header shown inline.

## V2.16

- Pool routes by their final upstream model across public model names. A route such as `gpt-5.6-luna -> deepseek-v4-flash` now participates at equal configured priority with native `deepseek-v4-flash` routes, without double-counting providers that expose both names.

## V2.15

- Hide provider and authentication selection checkboxes behind a top-level multi-select mode, and restore provider drag-and-drop ordering with a dedicated drag handle in normal mode.

## V2.14

- Fix explicit authentication-file model discovery: selected credentials now refresh their upstream model inventory even when inherited or previously configured routes already exist.

## V2.13

- Complete authentication-file management with model discovery, checkbox-based model enablement, same-platform batch model settings, and batch network-egress assignment.

## V2.12

- Add inline priority editing to provider and authentication-file lists, with compact priority controls and reduced status-column clutter.

## V2.11

- Make channel and authentication-file priorities explicit in the console, including editable priority on provider creation and OAuth/import flows.
- Increase default request failover to 15 attempts and open a channel circuit after 5 consecutive failures. Successful health probes immediately clear the failure counter and circuit cooldown.

## V2.10

- Keep Codex quota details visible when an authentication is disabled. Closing the participation switch still excludes the credential from model routing and health checks, while the card continues to show remaining quota, usage windows, reset countdowns, and the next reset time with manual refresh available.

## V2.09

- Ship the rebuilt embedded console assets for the Codex authentication switch and provider-dialog fixes introduced in V2.07-V2.08, ensuring the production binary serves the updated interface.

## V2.08

- Complete disabled-auth isolation by rejecting manual health checks for disabled providers, so a closed Codex credential cannot make upstream calls through routing, polling, refresh, automatic checks, or operator-triggered checks.
- Correct provider group clearing and reset stale save errors when the channel dialog is edited or reopened.

## V2.07

- Add an on/off control to each Codex authentication card. Disabled credentials are visibly marked, stop quota polling, and are excluded from routing, automatic health checks, and token refresh until re-enabled.
- Fix upstream channel creation when a provider group is selected by accepting and persisting the form's `group_id`. The channel dialog now requires an API key for new channels and displays the backend's concrete validation or conflict error instead of failing silently.

## V2.06

- Add model aliases with admin APIs and console management. Aliases enter the existing priority failover, round-robin, smart round-robin, and adaptive schedulers under their canonical model while preserving the requested alias in downstream responses and request accounting. Alias names and targets are transactionally protected from route conflicts, alias chains, transparent-only routes, and dangling targets, and aliases are included in provider backups.
- Complete agent protocol compatibility for OpenCode, Codex CLI, and Claude Code. OpenCode Zen now selects the correct upstream protocol per model family (Responses, Anthropic Messages, Gemini, or Chat Completions); Codex supports `/v1/responses/compact` and preserves turn state; Claude Code supports `/v1/messages/count_tokens`, Anthropic request IDs and error envelopes, and incremental tool-input streaming.
- Expand route and pricing management in the console with real route creation, per-route official/manual price controls, standard and long-context input/cache/output prices, official-first pricing synchronization with OpenRouter fallback, source/update metadata, and global synchronization status.
- Harden public protocol translation and routing consistency: normalized Chat, Responses, and Messages output keeps the requested public model name; local token estimates are health-neutral; provider/client policies still apply; health probes follow OpenCode's model-specific protocol; and native Anthropic errors preserve upstream correlation headers.

## V2.05

- Model picker as an on/off manager: enabled models are grouped at the top (checked), the rest below (unchecked), and saving applies the checked set as the provider complete model set — checking enables, unchecking disables/removes the route.

## V2.04

- Codex reset-card feedback and reset countdown: the reset-card button is always visible (shows a hint when there are no cards, asks for confirmation before redeeming, and shows success/failure feedback), and the next-reset time is now an animated countdown bar driven by the usage window.

## V2.03

- Fix auth-file platform grouping: the console grouped by `auth_source` (which stores values like `fusiongate_oauth` / `cliproxy`), so the Codex card view never matched. Grouping now derives the platform from the provider `type` (`codex_oauth` -> codex, `grok_oauth` -> grok, `claude_oauth` -> claude), so Codex accounts render as cards with quota and reset cards.

## V2.02

- Codex auth files as cards: each Codex account is now a card showing plan type, remaining quota with an animated progress bar, primary/secondary usage windows, next reset time, credits balance, and reset cards (count, per-card details, and one-click redeem).

## V2.01

- Provider links, archive, IP egress and grouping: channel names are now links to their upstream site, channels can be archived/unarchived, the editor lets you assign an IP-pool egress and a provider group, and a group manager creates/deletes groups.
- Precision health checks: the health-check dialog lists the channel's models and keys so you can check all models at once, a selected subset, and pin specific keys to specific models.
- Auth-file batch actions: multi-select auth files to batch health-check or batch export them.

## V2.00

- Complete console frontend rewrite on React 19 + Vite + TypeScript + Tailwind CSS + shadcn/ui + Motion (Framer Motion) + TanStack Query. All nine pages (login, overview, providers, keys, requests, routes, usage, quality, IP pool, auth files) are migrated from the old hand-written JS/CSS/HTML single page to a componentized React app with animated count-up stat cards, spring nav transitions, an animated usage chart, and a warm natural design system with a real dark theme. The Go embed now serves the Vite build's hashed, immutably-cached assets. Data loading uses TanStack Query caching and diff-based re-rendering, which also resolves the earlier Firefox + password-manager jank by eliminating full-table `innerHTML` rebuilds.

## V1.83

- Rebuild the console frontend on React 19 + Vite + TypeScript + Tailwind CSS + shadcn/ui + Motion (Framer Motion) + TanStack Query. This is the first milestone: the engineering skeleton, the login page, the overview (dashboard) page with animated count-up stat cards, the theme toggle, and a reworked Go embed that serves the Vite build's hashed assets from `internal/fusiongate/ui`. The remaining pages (providers, routes, keys, requests, usage, quality, auth files, IP pool) are still placeholders and will be migrated next.
- Replace the old hand-written JS/CSS/HTML console and its content-snapshot tests with the new embed tests.

## V1.73

- Deterministic Firefox reliability pass. Detect Firefox via UA at boot and tag `<html>` with an `is-ff` class, then drop the expensive backdrop-filter blur and SVG transform animations only for Firefox (bars fade via opacity instead). Also relax the quality-detector poll (1.5s -> 5s) and the request-ledger poll (3s -> 5s) so the page stays visually static, reducing both GPU load and churn seen by passive DOM-scanning extensions.

## V1.72

- Make the request ledger fully quiet in steady state. The poll now compares a data snapshot and skips rendering entirely when nothing changed, and the poll intervals are relaxed (1.5s -> 3s, running-clock 1s -> 2s). With no in-flight requests the page performs zero DOM mutations, so passive third-party extensions (e.g. password managers with MutationObserver-based form scanning) stop burning CPU on it.

## V1.71

- Replace the request-ledger full-table rebuild with incremental diff rendering. On the steady state the table only patches the running rows' live duration cell (and re-renders a single row when a request transitions to finished), instead of rewriting the entire `<tbody>` on every 1s / 1.5s poll. This removes the remaining Firefox jank on the requests page.

## V1.70

- Improve Firefox responsiveness. Firefox renders `backdrop-filter` blur far slower than Chrome, so the sidebar, topbar, and modal surfaces drop the blur in Firefox and use opaque backgrounds instead; table rows no longer animate on every poll; and the running-request clock re-renders once per second instead of every 250 ms. Also honor the `prefers-reduced-motion` preference by disabling animations and transitions.

## V1.69

- Restore a real dark theme. The earlier warm-natural redesign had also recolored the default (`:root`) palette into the same light cream, so toggling the theme had no visible effect. The default theme is now a warm dark palette (deep brown-black surfaces, off-white text, brighter green accent) while the light theme keeps the Octopus-inspired cream palette.

## V1.68

- Fix a stray closing brace left in the light-theme variable block that made the browser drop the whole `html[data-theme="light"]` rule group and silently discard the content-area padding rules that followed it. Page content (overview heading, stat cards, and every other page) had been rendered flush against the sidebar with no horizontal padding; the content container now applies its intended `34px` side padding and `1680px` max width again.

## V1.67

- Add motion and data-viz polish inspired by Octopus: a unified 0.15s smooth transition curve across cards, buttons, and navigation; hover lift with soft shadow on stat/usage/rank cards; animated stat number count-up on the overview and usage pages; and animated SVG charts (bars grow from the baseline, the trend line draws itself, and rank bars fill in).

## V1.66

- Redesign the console UI with a warm, natural color system inspired by Octopus: cream page background, white surfaces, forest-green accent, and earthy brown text, replacing the previous dark navy/blue palette. Both the default and light themes share the new tokens.
- Fix the light-theme variable block so the new palette actually applies when the console is in light mode (the previous build emitted an invalid nested selector and silently fell back to the old colors).

## V1.65

- Fix the Codex OAuth chat bridge for models such as `gpt-5.6-sol`. The ChatGPT Codex backend streams OpenAI Responses SSE without a `Content-Type` header, so FusionGate skipped its buffered Responses-to-Chat conversion and forwarded raw `response.created` events to Chat Completions clients, which failed strict stream validation. Upstream responses are now treated as SSE whenever the caller declared an SSE upstream, so Codex-backed chat requests are converted back into `chat.completion.chunk` frames.

## V1.64

- Fix upstream URL construction for OpenAI-compatible channels whose base URL already carries the API version path, such as the OpenCode Zen/Go channel (`https://opencode.ai/zen/go/v1`). Requests were being forwarded to a doubled `/v1/v1/chat/completions` path and failing with HTTP 404, which made OpenCode-served domestic models (DeepSeek, GLM, Kimi, Qwen) unusable through FusionGate. The version prefix is now de-duplicated for request routing and provider health probes alike.

## V1.63

- Add batch actions for authentication files of the same type: batch-select models and batch-change network egress across many accounts at once instead of editing them one by one. Model selection discovers once from the first account and applies the same enabled-model set to every selected account; egress applies one IP pool node (or direct) to all selected accounts in a single transaction.

## V1.62

- Normalize streamed OpenAI Responses events for strict clients such as OpenCode: add canonical `event:` names, preserve event boundaries, and remove private Codex-only response fields that are not accepted by the public Responses schema.
- Unify OAuth credentials by platform. Accounts of the same type share one canonical model discovery result and inherit one network egress, preventing model and route drift across large credential pools.
- Fix container health checks for wildcard-bound services and keep the independently monitored quality-detector sidecar out of the FusionGate core readiness probe.

## V1.61

- Export authentication files one account per JSON file. A single selected account downloads a standalone JSON; selecting several downloads a ZIP whose entries are one JSON per account, using the same per-account shape the importer accepts.

## V1.60

- Repair the unified channel editor introduced in V1.59: isolate asynchronous provider loads, reset balance-clear state between channels, protect unsaved Key edits, expose invalid fields in the correct tab, remove the obsolete duplicate Key modal, restore mobile overrides, and add accessible tab semantics.
- Prevent deleted provider credentials from surviving through the legacy backup fallback. Deleting the final Key now clears the legacy credential and disables the channel atomically.
- Fix native Gemini chat response text extraction, OAuth provider-name error handling, Codex reset-card request validation and retry safety, exact budget-exhaustion responses, gateway-owned response headers, route priority ordering, cooldown caps, pricing restore safety, and database readiness signaling.
- Make deployments traceable to one exact source revision through binary build metadata, OCI image labels, health endpoints, and tracked production Compose templates. Add update rollback, portable backup verification, backup retention, log rotation, and bounded build-cache cleanup.

## V1.59

- Rebuild the unified channel editor after the V1.53-V1.58 rollback sequence, restoring connection, Key management, and runtime tabs on the stable V1.52 console base.

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
