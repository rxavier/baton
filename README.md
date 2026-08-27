# baton

Pass the listening baton between your devices. **baton** bridges MPD's play queue to
Navidrome's Subsonic play queue (`savePlayQueue` / `getPlayQueue`), so you can continue
listening where you left off — Spotify-style — whether you pick up MPD (e.g. Malp on Android)
or a Subsonic client (Feishin, Tempo, Tempus, Resonus).

The shared source of truth is **Navidrome's per-user play queue**. MPD is treated as just
another device that reads and writes that queue, keeping everyone in sync. A single
statically-linked binary with no dependencies; runs as a systemd service, a container, or a
one-shot process.

> **⚠️ Requirement: MPD and Navidrome must read the SAME music library.**
> baton matches songs between the two by tag (`title`/`album`/`artist`/`track`/`disc`). That
> only works because both servers index the *same* files on disk. If MPD's `music_directory`
> and Navidrome's music folder point at different locations, songs won't resolve and sync will
> silently fail.

## How it works

Two sync directions, both driven by polling every `POLL_INTERVAL` seconds (Navidrome has no
event/subscription API for queue changes):

- **MPD → Navidrome (push).** When MPD's queue or current song changes, send the full ordered
  queue + current song + intra-song position via `savePlayQueue`. On startup the daemon seeds a
  baseline **without** pushing, so a freshly-restarted daemon never clobbers a newer queue that
  another device saved while it was down.
- **Navidrome → MPD (pull).** Only acts when MPD is stopped or paused (never stomps active
  playback). If the remote queue differs and is newer than our last push (staleness guard), it
  replaces MPD's queue and sets the current-song cursor. If only the remote *current* song
  changed (same queue), it just moves the cursor — the mid-list device handoff. The remote
  intra-song position is restored too.

### Song identity: matched by tag, not path

MPD addresses songs by their real on-disk path; Navidrome's Subsonic `path` field is
*simulated* and does not match disk. So baton matches songs by **tag fields** (`title`,
`album`, `artist`, `track`, `disc`), which match exactly because both sides read the same
files. Lookups are cached in memory per session. This is why baton needs **no media-folder
setting** — it never reads your music files.

### Cursor and position

MPD has no command to set the current song without starting playback. To move the cursor baton
uses `play <pos>` then immediately stops or pauses. This is silent because MPD has no audio
output configured; if you add an output it will blip briefly.

When a remote queue carries a position, baton restores it with `play <pos>` + `seekcur <ms>`,
then **pause** (not stop) — MPD discards the position on `stop` (verified: seek to 0:42, stop,
play resumed at 0:01), while `pause` retains it. So a position-restoring pull leaves MPD paused
at the exact spot, and pressing play resumes mid-track.

## Install

### Release binary (any OS)

Prebuilt static binaries are attached to each GitHub [release](https://github.com/rxavier/baton/releases):
`baton-linux-amd64`, `baton-linux-arm64`, `baton-darwin-amd64`, `baton-darwin-arm64`,
`baton-windows-amd64.exe`. Linux builds are fully static — no runtime or libraries needed.

```sh
wget https://github.com/rxavier/baton/releases/latest/download/baton-linux-amd64
chmod +x baton-linux-amd64
./baton-linux-amd64 --env-file /path/to/baton.env
```

### systemd (recommended on Linux)

Requires root (it installs a system service into `/usr/local` and `/etc`):

```sh
sudo make install       # builds, installs to /usr/local/bin, creates
                        # /etc/baton/baton.env from .env.example, enables+starts
systemctl status baton
```

Edit `/etc/baton/baton.env` to set your credentials, then `sudo systemctl restart baton`.

Not running as root? Skip systemd and run the binary directly with your own config —
no root needed: `baton --env-file /path/to/your.env`.

### docker compose

Docker access needs root privileges (sudo, or a user in the `docker` group).

docker compose reads a `.env` file **in this directory** (or your shell environment) and
injects the values as environment variables into the container — it does **not** read
`/etc/baton/baton.env`. It must define `NAVIDROME_URL`, `NAVIDROME_USER` and
`NAVIDROME_PASSWORD` (compose refuses to start without them); everything else is optional with
the defaults in `docker-compose.yml` (notably `MPD_HOST` defaults to the bridge gateway
`172.17.0.1`):

```sh
cp .env.example .env    # docker compose reads THIS .env, next to docker-compose.yml
# edit NAVIDROME_USER / NAVIDROME_PASSWORD (and any overrides)
docker compose up -d --build
```

### From source

```sh
make build      # single static binary ./baton
make dist       # cross-compiles for 5 platforms into dist/ + SHA256SUMS
make test       # go test ./...
```

## Configuration

baton reads configuration from three places, in precedence order: **CLI flags, environment
variables, and an optional `--env-file`**. There's no magic config location — you point it at a
file explicitly, or set the keys directly.

```sh
cp .env.example mybaton.env   # edit NAVIDROME_USER / NAVIDROME_PASSWORD
baton --env-file mybaton.env
```

Or skip the file entirely:

```sh
NAVIDROME_USER=me NAVIDROME_PASSWORD=secret baton
baton --navidrome-user me --navidrome-password secret
```

The systemd install provisions a config at `/etc/baton/baton.env`, injected via the unit's
`EnvironmentFile`; non-root users just point `--env-file` at any file they own. A missing
`--env-file` path is a hard error.

Use the **same Navidrome account** as your music clients so they all share one queue.

| Env var                 | Default                 | Meaning                                        |
|-------------------------|-------------------------|------------------------------------------------|
| `NAVIDROME_URL`         | — (required)            | Navidrome URL or `host:port` (http:// is added)|
| `NAVIDROME_USER`        | — (required)            | Navidrome username                             |
| `NAVIDROME_PASSWORD`    | — (required)            | Navidrome password                             |
| `NAVIDROME_LEGACY_AUTH` | `0`                     | `1` for plaintext `p=` auth                    |
| `MPD_HOST`              | `127.0.0.1`             | MPD host (docker default `172.17.0.1`)         |
| `MPD_PORT`              | `6600`                  | MPD port                                       |
| `MPD_PASSWORD`          | —                       | Optional MPD password                          |
| `POLL_INTERVAL`         | `5`                     | Poll interval (seconds) for both directions    |
| `BATON_LISTEN`          | `127.0.0.1:8080`        | Status API + web UI address                    |
| `BATON_API_TOKEN`       | —                       | Optional bearer token for the API              |
| `BATON_STATE_FILE`      | `~/.local/state/baton/mode` | Where the runtime sync mode is persisted   |

### Sync modes

- `auto` — push and pull on the poll ticker.
- `push` / `pull` — only that direction on the ticker.
- `manual` — no automatic sync; sync only when `POST /api/push` or `POST /api/pull` is called.
  Useful for a dev instance running alongside the production service, or for a "press a button
  when I want it" workflow.

The mode defaults to `manual` at startup and is **set from the web UI** (the dropdown in the
header), not from config — there is no `MODE` env var. The current mode is persisted to
`BATON_STATE_FILE` on every change and restored on startup, so it survives restarts; if that
file can't be written (e.g. an unwritable location, or Docker without a mounted volume) baton
still switches mode, it just doesn't survive a restart.

## Web UI & API

The binary embeds a web UI (single page, no build step) served at the `BATON_LISTEN`
address: a **Now playing** bar showing both the current MPD and Navidrome songs, side-by-side
**MPD** and **Navidrome** queue columns (tagged entries, current song highlighted, one column
on mobile), **Push** / **Pull** buttons for manual sync, and a **mode** dropdown.

- `GET /api/status` — MPD state, current song + position, the full MPD queue (tagged), the
  Navidrome queue (`remote_queue` with title/artist/album/track), remote `changed`, last push.
- `POST /api/push` — force a push now.
- `POST /api/pull` — force a pull now.
- `POST /api/mode` — `{"mode":"auto|manual|push|pull"}` to switch sync mode at runtime
  (persisted to `BATON_STATE_FILE`).

When `BATON_API_TOKEN` is set, `/api/*` requires `Authorization: Bearer <token>` (the UI has a
token field so it still works).

## Design notes

- **Single static binary.** Zero external Go dependencies, `CGO_ENABLED=0`; the web UI is
  embedded with `//go:embed`. One file to ship, nothing to serve separately.
- **MPD client** is written directly against the MPD TCP protocol (no library): commands are
  lines, responses are `key: value` blocks terminated by `OK`. A mutex guards the connection so
  the API handlers and the poll loop can share it; reconnect is automatic.
- **Polling, not idle.** Navidrome has no queue-change subscription API, and MPD `idle()` blocks
  the loop and leaves the socket in a bad state after timeout (both problems hit the Python
  prototype). Polling every `POLL_INTERVAL` is simpler and robust.
- **Staleness guard.** `getPlayQueue` returns a `changed` timestamp; baton only pulls when it is
  newer than the last successful push, so an older remote queue never clobbers newer MPD state.
  Conservative by design — under clock skew a genuinely-new remote queue may occasionally be
  skipped rather than clobbering.
- **Tag matching includes duration.** MPD→Navidrome resolution and Navidrome→MPD lookup compare
  title/artist/album/track/disc *and* duration (±2s), so two versions of the same song can't be
  confused with each other.
- **Position restore ends in pause.** MPD drops the intra-song position on `stop`, so restoring
  a position leaves MPD paused. Trade-off: a position-restoring pull shows `paused` instead of
  `stopped`.
- **Rate-limit friendly.** The web UI's `/api/status` serves the Navidrome queue from a cache
  refreshed at most every 10s (or by the poll loop), instead of calling `getPlayQueue` on every
  3s page poll.

## Development & testing

```sh
make test
```

The test suite spins up fake MPD and fake Navidrome servers over real sockets and drives the
sync engine end-to-end: push on change, no startup push (baseline seed), pull-load, cursor-only
handoff, position restore, staleness guard, don't-stomp-while-playing, MPD password auth, and
config-file precedence. `TestRealMPD` additionally runs against a live local MPD and is skipped
when one isn't reachable.

## Known limitations

- **One queue per Navidrome user.** The daemon and all Subsonic clients share the same per-user
  queue; "last active device wins" is the intended handoff design.
- **Empty MPD queue never clears Navidrome's saved queue.** Clear the server queue in a client.
- **Unresolvable songs are skipped** — a song that can't be matched (e.g. not yet scanned) is
  dropped from that sync cycle.
- **Caches are in-memory** and reset on restart (harmless; lookups are cheap).
- **Cursor moves use `play <pos>` + `stop`/`pause`** — silent only because MPD has no audio
  output; if you add one, cursor moves and position restores will blip audio.

## License

MIT