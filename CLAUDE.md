# Origin Media Server

A live-streaming origin server written in Go that ships as one binary
(module `github.com/5centscdn/origin-mediaserver`). It takes streams in over
RTMP/RTMPS, SRT, WHIP, Icecast, MPEG-TS and pull sources, and serves them
as HLS, LL-HLS, DASH, WHEP and raw MPEG-TS. Everything (HLS, DASH, WHIP/WHEP,
REST API and the panel) is served on one HTTP port.

## Build and run

```sh
make build         # CGO_ENABLED=0 single binary -> ./origin-mediaserver
make build-static  # static linux/amd64 binary
make dev           # copies config.example.toml -> config.toml if missing, then go run
make run           # build + ./origin-mediaserver --config config.toml
make test          # go test ./...
make panel         # Next.js panel dev server on :3000 (proxies API to :8080)
make panel-build   # builds panel/out/, which panel/panelfs.go embeds
make urls          # prints example ingest/playback URLs
```

- Never require CGO. The muxers are pure Go, SQLite is `modernc.org/sqlite`
  and SRT is `datarhei/gosrt`.
- `panel/panelfs.go` uses `//go:embed all:out`, so `panel/out/` must exist
  before `go build`. Run `make panel-build` after changing the panel.
- Config is TOML. `config.example.toml` is the documented reference; keep it
  up to date whenever a config field is added or changed.

## Layout

- `cmd/server/main.go`: wiring. Every ingest protocol's OnPublish/OnLive
  callback calls `streamoutputs.Start`.
- `internal/media`: the core types (`Stream`, `Bus`, `Packet`, registry,
  viewer counts, H.264/HEVC SPS parsing, SEI).
- Ingest:
  - `rtmp`: server, puller, FMLE `%i` ABR grouping (`abr.go`)
  - `srt`: listener and caller
  - `webrtc`: WHIP publish and WHEP sessions (pion)
  - `icecast`: MP3/ADTS audio only
  - `ingest/mpegts`: UDP, HTTP pull, demux
  - `ingest/rtsp`
  - `pull`: managed outbound pull sources with reconnect (rtmp, rtsp,
    mpegts_http, mpegts_udp, srt)
- Output:
  - `hls`: TS/fMP4/CMAF, LL-HLS, ad markers
  - `dash`: MPD, DVB-DASH, timeshift
  - `tsout`: continuous HTTP MPEG-TS
  - `dvr`: nDVR. Records each stream once as fMP4 (video moof+mdat then
    audio moof+mdat per file) and serves rewind from it: HLS uses the
    whole file, DASH serves each track as a byte range. URLs follow the
    standalone DVR server: `playlist_dvr.m3u8`, `playlist_dvr_range-*`,
    `playlist_dvr_timeshift-*` and `manifest_dvr*.mpd`. Index in SQLite
    (`dvr_sessions`, `dvr_segments`). Enabled globally, per app or per
    stream config, with a per-app/stream rewind window.
  - `mux`: pure-Go fMP4 and MPEG-TS muxers
- `streamoutputs`: works out each stream's effective app/stream DB settings
  and starts or reloads its outputs without touching the ingest connection.
- Captions:
  - `captiondecode`: CEA-608/708 decoding
  - `ebuttd`: EBU-TT-D `stpp` for DASH
  - `webvtt`
  - `teletext`: DVB Teletext
- SCTE-35:
  - `scte35`: parse, generate, dedup, state machine
  - `scte35ctrl`: API-triggered insertion timed on the stream's PTS clock,
    never wall clock
- `failover`: stall watchdog with backup switching and warmup buffer.
- `api`: chi router. REST under `/api/v1`, JWT auth with roles and per-user
  API tokens, delivery routes, WHIP/WHEP.
- `db`: SQLite schema and access for applications, streams, ABR groups, pull
  sources, failover rules and SCTE-35 events.
- `node`: multi-node peer discovery over NATS heartbeats
  (`GET /api/v1/nodes`). It's discovery only, not central management.
- `metrics`: Prometheus.
- `panel/`: Next.js 14 management UI with pages for dashboard, streams,
  applications, ABR, pull, DVR, failover, servers, users, settings and login.
- `src/`: legacy empty directory tree, not used.

## Conventions

- Keep decoders narrow and tested against real encoders. Write down the
  scope (what's supported and what's left out) in the package doc comment,
  as `captiondecode`, `teletext` and `ingest/mpegts` do.
- Package and function doc comments explain *why* and link to related code.
  Match that density.
- Media timing uses the stream's PTS domain, not wall-clock time.
- A reconnect creates a new `*media.Stream`/`Bus` under the same key, so
  per-stream state must be keyed by pointer, not just by `StreamKey`.
- Auth is per application. RTMP puts the key in the stream name, SRT uses
  `app/stream?key`, WHIP uses `Authorization: Bearer <publish_key>`, and
  Icecast uses the Basic auth password. RTMP also supports Adobe auth
  (`authmod=adobe`, user/password, which FMLE prompts for): a stateless
  three-connection exchange in `rtmp/adobeauth.go`. FMLE hashes with the
  server challenge, not the opaque; both are accepted.
- AMF0 objects go on the wire in a fixed key order (`amf0KeyOrder` in
  `rtmp/amf0.go`): FMS order for known keys (`level, code, description`,
  `fmsVer, capabilities, mode`, ...), then the rest alphabetically. Never
  encode by ranging over the `AMF0Object` map. Go map order is random, and
  FMLE 3.x exits when an `_error` starts with `description` (found in a
  pcap: an intermittent crash during Adobe auth). Add new keys to
  `amf0FieldRank` in FMS order.
- For encoder interop bugs, capture on the server (`tcpdump -U -w ... tcp
  port 1935 and host <client>`) and compare a working connection with a
  failing one byte by byte before changing code.

## Known gaps / follow-ups

- SCTE-35 HLS ad markers only work with the `ts` container. fMP4/CMAF
  streams get no markers yet (the `emsg` box is not implemented).
- `hls.separate_audio_track` (separate audio and video playlists) can't be
  used with `hls.ll_hls.enabled`. If both are set, it's ignored with a
  warning.
- Ingest codecs: H.264/HEVC video and AAC/MP3/MP2 audio.
- ABR has two sources, both served at `/{app}/{group}/playlist.m3u8` by
  `api.abrMaster` (`internal/api/abr_master.go`):
  - Manual groups: panel ABR editor (`panel/src/app/abr`, modelled on the
    standalone DVR server), stored in `abr_groups`/`abr_variants`. A
    variant can come from another app (`src_app_name`). Saved in one go
    via `PUT /abr-groups/{id}/tracks`.
  - Automatic `%i` groups: config `abr_patterns`, or a panel stream config
    like `test%i` → group `test`. FMLE publishes all renditions on one
    connection (`rtmp/abr.go`), and its `totalDatarate=` query param
    becomes the variant's bandwidth hint (`ABRBandwidthHint`).

  Manual wins on a name clash. Offline variants are left out of the master,
  and if all of them are offline the master returns 503 instead of an empty
  playlist (same as the DVR server's `hls.LiveABRMaster`). Any
  `EXT-X-STREAM-INF` field left empty/0 ("auto") is filled from the live
  stream (`streamInf`). BANDWIDTH comes from the configured value, then the
  encoder hint, then the measured ingest rate +20%. RESOLUTION/CODECS come
  from CodecInfo, so players can tell the renditions apart. Manual groups
  are HLS only; DASH ABR exists only for `%i` groups.
- Legacy SHOUTcast v1 (no password) Icecast handshakes are not supported.
- nDVR doesn't record HEVC (no `hvc1` sample entry in `mux`'s fMP4
  writer) or SCTE-35 markers (no `emsg`).
- nDVR ABR rewind is HLS only. `/{app}/{group}/playlist_dvr*.m3u8` is a
  master over each variant's own recording (`api.abrDVRMaster` in
  `internal/api/abr_dvr.go`). Nothing is recorded under the group name.
  Variants come from the manual group, then the live `%i` group, then
  recorded streams named `{group}{N}` once the encoder is gone. Variant
  URIs follow the live layout, `{srcApp}/{srcStream}/playlist_dvr*.m3u8`
  under the group (no `..`), served by `handleVariantDVRManifest`/
  `handleVariantDVRFile`. The
  window/range/timeshift file name is passed through to each variant, and
  attributes come from the recording (`dvr.Engine.VariantInfo`). There's no
  group `manifest_dvr*.mpd` yet (it would need the variants merged into one
  MPD).
