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
  viewer counts, H.264/HEVC SPS parsing, SEI), plus `H264Retimer`
  (`retime.go`, `h264poc.go`). The retimer rebuilds DTS, and fixes PTS from
  the bitstream's picture order count, for H.264 that arrives with one
  timestamp per picture.
- Ingest:
  - `rtmp`: server, puller, FMLE `%i` ABR grouping (`abr.go`)
  - `srt`: listener and caller
  - `webrtc`: WHIP publish and WHEP sessions (pion)
  - `icecast`: MP3/ADTS audio only
  - `ingest/mpegts`: UDP, HTTP pull, demux
  - `ingest/rtsp`: TCP-interleaved RTP, H.264 + G.711. SDP lines are put
    in RFC 4566 order before pion/sdp parses them (`normalizeSDP`):
    Hikvision cameras send session `b=` after `t=`/`a=`, which pion
    rejects outright.
  - `pull`: managed outbound pull sources with reconnect (rtmp, rtsp,
    mpegts_http, mpegts_udp, srt), and Nimble-style fallback URLs: on
    failure, or no media for 10 s (an RTMP server with nothing published
    accepts the play and sends nothing), the next URL is tried at once.
    While on a fallback the primary is probed every `primary_check_s` and
    taken back once it has delivered media steadily for 3 s. The probe's
    connection becomes the live one. Each URL's protocol comes from its
    scheme (`pull.ProtocolForURL`).
  - `ingest/mp4file`: an MP4 file played as a looping live source (used by
    failover's `mp4` backups)
- Output:
  - `livestore`: live HLS/DASH output (playlists, MPDs, init segments,
    segments, LL-HLS parts) is kept in RAM, as Nimble does; only DVR is
    on disk. Packagers write and delivery reads by the old on-disk path
    (`<data_dir>/hls|dash/<app>/<stream>/...`), but nothing is written
    there. Segments expire after a per-stream TTL derived from the
    playlist/timeline window. Playlists and init segments stay until the
    packager stops plus a 2 min grace, so a reconnect or pull fallback
    takes them over. `[live_cache] mode`: `hybrid` (default) moves live
    output to disk at `memory_high_pct` (system or cgroup memory, page
    cache excluded) and back to RAM below `memory_low_pct`, so a full
    memory doesn't get the server OOM-killed; `memory` only warns;
    `disk` always writes to disk. Mode, memory use, sizes and any
    warning are in `live_cache` in `GET /api/v1/node`, and the panel
    shows the warning as a banner on every page. Settings → Live cache →
    Edit (`PUT /api/v1/live-cache-config`) changes mode and thresholds
    without a restart and saves them in the `settings` table (schema
    v15), which overrides config.toml at startup.
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
- `failover`: stall watchdog with backup switching and warmup buffer. A
  backup is an RTMP pull, or an MP4 file looped in real time
  (`ingest/mp4file`: pure-Go progressive MP4 reader, H.264 + AAC, local
  path or http(s) URL cached under `data_dir/failover-cache`).
- `api`: chi router. REST under `/api/v1`, JWT auth with roles and per-user
  API tokens, delivery routes, WHIP/WHEP.
- `db`: SQLite schema and access for applications, streams, ABR groups, pull
  sources, failover rules and SCTE-35 events.
- `node`: multi-node peer discovery over NATS heartbeats
  (`GET /api/v1/nodes`). It's discovery only, not central management.
- `metrics`: Prometheus.
- `sysmon`: dashboard monitoring. One goroutine samples `/proc` and statfs
  every 5 s (CPU, memory, disks, host NIC throughput, established
  connections on the HTTP/RTMP ports, listener health from the kernel
  socket tables, ingest bytes, stream and pull events). No process spawning,
  about 1 ms per sample. History is two fixed rings: 5 s points for 1 h and
  1-minute averages for 24 h. Served by `GET /api/v1/monitor` (snapshot) and
  `/monitor/history?range=5m|15m|1h|24h`. Event counters come from
  `media.Registry.EventCounts` and `pull.Manager.Stats`. Linux only;
  elsewhere fields are null and the panel shows them as unavailable.
- `panel/`: Next.js 14 management UI with pages for dashboard, streams,
  applications, ABR, pull, DVR, failover, servers, users, settings and login.
  Design system:
  - Colors are CSS variables in `src/app/globals.css` (RGB channels,
    contrast-checked against the card surface), exposed as semantic Tailwind
    names in `tailwind.config.ts`: `bg-canvas`, `bg-surface`, `bg-elevated`,
    `border-border`, `text-fg`/`-secondary`/`-muted`, `primary`, `success`,
    `warning`, `danger`, `info`. The `gray`/`blue`/`yellow` scales are
    remapped onto the same palette as a fallback. Use the semantic names in
    new code.
  - Shared components live in `src/components/ui/` (Button, IconButton,
    Card, PageHeader, EmptyState, StatusBadge, Table/TH/TD/Truncate,
    CopyButton/CopyField, DropdownMenu, Toggle, toasts via `useToast`,
    skeletons), plus `FormHelpers`, `Modal` and `Badge`.
  - `Layout` is the shell: a grouped sidebar (collapsible on desktop, a
    drawer below `lg`), `ToastProvider`, and the live cache banner. It
    already provides padding and `space-y-6`, so pages return a fragment.
    Fixed-position children need `!mt-0`.
  - Inter and JetBrains Mono come from `next/font`, bundled into the static
    export (no runtime font requests).
- `src/`: legacy empty directory tree, not used.

## Conventions

- Keep decoders narrow and tested against real encoders. Write down the
  scope (what's supported and what's left out) in the package doc comment,
  as `captiondecode`, `teletext` and `ingest/mpegts` do.
- Package and function doc comments explain *why* and link to related code.
  Match that density.
- Media timing uses the stream's PTS domain, not wall-clock time.
- Every `Packet` on a `Bus` must have DTS that increases and is <= PTS: the
  DVR recorder treats a backwards DTS as a discontinuity, and fMP4 needs
  increasing sample times. Ingest that has only one timestamp per picture
  runs H.264 slices through `media.H264Retimer` (D-picture reorder buffer,
  POC order). That's MPEG-TS that never sends DTS (`ingest/mpegts`, used by
  UDP/HTTP/SRT/pull) and RTP (`ingest/rtsp`). A PTS-only PES in a stream
  that has sent DTS means DTS = PTS (muxers drop it when equal), so the
  demuxer's "no DTS" check is sticky per stream, not per packet. Same fix
  as the standalone DVR server's v2.16.6 flicker (B-frames, missing DTS,
  duplicate PTS). Tested against real x264 B-pyramid output in
  `ingest/mpegts/testdata/x264_bframes.ts`.
- A reconnect creates a new `*media.Stream`/`Bus` under the same key, so
  per-stream state must be keyed by pointer, not just by `StreamKey`.
  HLS (TS container) carries the playlist over to the new source: the new
  packager continues the media sequence and segment numbers from the
  previous playlist (`hls.PlaylistRegistry.Previous`, kept 2 min after a
  stream stops) and marks the switch `#EXT-X-DISCONTINUITY`, so players
  following the stream ride through reconnects and pull fallback switches.
  fMP4/CMAF and DASH still restart.
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
- Ingest codecs: H.264/HEVC video and AAC/MP3/MP2 audio. MP3 audio
  (e.g. FMLE) reaches HLS with the `ts` container (stream_type 0x03,
  CODECS `mp4a.40.34`) and MPEG-TS output only; DVR, DASH and fMP4/CMAF
  HLS carry the video without it. G.711 (PCMU/PCMA, RTSP cameras) and
  Opus (WHIP) aren't carried at all: the PMT and the master playlist's
  CODECS leave the audio out (`media.TSAudioStreamless`/`TSAudioCodec`),
  since players wait for an advertised track that never arrives.
- `H264Retimer` covers H.264 only. HEVC that arrives without DTS (and
  WHIP, which also has only RTP timestamps but goes through pion) isn't
  retimed yet. POC types 1 and field-coded pictures fall back to ordering
  by source PTS, which gives a valid DTS but can't fix duplicate PTS.
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
- DASH keeps its audio timeline on the source clock (`dash.Packager`
  `tsOrigin`/`audioPos`: source audio gaps stretch the timeline, and audio
  is cut at the video cut's DTS, not by arrival). HLS fMP4/CMAF still sums
  nominal AAC frame durations (`fmp4AudioBaseDT`), so a source that drops
  audio drifts A/V there over hours.
- Legacy SHOUTcast v1 (no password) Icecast handshakes are not supported.
- Failover backups are `rtmp` or `mp4` only. The panel lists rtsp/srt/hls
  as "not supported yet" (rules with them never fail over). `mp4` backups
  don't support fragmented MP4 or HEVC.
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
