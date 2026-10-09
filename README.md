# CPA Mini Usage

A lightweight native CPA v8 plugin (Linux amd64) with **exactly two tabs**:

1. **Quotas:** separate Claude / Codex sections showing five-hour / seven-day remaining percentages, resets, and estimated remaining tokens/cost.
2. **Requests:** recent requests with explicit model names, per-credential/model usage, and estimated API-equivalent USD cost. Tokens use `k / m / b`; hover for full counts.

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

Data is stored under `data/cpa-mini-usage/` (create/mount as persistent in Docker). If the directory is not writable, the UI will show a storage warning and in-memory data may be lost on restart. Collection starts at installation time; prior request history is **not backfilled**.

### Limits of estimates

- Percentages and reset times are read from the provider's **OAuth quota endpoint** and are not calculated from token counts.
- Actual tokens are captured from requests handled by **this CPA instance**, not requests made on other machines/clients directly to the provider.
- Estimated maximum tokens / dollars = **CPA-observed usage within the active window / provider reported utilized fraction**. This is an extrapolation, **not an official quota cap**. Values will be inaccurate if collection began mid-window, quota was spent outside this CPA, traffic mixes different token/model types, or the upstream uses non-token factors.
- Prices are automatically refreshed from `https://models.dev/api.json` every 6 hours. Cache read token cost is separated from uncached input; reasoning tokens are **not** double-charged when already included in output tokens. Unknown model prices are shown as unknown (not $0).
- Historical costs are revalued using the latest known prices, and represent **API-equivalent cost**, not an actual Claude Pro/Max or ChatGPT subscription charge.
- Quota is automatically checked every 3 minutes, with at most four account reads in parallel. The visible dashboard syncs snapshots every minute and updates countdown text every 30 seconds. Unchanged content keeps its DOM nodes; quota and request sections update independently, without entry animations. Manual refresh polls completion every 2 seconds. Only read-only GET requests to known quota endpoints are made.
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
