# Origin Media Server

A live-streaming origin server in Go that ships as a single, CGO-free binary.
It takes streams in over RTMP/RTMPS, SRT, WHIP, Icecast, MPEG-TS and pull
sources, and serves them as HLS, LL-HLS, DASH, WHEP and raw MPEG-TS, with
nDVR rewind, ABR, SCTE-35, captions and failover. HLS, DASH, WHIP/WHEP, the
REST API and the management panel are all served on one HTTP port.

## Features

| Area | What's supported |
|---|---|
| Ingest | RTMP/RTMPS (incl. FMLE multi-bitrate), SRT (listener and caller), WHIP, Icecast (MP3/AAC), MPEG-TS over UDP/HTTP, RTSP, managed pull sources with reconnect |
| Codecs | H.264 / HEVC video, AAC / MP3 / MP2 audio |
| Output | HLS (TS, fMP4, CMAF), LL-HLS, DASH and DVB-DASH, WHEP, continuous HTTP MPEG-TS |
| ABR | Manual groups built in the panel, and automatic FMLE `%i` groups (`test1`, `test2`, … → group `test`) |
| nDVR | One fMP4 recording per stream serves HLS and DASH rewind: whole window, time range (VOD) and timeshift. ABR groups get an HLS rewind master. |
| Ads | SCTE-35 passthrough and API-triggered insertion, HLS ad markers |
| Captions | CEA-608/708, DVB Teletext, WebVTT, EBU-TT-D (`stpp`) for DASH |
| Reliability | Stall watchdog with backup-source failover |
| Ops | REST API with JWT auth, roles and API tokens; Prometheus metrics; multi-node discovery over NATS; embedded Next.js panel |
| Auth | Per application: RTMP stream key or Adobe auth (user/password, as FMLE prompts for), SRT `?key`, WHIP bearer token, Icecast password |

## Quick start

Requirements: Go 1.25+, and Node.js/npm to build the panel.

```sh
make panel-build   # builds panel/out/, which the binary embeds (needed before go build)
make dev           # creates config.toml from config.example.toml if missing, then runs
```

Open the panel at http://localhost:8080/ and log in with the bootstrap admin
from `config.toml` (`admin@localhost` / `admin` by default). **Change the
password and `auth.jwt_secret` before exposing the server.**

Publish a test stream:

```sh
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://localhost:1935/live/test
```

and play it at `http://localhost:8080/live/test/playlist.m3u8`.

## Build

```sh
make build         # ./origin-mediaserver (CGO_ENABLED=0)
make build-static  # ./origin-mediaserver-linux-amd64, fully static
make run           # build, then run with config.toml
make test          # go test ./...
make panel         # panel dev server on :3000, proxies the API to :8080
```

`panel/out/` isn't committed, so run `make panel-build` once after cloning
and again whenever the panel changes.

## Configuration

Configuration is TOML. [`config.example.toml`](config.example.toml) is the
documented reference for every option. Copy it to `config.toml` (which is
git-ignored, since it holds secrets) and edit. Applications, stream configs,
ABR groups, pull sources and failover rules can also be managed in the panel
or the API; they're stored in SQLite under `server.data_dir`.

Default ports:

| Port | Use |
|---|---|
| 8080 | HTTP: playback, WHIP/WHEP, API, panel, metrics |
| 1935 / 1936 | RTMP / RTMPS |
| 4000 (UDP) | SRT |
| 8005 | Icecast |
| 5000 (UDP) | MPEG-TS |

## URLs

Ingest (app `live`, stream `test`):

```
rtmp://<host>:1935/live/test            # add ?<key> or use Adobe auth if the app requires it
rtmps://<host>:1936/live/test
srt://<host>:4000?streamid=live/test?<key>
POST http://<host>:8080/whip/live/test  # Authorization: Bearer <publish_key>
```

Playback:

```
/live/test/playlist.m3u8           HLS master (single stream or ABR group)
/live/test/chunks.m3u8             HLS media playlist
/live/test/manifest.mpd            DASH
/live/test/mpeg.2ts                continuous MPEG-TS
POST /whep/live/test               WebRTC playback
```

nDVR rewind (when enabled globally, per application or per stream):

```
/live/test/playlist_dvr.m3u8                          whole rewind window, live
/live/test/playlist_dvr_range-<unix_from>-<secs>.m3u8 fixed range (VOD once in the past)
/live/test/playlist_dvr_timeshift-<delay>[-<depth>].m3u8  delayed live
/live/test/manifest_dvr*.mpd                          the same three for DASH
```

For an ABR group, `/{app}/{group}/playlist.m3u8` and
`/{app}/{group}/playlist_dvr*.m3u8` are masters whose variants live under the
group (`{srcApp}/{srcStream}/chunks.m3u8` or `.../playlist_dvr*.m3u8`).

API and monitoring:

```
/api/v1/...                        REST API (POST /api/v1/auth/login for a token)
/metrics                           Prometheus
```

`make urls` prints more examples.

## Project layout

```
cmd/server/            wiring: config, ingest servers, outputs, API
internal/media/        core types: Stream, Bus, Packet, registry, codec parsing
internal/rtmp|srt|webrtc|icecast|ingest/|pull/   ingest protocols
internal/hls|dash|tsout|dvr|mux/                 packaging and output
internal/streamoutputs/  per-stream effective settings, starts/reloads outputs
internal/scte35|scte35ctrl/  SCTE-35 parsing, state, API-triggered insertion
internal/captiondecode|teletext|webvtt|ebuttd/  captions
internal/failover/     stall watchdog and backup switching
internal/api/          chi router: REST API, delivery routes, WHIP/WHEP
internal/db/           SQLite schema and access (modernc.org/sqlite, pure Go)
internal/node/         multi-node discovery over NATS
internal/metrics/      Prometheus metrics
panel/                 Next.js management UI, embedded via go:embed
```

See [`CLAUDE.md`](CLAUDE.md) for design notes, conventions and known gaps.

## Known limitations

- SCTE-35 HLS ad markers need the `ts` container (no `emsg` for fMP4/CMAF yet).
- nDVR doesn't record HEVC or SCTE-35 markers, and ABR group rewind is HLS only.
- Manual ABR groups are HLS only; DASH ABR exists for `%i` groups.
- `hls.separate_audio_track` can't be combined with LL-HLS.
- Legacy SHOUTcast v1 (passwordless) handshakes aren't supported.
