# DevPilot APNs relay

The **cloud push relay** of the DevPilot project, in [`devpilot-app/apns`](https://github.com/devpilot-app/apns): a single Go binary deployed on a VPS, sitting between the hook daemon and Apple APNs — the daemon connects to it, the iPhone connects to it, and it pushes to APNs on your behalf. Without it, "agent waiting for approval" events die on the dev machine, and a killed app receives nothing.

> **License:** source-available under [FSL-1.1-ALv2](LICENSE.md) — free to self-host, audit and modify; running it as a competing hosted service needs a commercial license. Details under [License](#license).

## How the three projects fit together

Two Go repos and one iOS app, one chain:

| | DevPilot (iOS app) | [devpilot-app/hook](https://github.com/devpilot-app/hook) | **[devpilot-app/apns](https://github.com/devpilot-app/apns) (this repo)** |
|---|---|---|---|
| What it is | iOS app (Swift/SwiftUI) | Go daemon on the dev machine | **Go relay on a cloud VPS** |
| Responsibility | Terminal/SSH, inbox UI, approval panel, Live Activity, APNs token registration | Hook adapters (Claude Code / Codex), local gateway, event aggregation & rate limiting, offline buffering | **WSS ingest, pairing routing table, APNs push, Live Activity remote-update forwarding** |
| Role in the chain | WSS client (app side) | WSS client (daemon side) | **Hub** |
| Code boundary | App-side integration (RelayChannel model, etc.) | The daemon; `pkg/wire` imported from `github.com/devpilot-app/apns` via go.mod | The whole relay + the **`pkg/wire` protocol type library (single source of truth for the wire protocol across all three ends)** |

```
Claude Code/Codex hooks
  → hook daemon, devpilot-app/hook (local gateway 127.0.0.1:24543)
  → APNs relay, devpilot-app/apns (this repo: auth → routing table → channel selection)
  → Apple APNs (HTTP/2) ── or ── direct push over the app's WSS connection
  → iPhone (notification / Live Activity / inbox)
Approval flows back: iPhone Allow/Deny → relay → daemon → hook unblocks, agent continues
```

Design rules: a timeout anywhere in the approval chain falls back to the terminal's native prompt (worst case you are asked twice; it never approves on its own); the relay **routes but never reads content** — the cloud channel only carries summary fields you could already see in the notification; benchmarked against moshi's cloud relay.

## Quick start (deploy)

```bash
git clone https://github.com/devpilot-app/apns.git
cd apns/deploy
cp .env.example .env && chmod 600 .env   # fill in DOMAIN / APNS_KEY_ID / APNS_TEAM_ID / METRICS_TOKEN
mkdir -p secrets && chmod 700 secrets
cp ~/Downloads/AuthKey_XXXX.p8 secrets/apns.p8 && chmod 600 secrets/apns.p8
docker compose up -d --build
curl -fsS https://relay.example.com/readyz
```

Caddy terminates ACME TLS and reverse-proxies WSS (`flush_interval -1`); the relay runs as distroless + nonroot with a read-only root filesystem. **Behind a reverse proxy, set `TRUSTED_PROXY_COUNT=1`** (already present in .env.example) so the relay can recover the real client IP from XFF for rate limiting and banning. The full EC2 runbook, backups, monitoring and the `.p8` checklist live in [deploy/README.md](deploy/README.md).

Once a Mac is paired, `test-push` verifies on a real device (the default relay address comes from `PUBLIC_BASE_URL`):

```bash
devpilot-relay test-push --relay https://relay.example.com \
  --credential dpd_... --kind approval   # iPhone receives a time-sensitive notification
```

## Quotas & abuse protection (multi-tenant hosting)

Defaults target "one relay serving many users"; a self-hosted single-user setup can turn everything off (threshold 0).

| Dimension | Default | Env vars | Meaning |
|---|---|---|---|
| Event rate | free 10/60s, pro 60/60s | `RATE_EVENT_FREE/PRO`, `RATE_EVENT_WINDOW_SECONDS` | per-daemon token bucket; over-limit returns `AckRejected` (daemon drops terminally, no retry storm) |
| Daily push quota | free 10/day, pro 3000/day | `QUOTA_DAILY_FREE/PRO` | per-daemon UTC calendar day; only gates APNs pushes; WSS realtime / inbox / resync are unaffected |
| Approval exemption | on | `QUOTA_EXEMPT_APPROVALS` | `approval_required/task_complete` bypass the rate gate but **not** the daily quota |
| Pairing rate limit | 15/10 per minute per IP | `PAIR_SESSION_LIMIT/PAIR_ATTEMPT_LIMIT` | fixed window |
| IP ban | 100 4xx·5xx / 5min → ban 24h | `BAN_THRESHOLD/WINDOW_SECONDS/TTL_SECONDS`, `BAN_ENABLED` | fail2ban style; whole IPv4 address, IPv6 /64 aggregation; loopback exempt |
| Identity TTL | 0 (disabled) | `DAEMON_TTL_DAYS/DEVICE_TTL_DAYS` | daemons that have not connected within TTL are cascade-unregistered; kept at 0 for launch, enable once stable |
| /metrics | public | `METRICS_TOKEN` | Bearer required when non-empty; Caddy adds a 404 fallback on top |

The tier (`free|pro`) is submitted by the app at pairing time; it is client-asserted today, with receipt-level verification planned for a later round.

## Development & testing

```bash
go test -race ./...            # all unit and integration tests
go build ./cmd/relay           # local build
make docker                    # build the image (needs a Docker daemon)
```

The APNs client can run the delivery path without a `.p8` via `APNS_MOCK=1`; unit tests use a fake HTTP/2 server covering 200/410/429/403/400/5xx, JWT caching and topic backoff.

## Release

`git tag vX.Y.Z && git push origin vX.Y.Z` triggers:

- `.github/workflows/docker.yml`: builds and pushes `ghcr.io/devpilot-app/devpilot-relay` (tags `X.Y.Z`, `X.Y`, `latest`);
- the hook repo (`devpilot-app/hook`) depends on this repo's tag via `github.com/devpilot-app/apns`,
  so **this repo must be released before the hook**.

Server upgrades: `docker compose pull && docker compose up -d`.

## License

Source-available under the [Functional Source License 1.1, ALv2 Future License](LICENSE.md) (FSL-1.1-ALv2). This is not an OSI-approved open source license: the code stays publicly readable and auditable, but there is one restriction.

**Permitted** — any purpose other than a Competing Use, which includes:

- reading, auditing and modifying the code;
- self-hosting the relay for yourself or your team, exactly as the quick start above shows;
- internal use in your own products and services;
- non-commercial education and research.

**Not permitted** — a Competing Use: offering this relay, or anything with substantially similar functionality, to others as a commercial product or hosted service. Running the relay as a business for other people's daemons needs a commercial license.

Every release becomes available under the [Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0) on the second anniversary of its release. For a commercial license covering a Competing Use, contact [support@devpilot.app](mailto:support@devpilot.app).
