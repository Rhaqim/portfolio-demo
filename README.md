# Demo API (api.rhaqim.com, api.thebinaryhouse.studio)

The backend for the live demos on rhaqim.com, and for The Binary House's
LaundryFlow demo on thebinaryhouse.studio, which has an API host of its own. The **pages** live in the
portfolio repo (`src/pages/demo/*`, scripts in `src/scripts/demo/`, expected
as a sibling folder: `../portfolio`); this repo is only the **API** they call
from the browser, running on one Hetzner box behind Cloudflare.

| Page on rhaqim.com | API prefix | Service | Language |
|---|---|---|---|
| `/demo/ussd` | `/api/ussd/*` | `services/ussd`: ussdframework behind actix | Rust |
| `/demo/buckt` | `/api/buckt/*` | `services/go/cmd/buckt`: per-visitor buckt sandboxes | Go |
| (Noewright Studio site) | `/api/studio/*` | `services/go/cmd/studio`: Studio's API | Go |
| `/demo/noewright` | `/api/loom/*` | `services/go/cmd/loom`: multiplayer tabletop + agent pipeline | Go |
| `/demo/forecast` | `/api/loom/desk/*` | `services/go/cmd/loom`: the Forecast Desk | Go |
| `/demo/captions` | `/api/loom/captions/*` | `services/go/cmd/loom`: sermon captions | Go |
| thebinaryhouse.studio`/laundryflow` | api.thebinaryhouse.studio `/api/laundryflow/*` | `laundryflow`: per-visitor sandboxes, built from the private [laundryflow](https://github.com/Rhaqim/laundryflow) repo | Go |
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

**Two hosts, kept apart.** The Binary House is a separate entity, so its demo
has its own host, `api.thebinaryhouse.studio` (`TBH_DOMAIN`), on the same box
and gateway. It has its own Cloudflare zone and origin certificate, allows
only `TBH_CORS_ORIGINS` (default thebinaryhouse.studio, with or without
`www`), and routes nothing but `/api/laundryflow/*`. api.rhaqim.com does not
route LaundryFlow at all. The page sends its sandbox token as `Authorization:
Bearer …` rather than relying on a cookie; Caddy strips that header from its
logs. Sandboxes live in
the service's memory (30 idle minutes, at most 400 at once, 15 new ones per IP
per 10 minutes) and nothing is written to `demo.db`. With
`LAUNDRYFLOW_INTERPRET=on` and `LAUNDRYFLOW_ANTHROPIC_API_KEY` set in `.env`,
messages the rules cannot read are interpreted by Claude Haiku: at most
`LAUNDRYFLOW_INTERPRET_PER_HOUR` calls an hour across all sandboxes (default
600) and 40 per sandbox a day. The key is the demo's own; give it a spending
limit in the Claude Console.

**One SQLite file.** Hub, buckt and loom share `demo.db`: one host, WAL mode,
10-second busy timeout, and each service prefixes its tables (buckt via
`WithTablePrefix("buckt_")`, Noewright's default `loom_`, `demo_` for the rest).

## Noewright Studio (API)

`services/go/cmd/studio` with the logic in `internal/studio` serves the API for
Noewright Studio under `/api/studio/*`. The frontend is a separate Astro and
Svelte site (the `noewright-studio` repo, next to this one).

- *Visitors* are anonymous, identified by the demo visitor cookie. Each one's
  records live in their own Noewright owner scope, `studio:<visitor>`, and each
  starts with three starter graphs loaded from embedded bundles
  (`internal/studio/templates`): a support desk (routing and a wait loop), an
  incident postmortem (fan-out and joins) and an onboarding flow (waits for
  text and a choice). Each template carries bundle tests, run in CI.
- *Keys:* Studio has no key of its own. Without the visitor's key
  (`X-LLM-Key` + `X-LLM-Model`, per request, never stored or logged) runs use
  a simulator: the graph, routing, waits and costs are real, the text is not.
- *Runs* stream as server-sent events: `session`, `node` (per node, with
  tokens and cost), `chunk`, `done` (where the run waits, what ran, the cost).
  A conversation is a Noewright session, resumed by id.
- *Tests and evidence:* a graph's tests are its starter template's (read-only)
  plus the visitor's own, usually saved from a conversation. Running them uses
  the visitor's model or the simulator; each test's latest result (evidence,
  output texts, outputs that changed since the last run) is kept in
  `studio_results`. Test runs are capped at 20 an hour per visitor, since their
  sessions sit outside the visitor's step budget.
- *Download* exports a graph and everything it uses as a bundle, with its
  tests and their latest evidence, its generators named `chat` for the
  importer to map.
- *Limits:* 300 agent steps per visitor per day (a Noewright budget), one run
  at a time, 120 requests per minute.
- *Data:* its own SQLite file, `studio.db`, on the shared volume.

## The Noewright demo

The `loom` service (`cmd/loom`, `/api/loom/*`) runs on
[Noewright](https://github.com/rhaqim/noewright), formerly loom. The service
and its routes kept the old name; its tables keep Noewright's default `loom_` prefix.

**Multiplayer tabletop.** A host writes the world and up to four NPCs,
chooses an AI game master or runs the table themselves, and shares a
six-character code (or the invite link `rhaqim.com/demo/noewright?game=CODE`; old `/demo/loom?game=` links still forward).
Up to six players join from their own browsers; everyone watches over SSE.
Built on the shared-session features that arrived in loom v0.0.3.

- *AI game master:* each round is one Noewright graph (`RunGraph`, see
  `graphs.go`): a rules agent decides who needs a check, a Go function node
  rolls the d20s against the sheets in the session snapshot and posts each roll
  to the feed, then the GM streams its narration to every player while a
  referee, chronicler and suggester run in parallel. The opening round starts
  the graph at the GM, since there are no actions to check.
- *Human game master:* the host narrates; their text is the recorded lead of
  the same Noewright turn (`TurnRequest.LeadResult`, authored by the host, no
  generator, no cost) and, with bookkeeping on, the same three AI followers
  keep the books. The host can call for checks, adjust sheets, and
  make NPCs speak (one agent, persona injected as a literal system prompt).
- *Players are Noewright participants* (`AddParticipant`, roles `player` / `dm`).
  Joins and DM adjustments are recorded steps (`RecordStep`); every change to
  the party is a step whose post-hook edits typed state in `State.Snapshot`,
  so the host can rewind to the end of any round with `Sessions().Fork`.
- *Live view:* each player's stream forwards Noewright's `Engine.Subscribe` events
  (`step.started`, `chunk`, `step.done`) for the game's session, and
  resubscribes when a rewind forks a new one. App events (actions, dice,
  costs) come from the game's feed table, which late joiners replay.
- *Effort:* the GM runs at `medium`, the bookkeeping agents at `low`
  (`output_config.effort` through the Anthropic adapter).
- *Cost:* each round's cost per agent and per turn comes from Noewright's ledger,
  priced for the host's model; the game total and a per-round chart sit beside
  the story. Before creating a game, "Check key & price" runs Noewright's
  `VerifyGenerator` (the provider's models endpoint, free) and prices an
  estimate with `Cost().Estimate`.
- *The host's key* is kept in process memory for that game only (6-hour
  sliding expiry, dropped when the game ends), never in SQLite or logs. Every AI
  step at the table uses it. After a server restart the host re-enters it.
- *Limits:* 400 agent steps per game per day (a Noewright budget on
  `PlatformID = game:<code>`), 6 players, 10 games created per IP per hour,
  40 wrong codes per IP per hour.

**Agent pipeline.** A visitor-editable incident-postmortem graph; Noewright
validates the wiring and plans it into parallel layers.

**Forecast Desk.** A fictional eight-team league with play chips (`desk.go`,
`desk_sim.go`). Matches are decided in Go from a seed, with odds from a Poisson
model plus a 6% margin. One Noewright graph per match (`graphs.go`): the
odds-maker streams a preview, the value hunter and contrarian pick in parallel,
and the bankroll manager, joining both picks, sizes their stakes. A post-hook,
the risk referee, rejects an over-limit stake once with `ErrRetryWith` (the
reason goes into the retry's prompt) and trims it on the retry. A Go function
node then plays the match from the seed and settles the bets; it is recorded as
an `fd-settle` step, so "What if?" forks before any matchday and replays it:
same seed, same score, different bets. The backtest runs the
strategies as plain Go over up to 500 seeded matches, no model calls.

**Sermon captions.** Live captions of a sermon in Yorùbá, Igbo and Nigerian
Pidgin (`captions.go`, `bible.go`). Per spoken line, `sc-detect` finds Bible
references (book code, chapter, verse, and whether the speaker read the passage
aloud, named it or paraphrased it); a Go function node drops any reference that
doesn't exist and looks the rest up in three bundled Bibles (`cmd/loom/bible/`,
see its NOTICE: KJV, and Biblica's Yorùbá and Igbo translations under CC BY-SA
4.0). Each line is one Noewright graph (`captions_graph.go`): the English caption
(`sc-caption`) needs only the spoken line, so it streams at once, in parallel
with the detector; `sc-yo`, `sc-ig` and `sc-pcm` start when both are done, each
handed its own language's Bible text by a pre-hook. A visitor can run every agent
on one model and key, or pick a model per agent (keys per provider, in
`X-LLM-Key-Anthropic` / `X-LLM-Key-OpenAI`); an agent without a key for its
provider runs dry. A post-hook, the scripture guard, checks that a passage
read aloud appears word for word in each caption (in-order word coverage of at
least 60%): a miss sends the step back once with `ErrRetryWith`, a second miss is
let through and flagged. The finished line is recorded (`sc-line`) and
reaches the screen before `sc-judge` scores it: the scores follow in a recorded
`sc-score` step, so the judge no longer delays the line. A line sent while the
previous one is still being scored waits for it. The line step (`sc-line`) whose hook appends it to the session's state; the
follow stream (`/captions/{id}/follow`, anyone with the id) replays the service
and forwards new lines from `Engine.Subscribe`. Without a visitor key, lines run
on `CAPTIONS_ANTHROPIC_API_KEY` while `CAPTIONS_LINES_PER_HOUR` (all visitors)
and `CAPTIONS_LINES_PER_VISITOR` (per day) last, then as a dry run, which shows
the real Bible text and a guard retry but no translation. Sessions are purged
after 7 idle days.

The *Client asks for loom* doc records what the demo needed from loom; v0.0.3
delivered all of it, and the demo's workarounds are gone. Noewright's GC runs with
retention rules: pipeline sessions are purged after 48 idle hours, tabletop
sessions after 8 days, Forecast Desk sessions after 7.

## Layout

```
gateway/            Caddyfile (production, CORS), Caddyfile.local, Dockerfile
services/go/        one Go module; internal/platform is shared plumbing
  cmd/hub           feedback API
  cmd/buckt         buckt sandbox API
  cmd/loom          engine, tabletop (game.go, rpg.go), Forecast Desk (desk.go, desk_sim.go),
                    sermon captions (captions.go, bible.go, bible/), pipeline,
                    dry-run generator, key check
services/ussd/      Rust/actix front for ussdframework; menu.json is QuickLoan
                    (laundryflow has no directory here: CI checks out its repo
                    and builds its Dockerfile; locally, ../binaryhouse/flow/laundryflow)
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
| `make run-loom` (etc.) | Run one service natively, no Docker (`make run-laundryflow` needs the laundryflow repo, see `LAUNDRYFLOW_DIR`) |
| `make bump-noewright v=v0.1.1` | Move to a new Noewright release |
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
the Noewright demo runs as a dry run: the orchestration, hooks, forks, budgets and ledger
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
   cloud), in **both** zones: rhaqim.com and thebinaryhouse.studio. SSL/TLS
   mode **Full (strict)** in each zone. Cloudflare keeps SSE open as long as
   bytes flow; the game stream sends a keep-alive every 20 s.
3. **Origin certificates, one per host.** In each zone: SSL/TLS → Origin
   Server → Create certificate.
   - `api.rhaqim.com` → `/opt/demo/certs/origin.pem` and `origin-key.pem`
   - `api.thebinaryhouse.studio` → `/opt/demo/certs/tbh-origin.pem` and
     `tbh-origin-key.pem`

   `chmod 600` all four. Caddy will not start while any of them is missing,
   which would take both hosts down, so CI checks for all four before it
   restarts anything. The setup script asks for any that are missing.
4. **Firewall.** Allow SSH, and 80/443 **only from Cloudflare**. The services
   rate-limit on `CF-Connecting-IP`, which anyone who reaches the box directly
   could forge. Hetzner's Cloud Firewall with Cloudflare's ranges
   (https://www.cloudflare.com/ips/) is simplest; ufw alone isn't enough,
   because Docker publishes ports through iptables and bypasses it.
5. **Environment.** Copy `.env.example` to `/opt/demo/.env`; set `ADMIN_TOKEN`
   (`openssl rand -hex 32`). If the portfolio is served from another origin
   (a Pages preview domain), widen `CORS_ORIGINS`.
6. **GitHub.** Add the secrets and variables listed at the top of
   `.github/workflows/deploy.yml` and an environment called `production`.
   `LAUNDRYFLOW_DEPLOY_KEY` lets CI check out the private laundryflow repo:
   run `ssh-keygen -t ed25519 -N "" -C portfolio-demo-ci -f lf_deploy`, add
   `lf_deploy.pub` under laundryflow → Settings → Deploy keys (leave *Allow
   write access* off), and paste `lf_deploy` as the secret here. Without it
   the laundryflow build fails. A push to
   laundryflow does not redeploy by itself: run this workflow by hand
   (*Run workflow*) to pick it up, or set `LAUNDRYFLOW_REF` to pin a version. Hetzner
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
  handler checks ownership. Noewright pipeline agents live under
  `owner = visitor:<id>`. Game actions check host or player membership; codes
  are 6 characters from a 31-letter alphabet (~887 million) behind a
  wrong-code limiter.
- **LLM keys.** Sent in `X-LLM-Key`, passed to generators through
  `StepRequest.Overrides`, which Noewright never persists. Per-request `base_url`
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
- **Noewright** `v0.1.0` (`github.com/rhaqim/noewright`, formerly
  `github.com/rhaqim/loom`), with its Anthropic adapter at
  `github.com/rhaqim/noewright/generator/anthropic` `v0.1.0`. Both are tagged,
  so `make bump-noewright v=<version>` moves them together.
