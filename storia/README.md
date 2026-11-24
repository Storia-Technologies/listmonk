# Storia's listmonk

This is [listmonk](https://github.com/knadh/listmonk) with two additions, built for Storia's Hosted Apps tenants:

1. **Sequences**: drip campaigns. A timed series of e-mails sent to each person who joins a list.
2. **Claude connector**: every instance is an MCP server, so a tenant can add their listmonk to Claude and have Claude work in it for them.

Everything else is upstream listmonk, unchanged. listmonk is AGPL-3.0 and so is this fork: anyone using an instance of it is entitled to this source.

- Branch: `sequences`, based on upstream tag `v6.2.0`.
- Image version: `v6.2.0-storia.N` (upstream version, then our build number).
- Decision record: `decisions/log.md` in the AIOS repo (2026-11-16 and 2026-11-24). Business context: the Hosted Apps page in Storia SB.

## Contents

- [How the fork is kept mergeable](#how-the-fork-is-kept-mergeable)
- [Sequences](#sequences)
- [Claude connector](#claude-connector)
- [Building](#building)
- [Testing](#testing)
- [Deploying to a tenant](#deploying-to-a-tenant)
- [Rolling back](#rolling-back)
- [Taking an upstream release](#taking-an-upstream-release)
- [Known limits](#known-limits)

## How the fork is kept mergeable

Our code lives in its own files. Upstream files only carry one-line hooks into them.

| Ours (new files) | What it is |
| --- | --- |
| `cmd/sequences.go` | Sequences: tables, API, engine |
| `cmd/mcp.go` | Connector: OAuth sign-in, MCP endpoint, admin API |
| `cmd/mcp_tools.go` | Connector: the tools |
| `frontend/src/views/Sequences.vue`, `Sequence.vue` | Sequences screens |
| `frontend/src/views/Connector.vue` | Settings > Claude connector screen |
| `Dockerfile.storia` | Builds the image from source |
| `storia/` | This file, the local test stack and the end-to-end tests |

| Upstream file | Our hook |
| --- | --- |
| `cmd/handlers.go` | `initSequenceHandlers(g, a)` and `initMCPHandlers(e, a)` |
| `cmd/main.go` | `app.initSequences()` and `app.initMCP()` |
| `cmd/campaigns.go` | `checkSequenceCampaign` in `UpdateCampaignStatus` |
| `cmd/admin.go` | `sequences` and `mcp` flags on the server config |
| `frontend/src/api/index.js` | API functions appended at the end |
| `frontend/src/router/index.js` | three routes |
| `frontend/src/components/Navigation.vue` | two menu items |

Rules we keep to so this stays true:

- **Our tables are created at startup** (`CREATE TABLE IF NOT EXISTS` in `seqSchema` and `mcpSchema`), not in `schema.sql` or the versioned migrations in `internal/migrations`. Upstream's migrations are keyed on its version numbers, and ours must never collide with them.
- **Our SQL is inline** in our Go files, not in `queries/*.sql`, which map onto an upstream struct.
- **No new permissions.** Sequences use the existing `campaigns:*` permissions and the connector screen uses `settings:*`.
- **Screen text is plain English in the views**, not in `i18n/`, which upstream regenerates.

Upstream has announced that the Vue admin is being replaced in v7. Our three views will need rewriting then. The backend, the connector and the tests are unaffected.

## Sequences

### What a tenant sees

Campaigns > Sequences. A sequence watches one list. Anyone who joins that list after the sequence is switched on gets each e-mail in order, each a set wait after the one before. The People tab shows who is at which step.

Each e-mail is a normal draft campaign, written in the normal campaign editor. "Write a new e-mail" creates the draft, addresses it to the sequence's list, tags it `sequence` and opens the editor.

### Rules

- **Switching on never e-mails people already on the list.** The first switch-on sets a cut-off (`sequences.activated_at`). Only subscriptions created after it are enrolled.
- **Who counts as joined** matches how campaigns pick recipients: on a single opt-in list, anyone not unsubscribed; on a double opt-in list, only once confirmed. Blocklisted people are never enrolled.
- **Waits are relative.** The first step waits after joining, each later step waits after the previous one was actually sent. So pausing a sequence shifts everything rather than causing a burst on resume.
- **A person leaves the sequence** when they unsubscribe from the list, are blocklisted (which is what a hard bounce does), are removed from the list, are added to one of the sequence's exit lists, or are removed by hand. The reason is recorded and shown.
- **Paused means paused.** Nobody is enrolled or sent anything while a sequence is off. People who join during a pause are enrolled when it comes back on.
- **A step's e-mail is protected.** It can't be started or scheduled as a normal campaign (that would send it to the whole list), and it can't be deleted while it is a step.
- **A step's e-mail must stay addressed to the sequence's list.** The unsubscribe link in a campaign unsubscribes the person from that campaign's lists, and that is what stops the sequence for them. Adding a step checks this, and the engine checks again before each send. If the list has been taken off the e-mail, the sequence shows an error and retries every minute until it is fixed.
- **Nobody gets a step twice.** `sequence_sends` has a unique key on (step, subscriber), written before the e-mail is queued.

### How it works

`initSequences` starts a goroutine that runs `runSequences` once a minute. For each active sequence it: enrols new joiners, exits people who have left, marks people who have had the last step as finished, then finds each active person's next step whose wait has passed (up to 500 per sequence per pass) and sends it.

Sending reuses listmonk's campaign test-send path: the step's campaign is compiled once per pass, rendered for the subscriber with `manager.NewCampaignMessage` and queued with `manager.PushCampaignMessage`. It goes out through the normal sending workers, so the instance's message rate limit applies, and opens, clicks, the unsubscribe link and bounces are attributed to the step's campaign as for any campaign. The campaign itself stays in `draft` status.

If a send can't be queued or rendered, its `sequence_sends` row is removed so it is retried on the next pass, and the error is stored on the sequence (`last_error`) and shown at the top of its page.

### Tables

| Table | Holds |
| --- | --- |
| `sequences` | name, starting list, exit lists, status (`active`/`paused`), `activated_at`, `last_error` |
| `sequence_steps` | sequence, campaign (unique: a campaign can be one step only), position, `delay_minutes` |
| `sequence_enrolments` | one row per person per sequence: status (`active`/`completed`/`exited`), exit reason, how far they have got, when the last step was sent |
| `sequence_sends` | one row per step per person sent |

Deleting a list that starts a sequence, or a campaign that is a step, is blocked by a foreign key (the constraint names say why). Deleting a sequence removes its steps and history, and leaves its e-mails as ordinary drafts.

### API

All under the normal authenticated API. Read needs `campaigns:get` or `campaigns:get_all`, changes need `campaigns:manage` or `campaigns:manage_all`, switching on or off needs `campaigns:send`.

```
GET    /api/sequences
GET    /api/sequences/:id
GET    /api/sequences/:id/subscribers?status=&page=&per_page=
POST   /api/sequences                          {name, list_id, exit_list_ids}
PUT    /api/sequences/:id                      {name, list_id, exit_list_ids}
PUT    /api/sequences/:id/status               {status: "active" | "paused"}
DELETE /api/sequences/:id
POST   /api/sequences/:id/steps                {campaign_id, delay_minutes}
PUT    /api/sequences/:id/steps/order          {ids: [step ids in order]}
PUT    /api/sequences/:id/steps/:stepID        {delay_minutes}
DELETE /api/sequences/:id/steps/:stepID
DELETE /api/sequences/:id/subscribers/:subID   remove one person from the sequence
```

### Switching it off for a tenant

Set `LISTMONK_sequences__enabled=false` in the tenant's environment. The menu item, API and engine all go away. It is on by default.

## Claude connector

### What a tenant sees

Settings > Claude connector shows the address to paste into Claude (`https://<their-listmonk>/mcp`), a switch for "Let Claude schedule and send", and the list of what is connected with a Disconnect link.

To connect: in Claude, Settings > Connectors > Add custom connector, paste the address. Claude opens the tenant's listmonk, they sign in with their normal login, and approve. Claude then works in listmonk as that user, with that user's permissions.

### What Claude can and can't do

| Always | Only with "Let Claude schedule and send" on | Never |
| --- | --- | --- |
| Read lists, subscribers, campaigns, templates, sequences and results | Schedule a campaign | Delete anything |
| Create lists | Start sending a campaign now | Change settings, users or templates |
| Add subscribers, change which lists they are on | Switch a sequence on | Import or export in bulk |
| Create and edit draft campaigns | | |
| Send a test e-mail to named addresses | | |
| Create sequences and add e-mails to them | | |
| Unschedule or pause a campaign, switch a sequence off | | |

The switch is off by default and lives in `mcp_config.allow_sending`. With it off, the sending tools answer with an explanation and a link to the draft. With it on, the server's instructions tell Claude to show the subject, lists, recipient count and time and get a clear yes first. That is guidance to the model, not an enforced step: the switch is the enforced control.

One thing to know: adding a subscriber to a list is always allowed, and if that list starts a sequence that is switched on, the person will get the sequence. The tool descriptions say so.

### Tools

`get_overview`, `list_lists`, `create_list`, `search_subscribers`, `add_subscriber`, `change_subscriber_lists`, `list_campaigns`, `get_campaign`, `list_templates`, `create_campaign`, `update_campaign`, `send_test_email`, `schedule_campaign`, `stop_campaign`, and, when sequences are enabled, `list_sequences`, `get_sequence`, `list_sequence_people`, `create_sequence`, `add_sequence_email`, `set_sequence_email_wait`, `set_sequence_status`.

Each tool runs an existing listmonk HTTP handler in-process as the signed-in user (`mcpInvoke` in `cmd/mcp_tools.go`), wrapped in the same permission check the REST route uses. So validation, per-list permissions and side effects are exactly what the admin UI gets, and a tool can never do more than its user could do by hand. To add a tool, add an entry to `mcpTools`.

### How sign-in works

The instance is its own OAuth 2.1 authorization server, following the MCP authorization spec (2025-11-25).

1. Claude calls `POST /mcp` with no token and gets `401` with a `WWW-Authenticate` header pointing at `/.well-known/oauth-protected-resource/mcp`.
2. That document names the authorization server (the instance itself). `/.well-known/oauth-authorization-server` lists the endpoints.
3. Claude registers itself at `POST /mcp/oauth/register` (Dynamic Client Registration). Clients are public: no secret, PKCE required.
4. Claude sends the person's browser to `GET /mcp/oauth/authorize`. If they aren't signed in they go through listmonk's own login first. They see the approval page and choose Connect or Cancel.
5. Claude swaps the code at `POST /mcp/oauth/token` for an access token (1 hour) and a refresh token (60 days, replaced on every use).

Details worth knowing:

- **Login return address.** listmonk's login keeps only the path of where to go next and drops the query. So an authorization request that arrives without a session is parked in `mcp_login_resumes` and picked up at `/mcp/oauth/resume/:id` after login.
- **Tokens and codes are stored hashed** (SHA-256), never in the clear. Codes work once and last 5 minutes.
- **Tokens are opaque and only valid on the instance that issued them.** A request for a token for any other resource is refused (`invalid_target`).
- **A token is a listmonk user.** If the user is disabled or deleted, the token stops working. Disconnecting in the settings screen deletes the token straight away.
- **Redirect addresses** must be https, or http on localhost, and must match what the client registered exactly.
- **Origin check.** A request with an `Origin` header is refused unless it is the instance itself, localhost, or claude.ai / claude.com. Server-side clients send no Origin.
- **Not supported:** Client ID Metadata Documents (clients fall back to Dynamic Client Registration, which the spec allows), scopes, SSE streams and sessions. The endpoint answers each POST with plain JSON.

### Tables

| Table | Holds |
| --- | --- |
| `mcp_config` | one row: `allow_sending` |
| `mcp_clients` | registered clients and their redirect addresses |
| `mcp_auth_requests` | approvals in progress and one-time codes |
| `mcp_login_resumes` | authorization requests parked during login |
| `mcp_tokens` | one row per connection: client, user, hashed tokens, expiry, last used |

An hourly job clears expired rows and clients that registered but never completed a sign-in.

### Requirements

- The instance's **root URL setting must be its real public https address** (`app.root_url`). The connector builds every address it hands to Claude from it. Hosted Apps provisioning already sets this.
- The instance must be reachable from the internet at that address, which every hosted tenant is.

### Switching it off for a tenant

Set `LISTMONK_mcp__enabled=false`. The endpoint, sign-in routes and settings screen all go away. It is on by default.

## Building

Only Docker is needed.

```
docker build -f Dockerfile.storia \
  --build-arg VERSION=v6.2.0 --build-arg STORIA_BUILD=storia.2 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t storia-listmonk:6.2.0-storia.2 .
```

`VERSION` must stay the upstream version the branch is based on. listmonk uses it to decide which database migrations to run. The build runs the frontend lint, so a lint error fails it.

Check what an image is: `docker run --rm --entrypoint ./listmonk <image> --version`.

## Testing

A local stack with a mail catcher, so no e-mail leaves the machine:

```
docker build -f Dockerfile.storia -t storia-listmonk:dev .
docker compose -p seqtest -f storia/test/docker-compose.yml up -d
docker logs seqtest_app 2>&1 | grep LISTMONK_ADMIN_API_TOKEN     # the API token for the tests

python storia/test/e2e.py <token>        # Sequences, about 5 minutes
python storia/test/e2e_mcp.py <token>    # Claude connector, a few seconds

docker compose -p seqtest -f storia/test/docker-compose.yml down -v
```

- listmonk: http://localhost:9100/admin (`admin` / `storia-test-1`)
- Mailpit: http://localhost:8125

`e2e.py` covers timed sends, unsubscribing by API and by the real link in the e-mail, exit lists, removing by hand, pause and resume, re-ordering, and no duplicates. `e2e_mcp.py` plays the part of Claude: discovery, registration, the real login and approval pages, PKCE, every tool, the sending switch, token refresh and disconnecting.

Run both before every deploy. Neither replaces connecting a real Claude to a real tenant, which is the only test of the connector against Claude itself.

## Deploying to a tenant

Tenants run from a compose file on their box at `~/clients/<slug>/docker-compose.yml` (root-owned), created by the Hosted Apps control plane. Moving a tenant onto this build means changing the `image:` line of its `app` service. The first tenant (16 Nov 2026) was moved by hand as below. This should become an upgrade script in `storia-hosted-apps` before more tenants move.

1. Build and tag the image, then get it onto the tenant's box. Until the image is published to a registry: `docker save <image> | gzip > file.tar.gz`, copy it to the box, `gunzip -c file.tar.gz | docker load`.
2. Check no campaign is sending: `select count(*) from campaigns where status = 'running'`. Restarting mid-send interrupts it.
3. Back up: `docker exec listmonk_db_<slug> pg_dump -U listmonk listmonk | gzip > ~/deploy-backups/<slug>-listmonk-<when>.sql.gz` and copy the compose file next to it.
4. Note the counts of subscribers, lists, campaigns and scheduled campaigns.
5. Change the image line and recreate only the app: `cd ~/clients/<slug> && docker compose up -d --no-deps app`.
6. Check: the log says `http server started`, `./listmonk --version` shows the new build, the counts are unchanged, and the login page and public subscription form answer 200.

Moving between two of our builds on the same upstream version changes no upstream tables. Our own tables are created or extended on boot.

## Rolling back

Put the saved compose file back and run `docker compose up -d --no-deps app`. Stock listmonk ignores our `sequence*` and `mcp_*` tables, so they can stay. Sequences stop running and connected Claudes stop working until the tenant is moved forward again.

## Taking an upstream release

1. `git fetch upstream --tags`, then merge or rebase `sequences` onto the new tag. Conflicts should be limited to the hook lines in the table above.
2. Check the handlers our tools call still exist with the same request shapes: everything passed to `mcpInvoke` in `cmd/mcp_tools.go`, and `NewCampaignMessage` / `PushCampaignMessage` / `CompileTemplate` used by the sequence engine. A changed signature fails the build. A changed request shape only shows up in the tests.
3. Build with the new `VERSION`, run both end-to-end tests, then deploy to one tenant first.

## Known limits

Sequences:

- People already on a list can't be enrolled. Only new joiners are.
- No branching on opens or clicks. A sequence is one straight line.
- A step goes out a set wait after the previous one, not at a chosen time of day.
- A user with per-list permissions can see every sequence's name and counts, not only those for lists they can access.
- Delivery failures after an e-mail is queued are logged by listmonk but not shown on the sequence, the same as for a campaign test send.

Connector:

- Not yet tested against a real Claude client, only against the scripted client in `e2e_mcp.py`.
- No rate limiting on client registration beyond the hourly clean-up.
- The "confirm before sending" step relies on the model following instructions. The enforced control is the sending switch.
- Everything a connection can do is set by its user's listmonk role. There are no finer scopes per connection.
