# Proxy risk scoring (for reference only)

Codex2API has a built-in scoring adapter for the Scamalytics IP Fraud Risk API v3. It can submit egress IPs from the proxy pool directly to Scamalytics for batch observation. The separate 8788 wrapper service is not a runtime dependency. Scoring results are used for operational screening, auditing, and troubleshooting; they do not automatically disable proxies and do not change account bindings, proxy pool routing, or request forwarding.

## Supported service protocol

The built-in engine uses the following official v3 requests:

```text
GET https://{SCAM_HOST}/v3/{SCAM_USER}/?key={SCAM_KEY}&ip={ipv4}
Header: Accept: application/json
Header: User-Agent: Codex2API-Scamalytics/1.0
```

The `score`, `risk`, proxy/VPN/TOR/datacenter/blocklist, ISP, country/region, and credits fields returned by the service are parsed into a redacted scoring snapshot. Fields that are not returned stay "unknown" and are never faked as 0.

## Setup steps

1. Open "Proxy pool" in the admin panel.
2. Click "Configure risk scoring".
3. Create a new built-in scoring profile, and fill in the Scamalytics Host, User, and API Key.
4. Set the timeout, concurrency, request interval, cache, and task and daily count limits.
5. Confirm "Risk scores are for reference only", then enable the profile.
6. Click "Test built-in engine". After it succeeds, you can score the current page or all proxies.

"Test built-in engine" makes one real v3 query using `8.8.8.8`, so it consumes one unit of Scamalytics quota.

The Scamalytics User and API Key are stored only on the Codex2API server; the admin API returns only "configured" and a masked value. Codex2API submits only the IP parsed from the proxy URL and never sends the username or password in the proxy URL to Scamalytics.

## Quota and counts

- `Max checks per day` is a local Codex2API task budget; `0` means unlimited.
- `Credit reserve threshold` is used for stop protection when Scamalytics returns credits.
- Within the `Cache TTL`, repeated scoring uses the snapshot by default and does not consume external requests.
- `Allow force refresh` can bypass the cache only when the profile explicitly allows it.
- Credits returned by Scamalytics are server-observed values; if the service returns no quota field, the page shows "unknown".

## IP and DNS safety

By default, only the literal IPv4 address in the proxy URL is scored. Domain proxies must explicitly enable "Allow restricted DNS resolution", and the resolved result must be a public IPv4 address.

Denied by default:

- localhost, loopback, private network, and link-local addresses;
- multicast, broadcast, reserved test ranges, and unspecified addresses;
- domains with no public IPv4 result.

Allowing private-network targets is a high-risk operational option, suitable only for internal scoring services that the admin explicitly controls. Even when enabled, it does not affect proxy scheduling.

## Reading the results

The proxy list shows two independent statuses at once:

```text
Connectivity: success · 182ms
Risk score: 72 · High Risk
Flags: VPN / Datacenter / Blacklist
ISP / attribution: Example ISP · US
Suggestion: Replace
```

A risk score is not an availability probe, and it is not a basis for automatic bans. Make a manual judgment using upstream responses, connection latency, account bindings, and real business requests.

## Official references and sign-up

The default profile presets only the official homepages that can be confirmed:

- [Scamalytics official homepage](https://www.scamalytics.com/)
- [Scamalytics IP risk lookup](https://www.scamalytics.com/ip)

API hosts, users, keys, quotas, and authorization scopes may differ between plans. Follow the official account page or the provider's API documentation, and enter the actual sign-up entry in the "Official docs URL" and "Sign-up / setup guide URL" fields of the scoring profile.

## Troubleshooting

- Scamalytics timeout, 401, 429, or 5xx: the proxy is marked "Scoring error"; the proxy keeps its existing enabled state.
- DNS resolution failure: the proxy is marked "Cannot score"; it is not deleted or unbound.
- Daily count or quota protection triggered: the task stops safely, and completed results are kept.
- A scoring task can be canceled; canceling does not roll back scoring snapshots that were already saved.

Scoring tasks use a background queue and paged refresh, so an unavailable external service does not block the proxy pool page.
