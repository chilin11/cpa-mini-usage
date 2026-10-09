# CPA Mini Usage

A lightweight native CPA v8 plugin (Linux amd64) with **exactly two tabs**:

1. **Quotas:** Claude + Codex five-hour / seven-day utilization, resets, observed tokens/cost, and percentage-extrapolated full-window totals.
2. **Credentials:** per-credential requests, input/output/cache tokens, model breakdown and estimated API-equivalent USD cost.

No React, no Node runtime, no separate web service, no inference probes, no database process.

## Install

Use the bundled Linux amd64 binary (`dist/cpa-mini-usage.so`) or build from source:

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

Use the same **management key** as the CPA management UI, if the plugin page cannot find it. The key is held only in page memory and is never sent anywhere other than same-origin CPA Management API.

### Data and persistence

Data is stored under `data/cpa-mini-usage/` (create/mount as persistent in Docker). If the directory is not writable, the UI will show a storage warning and in-memory data may be lost on restart. Collection starts at installation time; prior request history is **not backfilled**.

### Limits of estimates

- Percentages and reset times are read from the provider's **OAuth quota endpoint** and are not calculated from token counts.
- Actual tokens are captured from requests handled by **this CPA instance**, not requests made on other machines/clients directly to the provider.
- Estimated maximum tokens / dollars = **CPA-observed usage within the active window / provider reported utilized fraction**. This is an extrapolation, **not an official quota cap**. Values will be inaccurate if collection began mid-window, quota was spent outside this CPA, traffic mixes different token/model types, or the upstream uses non-token factors.
- Prices are automatically refreshed from `https://models.dev/api.json` every 6 hours. Cache read token cost is separated from uncached input; reasoning tokens are **not** double-charged when already included in output tokens. Unknown model prices are shown as unknown (not $0).
- Historical costs are revalued using the latest known prices, and represent **API-equivalent cost**, not an actual Claude Pro/Max or ChatGPT subscription charge.
- Quota is polled no more frequently than every 5 minutes by default. The plugin performs **read-only GET** requests to the known upstream quota endpoints using CPA OAuth credentials; no inference calls are made.
- Supports Claude OAuth and Codex OAuth; API keys do not expose subscription quota windows.
- If `usage.handle` is not dispatched by your CPA build, the quota still works but per-request stats cannot be collected. Upgrade CPA to a build with the usage callback fix.

### Security

Resource HTML is public by design; actual usage/quota data comes only from **management-key-protected** plugin routes. Upstream credential tokens and CPA management keys are not written to disk, emitted in API responses, or included in logs. OAuth lookups are restricted to known HTTPS hosts.

### Limitations

This first release is tested locally with a fake CPA host callback harness, not against a live CPA with your accounts. Authentication file formats and upstream quota schemas may change. If it shows `No quota data`, inspect CPA logs for host callback errors.

### v0.1.1 registration fix (CPA 8.0.22)

Version 0.1.0 used an incorrect `github_repository` registration field. CPA requires
`GitHubRepository` and rejects the plugin when that metadata field is missing.
Refresh the plugin store source, upgrade to v0.1.1, enable the plugin, then restart CPA.
CI now tests registration, dashboard access, and management API authentication using
the real CPA 8.0.22 Linux amd64 release before publishing a new version.

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
