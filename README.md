# Demo API (api.rhaqim.com)

The backend for the live demos on rhaqim.com. The **pages** live in the
portfolio repo (`src/pages/demo/*`, scripts in `src/scripts/demo/`, expected
as a sibling folder: `../portfolio`); this repo is only the **API** they call
from the browser, running on one Hetzner box behind Cloudflare.

| Page on rhaqim.com | API prefix | Service | Language |
|---|---|---|---|
| `/demo/ussd` | `/api/ussd/*` | `services/ussd`: ussdframework behind actix | Rust |
| `/demo/buckt` | `/api/buckt/*` | `services/go/cmd/buckt`: per-visitor buckt sandboxes | Go |
| `/demo/loom` | `/api/loom/*` | `services/go/cmd/loom`: multiplayer tabletop + agent pipeline | Go |
| `/demo/forecast` | `/api/loom/desk/*` | `services/go/cmd/loom`: the Forecast Desk | Go |
| every page | `/api/feedback` | `services/go/cmd/hub`: the feedback form | Go |

```
 browser on https://rhaqim.com/demo/*      (static Astro pages)
        │  fetch / EventSource, credentials: include
        ▼
 Cloudflare  (proxied DNS for api.rhaqim.com, TLS, DDoS)
        │  443, Cloudflare Origin Certificate
        ▼
┌──────────────────────────────────────────────────────────────┐
│ gateway  Caddy: CORS for rhaqim.com, one route per service    │
└───┬───────────┬───────────────┬───────────────┬──────────────┘
    │/api/ussd  │/api/buckt     │/api/loom      │/api/*
┌───▼───┐  ┌────▼────┐     ┌────▼────┐     ┌────▼────┐
│ ussd  │  │  buckt  │     │  loom   │     │   hub   │
└───────┘  └────┬────┘     └────┬────┘     └────┬────┘
 in-memory      └───────────────┼───────────────┘
 sessions               demo_data volume
                 demo.db (SQLite, WAL) + buckt-media/
                 tables: buckt_*, loom_*, demo_*
```

**Cross-origin, same-site.** rhaqim.com and api.rhaqim.com are different
origins but the same site, so the visitor cookie (`demo_vid`, HttpOnly,
Secure, SameSite=Lax, set by the API) travels with `credentials: "include"`
requests and with `<img>` loads of buckt files. Caddy answers CORS only for
origins matching `CORS_ORIGINS` (default `^https://(www\.)?rhaqim\.com$`).

**One SQLite file.** Hub, buckt and loom share `demo.db`: one host, WAL mode,
10-second busy timeout, and each service prefixes its tables (buckt via
`WithTablePrefix("buckt_")`, loom's `loom_`, `demo_` for the rest).

## The loom demo

**Multiplayer tabletop.** A host writes the world and up to four NPCs,
chooses an AI game master or runs the table themselves, and shares a
six-character code (or the invite link `rhaqim.com/demo/loom?game=CODE`).
Up to six players join from their own browsers; everyone watches over SSE.
Built on loom v0.0.3's shared-session features.

- *AI game master:* each round, a rules agent decides who needs a check, Go
  rolls the d20s, then one loom turn streams the GM's narration to every
  player while a referee, chronicler and suggester run in parallel.
- *Human game master:* the host narrates; their text is the recorded lead of
  the same loom turn (`TurnRequest.LeadResult`, authored by the host, no
  generator, no cost) and, with bookkeeping on, the same three AI followers
  keep the books. The host can call for checks, adjust sheets, and
  make NPCs speak (one agent, persona injected as a literal system prompt).
- *Players are loom participants* (`AddParticipant`, roles `player` / `dm`).
  Joins and DM adjustments are recorded steps (`RecordStep`); every change to
  the party is a step whose post-hook edits typed state in `State.Snapshot`,
  so the host can rewind to the end of any round with `Sessions().Fork`.
- *Live view:* each player's stream forwards loom's `Engine.Subscribe` events
  (`step.started`, `chunk`, `step.done`) for the game's session, and
  resubscribes when a rewind forks a new one. App events (actions, dice,
  costs) come from the game's feed table, which late joiners replay.
- *Effort:* the GM runs at `medium`, the bookkeeping agents at `low`
  (`output_config.effort` through the Anthropic adapter).
- *Cost:* each round's cost per agent and per turn comes from loom's ledger,
  priced for the host's model; the game total and a per-round chart sit beside
  the story. Before creating a game, "Check key & price" runs loom's
  `VerifyGenerator` (the provider's models endpoint, free) and prices an
  estimate with `Cost().Estimate`.
- *The host's key* is kept in process memory for that game only (6-hour
  sliding expiry, dropped when the game ends), never in SQLite or logs. Every AI
  step at the table uses it. After a server restart the host re-enters it.
- *Limits:* 400 agent steps per game per day (a loom budget on
  `PlatformID = game:<code>`), 6 players, 10 games created per IP per hour,
  40 wrong codes per IP per hour.

**Agent pipeline.** A visitor-editable incident-postmortem graph; loom
validates the wiring and plans it into parallel layers.

**Forecast Desk.** A fictional eight-team league with play chips (`desk.go`,
`desk_sim.go`). Matches are decided in Go from a seed, with odds from a Poisson
model plus a 6% margin. One loom turn per match: the odds-maker streams a
preview, the value hunter and contrarian pick in parallel, and the bankroll
manager (planned into the next layer because it reads both picks) sizes their
stakes. A post-hook, the risk referee, rejects an over-limit stake once with
`ErrRetryWith` (the reason goes into the retry's prompt) and trims it on the
retry. Settlement is a recorded step, so "What if?" forks before any matchday
and replays it: same seed, same score, different bets. The backtest runs the
strategies as plain Go over up to 500 seeded matches, no model calls.

The *Client asks for loom* doc records what the demo needed from loom; v0.0.3
delivered all of it, and the demo's workarounds are gone. loom's GC runs with
retention rules: pipeline sessions are purged after 48 idle hours, tabletop
sessions after 8 days, Forecast Desk sessions after 7.

## Layout

```
gateway/            Caddyfile (production, CORS), Caddyfile.local, Dockerfile
services/go/        one Go module; internal/platform is shared plumbing
  cmd/hub           feedback API
  cmd/buckt         buckt sandbox API
  cmd/loom          engine, tabletop (game.go, rpg.go), Forecast Desk (desk.go, desk_sim.go),
                    pipeline, dry-run generator, key check
services/ussd/      Rust/actix front for ussdframework; menu.json is QuickLoan
docker-compose.yml  production stack (pulls images CI built)
docker-compose.local.yml  local overrides: API on http://localhost:8080
.github/workflows/deploy.yml  build → push to GHCR → deploy over SSH → smoke test
```

## Commands

`make` lists everything. The ones you'll use most:

| Command | What it does |
| --- | --- |
| `make up` / `make down` | Start or stop the local API on http://localhost:8080 |
| `make site` | Run the portfolio dev server against the local API |
| `make logs s=loom` | Follow one service's logs |
| `make check` | Formatting, `go vet`, Go and Rust tests, Caddyfile and compose validation |
| `make run-loom` (etc.) | Run one service natively, no Docker |
| `make bump-loom v=v0.0.4` | Move to a new loom release |
| `make server-up TAG=<sha>` | On the box: pull and start a given build |
| `make backup` | On the box: snapshot `demo.db` into `./backups/` |
| `make feedback` | Read the latest feedback (needs `ADMIN_TOKEN`) |

## Run it locally

```bash
make up      # the API on http://localhost:8080
make site    # the portfolio from ../portfolio, pointed at the local API
open http://localhost:4321/demo
```

`make site` takes `PORTFOLIO_DIR=...` if the portfolio is somewhere else.

The local Caddyfile allows any `http://localhost:*` origin. Without a key,
loom runs as a dry run: the orchestration, hooks, forks, budgets and ledger
are real; only the model text is canned. Open two browser profiles to play a
multiplayer game with yourself.

## Server setup (once)

**Quick path.** On the box:

```bash
git clone https://github.com/Rhaqim/portfolio-demo.git /opt/demo
cd /opt/demo && ./scripts/server-setup.sh --deploy-user deploy --deploy-key "ssh-ed25519 AAAA..."
```

The script is idempotent, so you can re-run it any time. It installs Docker,
writes `.env` with a generated `ADMIN_TOKEN`, asks you to paste the origin
certificate (step 3), restricts 80/443 to Cloudflare (step 4, both ufw and
Docker's `DOCKER-USER` chain), pulls the images (or builds them if the pull
fails), starts the stack, checks every service, and prints the GitHub secrets
for step 6. You still do DNS (step 2) and GitHub (step 6) by hand. The manual
steps below describe what the script does.

1. **Docker.** Install Docker Engine and the compose plugin. Create a deploy
   user in the `docker` group, and `sudo mkdir -p /opt/demo/certs && sudo chown deploy /opt/demo`.
2. **DNS.** In Cloudflare, add `api` → the server IP, **proxied** (orange
   cloud). SSL/TLS mode **Full (strict)**. Cloudflare keeps SSE open as long as
   bytes flow; the game stream sends a keep-alive every 20 s.
3. **Origin certificate.** Cloudflare → SSL/TLS → Origin Server → Create
   certificate for `api.rhaqim.com`. Save it as `/opt/demo/certs/origin.pem`
   and the key as `/opt/demo/certs/origin-key.pem` (`chmod 600`).
4. **Firewall.** Allow SSH, and 80/443 **only from Cloudflare**. The services
   rate-limit on `CF-Connecting-IP`, which anyone who reaches the box directly
   could forge. Hetzner's Cloud Firewall with Cloudflare's ranges
   (https://www.cloudflare.com/ips/) is simplest; ufw alone isn't enough,
   because Docker publishes ports through iptables and bypasses it.
5. **Environment.** Copy `.env.example` to `/opt/demo/.env`; set `ADMIN_TOKEN`
   (`openssl rand -hex 32`). If the portfolio is served from another origin
   (a Pages preview domain), widen `CORS_ORIGINS`.
6. **GitHub.** Add the secrets and variables listed at the top of
   `.github/workflows/deploy.yml` and an environment called `production`. Hetzner
   CAX (Arm) boxes need the variable `PLATFORM=linux/arm64`. Push to `main`.
7. **Links.** The portfolio links into the demos while `demo.live` is `true`
   in its `src/data/site.ts` (the default). Set it to `false` to hide every link
   if the API will be down for a while.

## Operating it

**Read feedback.**
```bash
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" https://api.rhaqim.com/api/feedback | jq
```
Or set `FEEDBACK_WEBHOOK_URL` (Slack or Discord) to get each one as a message.

**Logs.** `docker compose logs -f loom`. Caddy logs every request with the
`X-Llm-Key`, `Cookie` and `Authorization` headers removed.

**Back up the database** with SQLite's backup (copying a WAL database mid-write is unsafe):
```bash
docker run --rm -v demo_demo_data:/data -v "$PWD":/out alpine \
  sh -c 'apk add -q sqlite && sqlite3 /data/demo.db ".backup /out/demo-$(date +%F).db"'
```

**Roll back.** `TAG=<older sha> docker compose up -d` in `/opt/demo`.

**Restarts** end nothing: games, feeds and sheets are in SQLite. Hosts with a
model re-enter their key (the page prompts them); live streams reconnect and
replay what they missed from the feed.

## Adding a demo

1. A service: `services/go/cmd/<name>` (shared DB, visitor cookie, rate limiter
   from `internal/platform`) or its own directory with a Dockerfile.
2. Prefix its tables `<name>_` if it uses `demo.db`.
3. Add it to `docker-compose.yml`, the workflow's build matrix, and a
   `handle /api/<name>/*` block in both Caddyfiles.
4. In the portfolio: `src/pages/demo/<name>.astro` + `src/scripts/demo/<name>.js`
   (use `api`/`stream`/`watch` from `common.js`), a card on `/demo`, the
   project's `demo: "/<name>"` in `src/data/projects.ts`, and the name in
   `demos` in `cmd/hub/main.go` for feedback.

## Safety notes

- **Visitor isolation.** buckt's lookups by ID aren't owner-scoped, so every
  handler checks ownership. loom pipeline agents live under
  `owner = visitor:<id>`. Game actions check host or player membership; codes
  are 6 characters from a 31-letter alphabet (~887 million) behind a
  wrong-code limiter.
- **LLM keys.** Sent in `X-LLM-Key`, passed to generators through
  `StepRequest.Overrides`, which loom never persists. Per-request `base_url`
  overrides stay disabled, so a key can't be redirected. Provider errors are
  scrubbed of anything key-shaped. Hosts' keys live in memory only.
- **Abuse limits.** Per-IP rate limits everywhere, one pipeline run per
  visitor, eight concurrent runs, per-visitor and per-game step budgets,
  5 MB uploads, 40 files / 25 MB per sandbox, file TTLs, idle-sandbox and
  idle-game janitors.
- **Uploads** pass buckt's scanner (sniffed images, PDFs, text only) and are
  served with `nosniff`, a sandboxing CSP and `Content-Disposition`.
- **USSD menus** are checked before they run (size caps, a cycle check on
  non-interactive screens, `function_url` stripped).

## Upstream versions

- **ussdframework** pinned at `a12ae3c`: logs through the `log` facade (this
  service installs no logger, so nothing about a session is printed) and shows
  a menu error once.
- **loom** `v0.0.3`. Its Anthropic adapter is now a separate module,
  `github.com/rhaqim/loom/generator/anthropic`, which has no tag yet, so
  `go.mod` pins it to a pseudo-version. Once `generator/anthropic/v0.0.3` is
  tagged, run `go get github.com/rhaqim/loom/generator/anthropic@v0.0.3`.
