# CPA Mini Usage

A lightweight native CPA v8 plugin (Linux amd64) with **exactly two tabs**:

1. **Quotas:** separate Claude / Codex sections showing five-hour / seven-day remaining percentages and resets. Each window displays **Used (observed)** and **Estimated total** side by side, with API-equivalent USD amounts on one row and tokens on the next. Used values include only requests observed by this CPA instance within that window, not official bills or activity from other clients.
2. **Requests:** recent requests with explicit model names, total input/output, cache read/write, per-request estimated USD cost and timing, plus per-credential/model usage. Tokens use `k / m / b`; hover for full counts and full USD precision. No uncached-input column is shown.

No React, no Node runtime, no separate web service, no inference probes, no database process.

## Install

Download the latest Linux amd64 ZIP from [GitHub Releases](https://github.com/chilin11/cpa-mini-usage/releases), extract `cpa-mini-usage.so`, or build from source. The legacy binaries under `dist/` are v0.1.0 and should not be used:

```sh
CGO_ENABLED=1 go build -buildmode=c-shared -o cpa-mini-usage.so .
```

Copy the `.so` into the CPA plugins directory (`plugins/linux/amd64/` or `plugins/`) and merge:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    cpa-mini-usage:
      enabled: true
      priority: 10
```

Restart CPA, then open **Mini Usage** in the plugin menu or:

`http://YOUR_CPA_HOST:8317/v0/resource/plugins/cpa-mini-usage/dashboard`

The dashboard automatically reuses the saved login from the **same-origin CPA management UI** (including its encoded `cli-proxy-auth` storage). Log in there with “Remember password” selected. The plugin has no address/key inputs, never persists a key, and sends credentials only to the same-origin CPA Management API. Cross-origin management pages and memory-only logins cannot be reused without a host credential bridge.

### Data and persistence

**Save data** is enabled by default. Requests (including measured first-token time and total latency), quota snapshots, account identities, price cache, and the server-side setting are stored separately from the plugin under `data/cpa-mini-usage/`. Set `CPA_MINI_USAGE_DATA_DIR` to use another persistent directory; hover over the dashboard storage control to see its path. Replacing/upgrading the `.so` does not replace these files. Disabling persistence stops further usage/quota/price writes but never deletes existing saved history; enabling flushes current in-memory data. Recent request storage retains up to 35 days / 200,000 records. If the directory is not writable, the UI reports a storage error. Collection starts at installation time; prior request history is **not backfilled**.

For Docker, bind-mount this directory or use a named volume, for example set `CPA_MINI_USAGE_DATA_DIR=/data/cpa-mini-usage` and mount `./mini-usage-data:/data/cpa-mini-usage`. A browser toggle cannot preserve files from a container that is deleted without a persistent mount.

### Request timing

The recent request table has a **Time** column with separate **First token** and **Total** lines in seconds, alongside **TPS = output tokens / total latency seconds**, following [CPA Manager Plus request TPS](https://github.com/seakee/CPA-Manager-Plus/blob/dbf61ddf438f399558722df3467355c42af9005d/apps/web/src/features/monitoring/model/eventRows.ts). This is request-average output throughput, including waiting, not pure decode speed; input/cache tokens never enter the numerator. TTFT is displayed independently and is not subtracted. Missing TTFT/non-streaming requests can still show TPS when total latency and output are available; failed requests, missing/nonpositive latency or output show unknown. CPA durations are nanoseconds internally, converted to milliseconds and displayed in seconds.

Audited benchmark definitions differ: [vLLM](https://github.com/vllm-project/vllm/blob/193922d6ff5f7e213eacadd3cb7dd96612b30ed3/vllm/benchmarks/serve.py) and [SGLang](https://github.com/sgl-project/sglang/blob/78218194adf0c6bef69aa6f71919751fee9f3834/python/sglang/benchmark/serving.py) calculate TPOT as `(latency − TTFT) / (output tokens − 1)` with a shared client timing basis; its inverse is post-first-token speed. [llama.cpp](https://github.com/ggml-org/llama.cpp/blob/10a60cf303566e10d6a7a2774c17d2085503d87b/tools/ui/src/lib/components/app/chat/ChatMessages/ChatMessageStatistics/ChatMessageStatistics.svelte) uses server-reported generation count/time. CPA 8.0.22 starts latency at reporter creation but may start TTFT at upstream transport and may fall back to the first packet. The plugin cannot derive an exact pure-decode interval from these two fields, so it does not pretend the subtraction is one.

### Limits of estimates

- Percentages and reset times are read from the provider's **OAuth quota endpoint** and are not calculated from token counts.
- Actual tokens are captured from requests handled by **this CPA instance**, not requests made on other machines/clients directly to the provider.
- Estimated total tokens / dollars follow [CPA Manager Plus `estimateWindowUsage`](https://github.com/seakee/CPA-Manager-Plus/blob/dbf61ddf438f399558722df3467355c42af9005d/apps/web/src/features/accounts/model/estimateWindowUsage.ts): **current observed usage × 100 / provider reported used percent**, with tokens rounded to integers and USD to cents, never below current observed usage. These are totals, not remaining values; no remaining fraction is multiplied. The official percentage still shows remaining. Current actuals continue updating, but a quota-based forecast is withheld if requests are ahead of the quota observation. When that basis is unavailable, observed previous-cycle actuals can be used as a labeled fallback only when consistent with current actuals; no previous records means no fallback. Missing prices preserve token figures but leave USD unknown. This is an extrapolation, not an official cap or guaranteed balance.
- Prices refresh from `https://models.dev/api.json` every 6 hours. The normalized ordinary-input/cache-read/cache-write/output buckets follow [CPA Manager Plus cost accounting](https://github.com/seakee/CPA-Manager-Plus/blob/dbf61ddf438f399558722df3467355c42af9005d/apps/manager-server/internal/service/pricing/cost.go) and the CPA/CodexBar provider contracts: OpenAI caches are subsets of total input; Anthropic ordinary input/cache read/cache creation are independent raw classes. Explicit cache-read zero takes precedence over legacy cached tokens. Reasoning already included in output is not charged twice. Context price tiers use declared thresholds. Per-request and aggregate costs share the same backend calculation; missing models are unknown, not $0. Price books, service-tier metadata and historical coverage may differ from Manager Plus, so matching formulas do not guarantee identical amounts.
- Historical costs are revalued using the latest known prices, and represent **API-equivalent cost**, not an actual Claude Pro/Max or ChatGPT subscription charge.
- All-account quota checks run every 15 minutes, with at most four account reads in parallel. Each quota card can refresh just its own credential; the top button refreshes all. Single-account reads do not advance the automatic full-scan schedule. Concurrent refresh requests return a busy response instead of silently dropping work. The visible dashboard syncs cached snapshots every minute and updates countdown text every 30 seconds; these reads do not query the providers. Manual refresh polls completion every 2 seconds. Only read-only GET requests to known quota endpoints are made.
- Supports Claude OAuth and Codex OAuth; API keys do not expose subscription quota windows.
- If `usage.handle` is not dispatched by your CPA build, the quota still works but per-request stats cannot be collected. Upgrade CPA to a build with the usage callback fix.

### Security

Resource HTML is public by design; actual usage/quota data comes only from **management-key-protected** plugin routes. Upstream credential tokens and CPA management keys are not written to disk, emitted in API responses, or included in logs. OAuth lookups are restricted to known HTTPS hosts.

### Limitations

Registration and dashboard/API access are tested against the real CPA 8.0.22 Linux amd64 release in CI, but OAuth reads are not tested with your live accounts. Provider schemas may change. Actual account emails are shown only in authenticated data responses. Manual-reset remaining counts appear only when an explicit supported field is returned; missing counts are omitted, never inferred from timestamps or credits.

### v0.1.1 registration fix (CPA 8.0.22)

Version 0.1.0 used an incorrect `github_repository` registration field. CPA requires
`GitHubRepository` and rejects the plugin when that metadata field is missing.
Refresh the plugin store source, upgrade to v0.1.1, enable the plugin, then restart CPA.
CI now tests registration, dashboard access, and management API authentication using
the real CPA 8.0.22 Linux amd64 release before publishing a new version.

### v0.1.2

Compact token counts, remaining quota, provider sections, actual account names without `.json`, model columns on recent requests, faster refresh feedback, and saved CPA login reuse. Manual-reset counts are hidden when unavailable.

### v0.1.3

Quota cards show the official remaining percentage first, the estimated USD value second at a similar moderate size, and readable compact tokens beneath it. Reset time is a days/hours/minutes countdown, with the full timestamp in a tooltip. Codex manual resets now use the official `rate_limit_reset_credits.available_count` field, with a read-only fallback to `/backend-api/wham/rate-limit-reset-credits` (`available_count`). This only lists remaining reset credits; the `/consume` endpoint is not permitted or called. Unknown counts remain hidden.

### v0.1.4

Simplified, shorter cards remove duplicate totals, badges, and explanatory paragraphs. Remaining percentage and USD stay at 22px, tokens at 14px. Reset bank appears beside the account name with an 18px bold count; unavailable counts remain hidden and explicit zero remains visible. Less frequent background refresh is silent, preserves unchanged content and focus, and keeps the last snapshot on temporary network errors while still clearing private data on logout/authentication failure. Fixed numerical tests cover per-million-token prices, cache read/write handling, missing prices, and percentage-based remaining-value arithmetic.

### v0.1.5

Per-account quota refresh buttons and less frequent 15-minute all-account checks. Only the changed account card updates; other cards keep their nodes. Refresh completion and failures are tracked independently for the selected credential. A Chinese/English selector defaults to the browser language and remembers only the language preference. Labels, tables, alerts, accessibility text, and countdowns switch together. Reset counts use “可重置” / “Resets remaining”, with no Reset bank or Manual reset UI labels.

Recent requests additionally show first-token time and generation-phase TPS. A server-side Save data switch persists request history/timings, quota snapshots, pricing, and its own preference in an independent directory that survives plugin replacement. Both languages use token/tokens rather than “词元”.

The requests tab has its own GET-only Refresh requests button, never queries OAuth quotas, and shows 20 rows by default with 40/60-row options in a wider, compact table. First-token time is displayed in seconds. Cache accounting and explicit long-context price tiers are regression-tested against the public CodexBar/CPA contracts. CodexBar's [quota-window summaries](https://github.com/steipete/CodexBar/blob/main/Sources/CodexBarCore/CostUsageModels%2BQuotaWindows.swift) aggregate observed usage separately from official percentages; the remaining-token extrapolation here is not claimed to be CodexBar's official balance.

### v0.1.6

Remove the artificial 5%-used threshold: any positive utilized fraction with observed requests can produce a total-value estimate. Recent and credential tables display total input, output and cache read/write; the uncached-input column has been removed. Claude total input includes its independent cache classes; Codex total input already includes cache and is not added twice. TPS now follows Manager Plus request-average output tokens divided by total latency; first-token time is separate. Provider-native stored fields are unchanged.

Quota windows display observed used USD/tokens alongside estimated totals on aligned rows, following CPA Manager Plus quota extrapolation, rounding and forecast consistency rules. Forecast basis tooltips distinguish proportional forecasts from previous-cycle actuals. Recent requests have individual API-equivalent USD cost, calculated from the same prices as credential totals; missing prices remain unknown and fractional-cent requests retain readable precision. Narrow screens stack the five-hour and seven-day windows without separating used/total pairs. The recent request Time column displays both first-token and total latency in seconds; missing measurements remain independently unknown. Chinese/English labels switch together, and the original timestamp column is labeled Requested at.

The v0.1.6 replacement build moves refresh, language, and connection controls to the left-side toolbar below the tabs so CPA's top-right floating control does not cover the refresh button. Request refresh remains GET-only. Since the version number is unchanged, existing v0.1.6 installations may need a manual reinstall or `.so` replacement followed by a CPA restart; keep the data directory.

## Optional CPA Plugin Store source

Once this project is in your own GitHub account and a `v0.1.0` GitHub Release has completed,
append the registry URL to the existing plugin store sources (do not replace your other sources):

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/chilin11/cpa-mini-usage/main/registry.json"
```

The included `registry.json` and release workflow provide the exact platform ZIP asset
and SHA256 checksum format expected by CPA's plugin store. Alternatively, installing the
`.so` manually does not require a GitHub Release.

To publish from a locally authenticated GitHub CLI, `./publish-to-github.sh` creates a
public repository in your account and tags `v0.1.0`. The workflow then creates the release.
Do **not** run the script against an existing repository of the same name without reviewing it.
