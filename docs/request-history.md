# Request history API and reasoning metadata

## Host contract audited

Source reference: CPA **v8.0.22**, commit `67465884ca179a8f9098328d03a361b50003fdd7`. These are source observations, not confirmation of a deployed CPA's version or provider coverage.

- [`sdk/pluginapi/types.go`](https://github.com/router-for-me/CLIProxyAPI/blob/67465884ca179a8f9098328d03a361b50003fdd7/sdk/pluginapi/types.go): `ManagementRequest.Query` is `url.Values`, independent of `Path`; `UsageRecord.ReasoningEffort string` explicitly records requested reasoning effort. Native JSON fields use exported Go names.
- [`internal/pluginhost/adapters_usage_translation.go`](https://github.com/router-for-me/CLIProxyAPI/blob/67465884ca179a8f9098328d03a361b50003fdd7/internal/pluginhost/adapters_usage_translation.go): forwards `record.ReasoningEffort` unchanged into `pluginapi.UsageRecord.ReasoningEffort` for `usage.handle`.
- [`sdk/cliproxy/usage/manager.go`](https://github.com/router-for-me/CLIProxyAPI/blob/67465884ca179a8f9098328d03a361b50003fdd7/sdk/cliproxy/usage/manager.go): core record describes this as translated upstream thinking level for request event logs.
- [`internal/runtime/executor/helps/usage_helpers.go`](https://github.com/router-for-me/CLIProxyAPI/blob/67465884ca179a8f9098328d03a361b50003fdd7/internal/runtime/executor/helps/usage_helpers.go): reporter initializes from `usage.ReasoningEffortFromContext(ctx)` and `SetTranslatedReasoningEffort(payload, format)` can replace it with `thinking.ExtractTranslatedReasoningEffort(...)`; publishes it as `ReasoningEffort`.

Thus the plugin can reliably report **the field it received from its host**, not the unmodified original client body or the actual effort performed upstream. Host executors can vary in whether they set it and whether they replace request-context metadata with translated configuration. The callback does not expose that distinction. The plugin does not claim to independently verify the host or upstream.

Collection accepts explicit `ReasoningEffort` / `reasoning_effort` in the existing top-level or wrapped `Record`/`record`/`Usage`/`usage` callback. A nonempty, trimmed, valid UTF-8 string up to 128 bytes without controls is persisted verbatim as `reasoning_effort`, with `reasoning_effort_source: "host_usage"`. It is not an enum: numeric-budget descriptions and future host levels remain representable. No new request bodies, tokens, headers or secrets are stored. `Detail.ReasoningTokens` remains an independent token counter and is never converted into a level. Model/Alias suffixes are never used as reasoning-level evidence. Missing/invalid values and old history show **Not provided**, not a guessed default. Old JSONL files need no migration or backfill.

## Authenticated, read-only API

`GET /v0/management/plugins/cpa-mini-usage/history`

The same CPA management authentication as `/data` applies. No OAuth lookups, host callbacks, network requests, disk reads/writes or forecast mutations are performed by this route. `/data` retains its compatible `recent` array and seven-day statistics, adding `history_available: true` for capability detection. Older endpoints fall back to a clearly labeled 60-record snapshot with history controls disabled.

Parameters (single-valued; duplicates, unknown keys and values over 2,048 bytes are rejected):

| Parameter | Contract |
| --- | --- |
| `start` | Required RFC3339 timestamp, inclusive. |
| `end` | Required RFC3339 timestamp, exclusive, after start, span <= 37 x 24 hours. Queries outside retention return an empty result, not a fabricated history. |
| `provider` | Optional exact normalized provider identifier, max 64 ASCII lowercase letters/digits/`._-`; empty means all. |
| `model_status` | Optional exact `same`, `variant`, `different`, `unknown`; empty means all. UI mismatch selector sends **only `different`**. |
| `limit` | Integer 1–60, default 20. UI offers 20/40/60. |
| `cursor` | Opaque continuation from `next_cursor`; omit for first page. Keep all other parameters identical. |

The v0.1.23 UI leaves both date fields **empty by default**. When both are empty, it sends explicit RFC3339 instants `[Date.now() - 86,400,000 ms, Date.now())`, using one captured `now`: exactly the rolling last 24 hours, not today, yesterday plus today, or a local calendar day. The actual query interval is shown in browser local time. “Last 24 hours” clears both fields and starts a new query; clearing both manually does the same.

Explicit dates use the browser's local calendar, not the server's timezone. UI constructs local midnight on the start day and local midnight **after** the inclusive end day using calendar arithmetic, then sends the resulting UTC instants. This supports 23/25-hour DST days. A single filled field, invalid/reversed dates, or a span over 37 x 24 hours is rejected before HTTP; partial dates never silently switch to the rolling default. The server still independently requires valid explicit `start`/`end`. There is no requirement for a server-side IANA timezone database or the machine's timezone to match the browser.

Response:

```json
{
  "events": [{
    "at": "2026-10-09T10:24:00Z", "auth_id": "example", "provider": "codex", "model": "gpt-example",
    "reasoning_effort": "high", "reasoning_effort_source": "host_usage",
    "model_status": "different", "usage_quality": "complete", "missing_price": false,
    "cost": 0.0028, "credential_name": "Example account"
  }],
  "total": 124, "offset": 0, "limit": 20, "cursor": "this-page-opaque", "next_cursor": "next-page-opaque",
  "generated_at": "2026-10-09T10:25:00Z",
  "retention": {
    "days": 35, "max_records": 200000, "retained_records": 148,
    "oldest_at": "2026-09-10T10:24:00Z", "newest_at": "2026-10-09T10:24:00Z"
  }
}
```

Event rows use the existing `RecentEvent` structure, including token accounting, timing and current-price API-equivalent valuation. `credential_name` is optional when an old credential cannot be resolved; auth identity remains visible. Reasoning values/source are omitted when unavailable. Empty `events` is `[]`. `cursor` replays the current page at the same anchor, including page one when navigating backwards. `next_cursor` is absent on the last page. Retention metadata is unfiltered, anchored to the page snapshot; oldest/newest are absent when no eligible retained events exist. The cap can shorten the available time range, and installation never backfills pre-existing requests. Future-dated or older-than-retention events are not exposed. This API does not expand the seven-day quota/statistics window.

## Pagination and concurrency guarantees

- Timestamp descending, process-local ingestion sequence descending for ties; supports out-of-order timestamps.
- First page anchors the highest ingestion sequence and query time. Later appends, even with backdated timestamps, cannot shift pages or enter them until requerying.
- Cursor includes query fingerprint (dates/provider/status/limit), offset, anchor, and retention revision, authenticated by per-process random HMAC-SHA256. No client-supplied arbitrary offset is accepted. Bounds and event count are capped at 200,000.
- Expiry is 15 minutes from the first page, not extended on each page. Record eviction/compaction invalidates the cursor with `409 history_cursor_expired`. Invalid/tampered/query-changed/restarted cursors return `400 invalid_history_cursor`. Invalid parameters return `400 invalid_history_query`. Random-source or JSON encoding failures return HTTP 500, not empty success.
- Read lock protects the retained array, aliases and prices; only returned rows are priced, with no capture-time or disk-format rewrite. Sorting uses bounded lightweight pointers, not copied full request records.
- Frontend aborts superseded requests and times them out after 20 seconds; monotonically increasing request ids additionally discard stale completions even if the transport ignores cancellation. Credential changes/logout invalidate private rows and pending requests. Loading/error/empty states are separate, announced via live regions, and keyboard-labeled controls remain operable. Error text never echoes arbitrary backend payloads.
- Provider/date/mismatch/page-size changes reset to page one. The frontend freezes **all** query parameters (including exact rolling `start`/`end`) together with the cursor chain; next/previous pages never recompute `now`. Previous pages replay their cursor, including the first-page cursor. Explicit refresh/requery resets the snapshot and captures a fresh rolling anchor (or retains selected local-calendar dates). Minute sync captures a fresh rolling anchor on page one but leaves deeper pages and their range unchanged. Credential changes/logout clear the saved query and cursors. Seven-day credential/model summaries remain independent of history dates and mismatch filtering.

Name mismatch is diagnostic only: `different` compares the existing execution/usage model against the upstream-declared response name, excluding recognized variants and missing comparison data. Neither different nor matching names establish model substitution or authenticity.

## Browsing many accounts (v0.1.23)

The quota view has a labeled account search (case-insensitive, literal name/email/credential-ID matching), enabled/disabled filter, default/name/lowest-remaining-quota sort, and 12/24/48-account pages. Provider filtering is shared with requests; account search/status/sort/page size affect only quota cards. Sorting selects a page before provider grouping; lowest remaining uses the minimum known percentage across all windows/model groups, with wholly unknown accounts last. Provider headings show this-page/matching counts, and the browser summary shows matched versus all accounts. Credential IDs disambiguate repeated display names. Clear account filters also clears the shared provider filter, not request dates or mismatch selection.

Only the current account page is mounted/cached. Cards retain their full quota windows, model groups, reset credits, enable switch, quota-refresh button, errors and notices. In-flight jobs, errors, reset retry identity and transient notices live outside the card cache, so paging/searching cannot cancel an operation or erase its result; returning to a card restores current state. Pages clamp after account removal, and search/status/provider/page-size changes return to page one. Desktop browsing controls stay visible while scrolling; narrow layouts wrap with a normal-flow toolbar and a second pager below the cards. No runtime libraries, remote fonts or other external resources are introduced.

Open-source interaction references inspected:

- [Headlamp ResourceTable](https://github.com/kubernetes-sigs/headlamp/blob/main/frontend/src/components/common/Resource/ResourceTable.tsx) ([project](https://github.com/kubernetes-sigs/headlamp)): visible global resource search plus structured filters, with bounded resource presentation. Borrowed the searchable/filterable collection approach rather than rendering an unbounded wall of account cards; no Headlamp code/dependency was copied.
- [Grafana TimeRangePicker](https://github.com/grafana/grafana/blob/main/packages/grafana-ui/src/components/DateTimePickers/TimeRangePicker.tsx) ([project](https://github.com/grafana/grafana)): quick relative ranges alongside explicit time selection. Borrowed the explicit “Last 24 hours” return action and visible effective interval; conversion and signed-cursor anchoring remain this plugin's own implementation.

## Validation scope

Targeted Go tests cover parameter validation, local-offset/DST boundary interpretation, >60 records older than seven days, strict status/provider filters, exclusive end, deterministic ties, anchored paging during appends, signed cursor changes/restarts/expiry/removals, retention/count cap, concurrent reads/capture, reasoning capture/persistence and legacy unknowns, and native `ManagementRequest.Query` dispatch/response serialization. Dashboard module tests exercise the actual embedded script for empty default dates, exact rolling 24-hour bounds, frozen signed inputs across next/previous pages, page-one/deeper-page minute sync, refresh/requery, partial-date rejection, local calendar/DST conversion, filters/paging, stale-response races, authentication changes, errors/retry, bilingual metadata and literal text rendering. A 137-account fixture covers bounded rendering, ID/name search, enabled/disabled/provider filters, sorting, page sizes, removal/page clamping, preserved operations/errors off-page and language changes. Run `node --test testdata/dashboard_test.cjs` for this frontend module. The existing offline browser fixture exposes `visualFixture.showHistory()` and `visualFixture.show('many')` for synthetic history/many-account inspection without live credentials. Live OAuth accounts, the user's deployed CPA, and upstream execution levels cannot be validated by these checks.
