# CPA Quota API

A standalone plugin for reading Claude and Codex quotas in **CLIProxyAPI v8.0.58**.
No CPA patch is required. The plugin does not select accounts, does not block
inference requests, and does not modify auth files. Default CPA routing stays unchanged.

The quota fetching, authorization, and caching code was extracted from
`cpa-plugin-five-hour-quota-router`. The original plugin is unchanged.

## Build

Requires Go 1.26.10 and a C compiler for cgo.

```sh
make build
```

On macOS the build output is `dist/quota-api.dylib`. The library name must
match the plugin ID: `quota-api.dylib` on macOS, `quota-api.so` on Linux,
`quota-api.dll` on Windows. For other platforms, use the matching output
file name with `go build -buildmode=c-shared -o <path> .`.

## Setup

Put the built library into the CPA plugins directory and add the configuration:

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/plugins
  configs:
    quota-api:
      enabled: true
      quota-api-keys:
        - replace-with-a-long-random-key
      cache-ttl: 60s
      request-timeout: 10s
      # user-agent: claude-code/2.1.80
```

`quota-api-keys` are dedicated quota access keys, not the CPA management key and
not provider tokens. Without keys the endpoint returns `404`; without a valid
Bearer key it returns `401`. Use HTTPS when accessing it from outside localhost.

If you no longer need the old router, disable it separately:

```yaml
plugins:
  configs:
    five-hour-quota-router:
      enabled: false
```

Merge this snippet into your existing `plugins` configuration instead of
adding a second YAML section with the same name. The new plugin does not
disable the old one by itself.

## API

```sh
curl -H "Authorization: Bearer $QUOTA_API_KEY" \
  http://127.0.0.1:8317/v0/resource/plugins/quota-api/quota
```

The response contains two arrays with the same entry structure:

```json
{
  "claude": [
    {
      "auth_index": "claude-account",
      "email": "user@example.com",
      "usage": {
        "five_hour": { "utilization": 35, "resets_at": "2026-11-07T10:00:00Z" }
      },
      "fetched_at": "2026-11-07T08:00:00Z"
    }
  ],
  "codex": []
}
```

`usage` holds the provider's quota object JSON without normalizing its fields.
It keeps the available limit windows and extra data, such as Claude
`extra_usage`. The `email` and `label` fields are optional. If a refresh fails,
the entry contains `error` and, while a backoff is active, `retry_after_ms`. The last
successful `usage` is kept together with its original `fetched_at`.

`retry_after_ms` is the time until the next attempt to **fetch quotas**,
not the time until the model request quota resets.

### Differences from the old router API

- The path changed from `/v0/resource/plugins/five-hour-quota-router/quota`
  to `/v0/resource/plugins/quota-api/quota`.
- `claude` is now an array of accounts, like `codex`. Provider data lives
  in `usage`; the former `cutoff_percent_used`, `protected_models`, and `blocked` are gone.
- Quota enforcement and inference error formatting are not part of this plugin.

## Refresh and errors

Quotas are fetched only on an authorized `GET /quota`. There is no background polling
and no provider calls at startup. Fresh entries are served from cache
(60 seconds by default). Concurrent refreshes of the same account are coalesced;
at most four quota requests run in parallel.

After a failed refresh a backoff applies: 5 minutes, doubling up to 30 minutes,
or the time from the provider's `Retry-After`/reset header, capped at 6 hours.
For `429` the backoff is at least 5 minutes. Up to 10% random jitter is added.
A success resets the error counter. Replacing credentials resets their cache.

The plugin reads only active physical Claude/Codex accounts through the host API.
If an OAuth access token cannot be obtained for an account, it is reported
as an entry error. A failure to list accounts returns `502`,
not an empty successful response. Tokens and auth JSON are never exposed by the API.

Undocumented provider endpoints are used:
`api.anthropic.com/api/oauth/usage` and `chatgpt.com/backend-api/wham/usage`.
Their format and limits may change. The default Claude User-Agent is
`claude-code/2.1.80`; override it with `user-agent`.

## Tests

```sh
go test ./...
go test -race ./...
```

The default suite includes a check that the library actually loads in CPA v8.0.58.
The process test is excluded from the `-race` run; unit tests are run with it.
Tests use temporary directories, synthetic accounts, and local provider
responses. No real keys or user configuration are needed.

Building and testing do not install the plugin or modify a running CPA.

Repository: [kurbezz/cpa-plugin-quota-api](https://github.com/kurbezz/cpa-plugin-quota-api).

## License

MIT. Attribution to the original `cpa-plugin-quota-router` and the derived
`cpa-plugin-five-hour-quota-router` is preserved; see LICENSE.
