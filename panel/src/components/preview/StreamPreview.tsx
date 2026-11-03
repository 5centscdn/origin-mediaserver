'use client'

/**
 * Universal stream preview: one modal player used by every page (Live
 * Streams, Streams, ABR Groups, Pull Sources, DVR).
 *
 * - One player per panel session: PreviewProvider (mounted in Layout) holds
 *   a single target; opening another preview replaces it.
 * - Playback URLs are built here from app/stream names, same-origin only,
 *   never taken from callers, so a page can't point the player anywhere
 *   else (and ingest URLs with credentials never get near it). Browsers
 *   can't play RTMP/RTSP/SRT, so every source previews through the
 *   server's own HLS (default) or DASH output; nothing is transcoded.
 * - hls.js and dash.js are imported on first use (code-split chunks), so
 *   pages don't load them. HLS uses hls.js where MSE exists (quality control
 *   and clear errors), native HLS otherwise (iOS Safari).
 * - Closing the modal destroys the player (hls.destroy / dash reset),
 *   detaches the video element and clears timers, so segment fetching stops.
 * - Metadata shown is what the server (CodecInfo summary, ingest bitrate)
 *   or the player (levels, decoded resolution) reports; nothing is guessed.
 */

import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import clsx from 'clsx'
import { AlertTriangle, Loader2, RefreshCw, Radio, WifiOff } from 'lucide-react'
import { Modal } from '../Modal'
import { Button } from '../ui/Button'
import { StatusBadge } from '../ui/Status'
import { inputClass } from '../FormHelpers'
import { formatBitrate } from '@/lib/utils'

export type PreviewKind = 'live' | 'abr' | 'dvr'

export interface PreviewTarget {
  app: string
  stream: string
  /** live (default): the stream's output; abr: an ABR group's master; dvr: the nDVR recording. */
  kind?: PreviewKind
  /** False = known unavailable: show that state and make no playback requests. */
  live?: boolean
  /** Message for the unavailable state (default: the stream is offline). */
  unavailable?: { title: string; text?: string }
  /** Server's codec summary, e.g. "h264/aac". */
  codec?: string
  /** Server-measured ingest bitrate. */
  bitrateBps?: number
  /** Offer DASH (default true; manual ABR groups are HLS only). */
  dash?: boolean
  /** Short context shown in the header, e.g. "ABR group", "Pull source". */
  context?: string
}

type Protocol = 'hls' | 'dash'

interface PreviewApi {
  open: (t: PreviewTarget) => void
  close: () => void
}

const PreviewContext = createContext<PreviewApi>({ open: () => {}, close: () => {} })

export function usePreview(): PreviewApi {
  return useContext(PreviewContext)
}

export function PreviewProvider({ children }: { children: React.ReactNode }) {
  const [target, setTarget] = useState<PreviewTarget | null>(null)
  const api = useMemo<PreviewApi>(() => ({ open: (t) => setTarget(t), close: () => setTarget(null) }), [])
  return (
    <PreviewContext.Provider value={api}>
      {children}
      {target && (
        // key: a new target remounts the player cleanly.
        <PreviewModal key={`${target.kind}:${target.app}/${target.stream}`} target={target} onClose={() => setTarget(null)} />
      )}
    </PreviewContext.Provider>
  )
}

const DVR_MODES = [
  { value: 'window', label: 'Rewind window' },
  { value: '30', label: 'Delayed 30 s' },
  { value: '60', label: 'Delayed 1 min' },
  { value: '300', label: 'Delayed 5 min' },
]

/** Same-origin playback path for a target; segments URL-encoded. */
function playbackPath(t: PreviewTarget, proto: Protocol, dvrMode: string): string {
  const base = `/${encodeURIComponent(t.app)}/${encodeURIComponent(t.stream)}`
  if (t.kind === 'dvr') {
    const suffix = dvrMode === 'window' ? '' : `_timeshift-${Number(dvrMode)}`
    return proto === 'hls' ? `${base}/playlist_dvr${suffix}.m3u8` : `${base}/manifest_dvr${suffix}.mpd`
  }
  return proto === 'hls' ? `${base}/playlist.m3u8` : `${base}/manifest.mpd`
}

/** Resolves a path against this origin and refuses anything else. */
function sameOriginURL(path: string): string | null {
  try {
    const u = new URL(path, window.location.origin)
    return u.origin === window.location.origin ? u.toString() : null
  } catch {
    return null
  }
}

interface Level {
  index: number
  height?: number
  bitrate?: number
}

type Phase = 'loading' | 'playing' | 'error' | 'offline'

function levelLabel(l?: Level) {
  if (!l) return ''
  if (l.height) return `${l.height}p`
  if (l.bitrate) return formatBitrate(l.bitrate)
  return `Level ${l.index + 1}`
}

function PreviewModal({ target, onClose }: { target: PreviewTarget; onClose: () => void }) {
  const dashAllowed = target.dash !== false
  const [proto, setProto] = useState<Protocol>('hls')
  const [dvrMode, setDvrMode] = useState('window')
  const [attempt, setAttempt] = useState(0)

  const videoRef = useRef<HTMLVideoElement>(null)
  const ctl = useRef<{
    setLevel: (i: number) => void
    goLive: () => void
    edgeSlack: () => number
    engine: string
  } | null>(null)

  const [phase, setPhase] = useState<Phase>(target.live === false ? 'offline' : 'loading')
  const [error, setError] = useState<{ msg: string; retry: boolean } | null>(null)
  const [buffering, setBuffering] = useState(false)
  const [levels, setLevels] = useState<Level[]>([])
  const [selected, setSelected] = useState(-1) // -1 = auto
  const [current, setCurrent] = useState(-1)
  const [res, setRes] = useState<string | null>(null)
  const [engine, setEngine] = useState('')
  const [edge, setEdge] = useState<{ start: number; end: number; t: number; live: boolean } | null>(null)

  const src = useMemo(
    () => (typeof window === 'undefined' ? null : sameOriginURL(playbackPath(target, proto, dvrMode))),
    [target, proto, dvrMode],
  )

  // Player lifecycle: (re)created for each source/protocol/retry, always
  // torn down on change or close.
  useEffect(() => {
    if (target.live === false) return
    const video = videoRef.current
    if (!video || !src) {
      if (!src) {
        setPhase('error')
        setError({ msg: 'Refused to load a playback URL outside this server.', retry: false })
      }
      return
    }
    let cancelled = false
    let destroy = () => {}
    let stallTimer: ReturnType<typeof setTimeout> | undefined
    setPhase('loading')
    setError(null)
    setLevels([])
    setSelected(-1)
    setCurrent(-1)
    setRes(null)
    setEdge(null)
    video.muted = true // autoplay policies; the user unmutes from the controls

    const fail = (msg: string, retry = true) => {
      if (cancelled) return
      setPhase('error')
      setError({ msg, retry })
      setBuffering(false)
      destroy() // stop all loading: no retry loop behind the error
      destroy = () => {}
    }

    // Codec check before loading: HEVC plays only where the browser decodes it.
    const codec = (target.codec ?? '').toLowerCase()
    if ((codec.includes('h265') || codec.includes('hevc')) && typeof MediaSource !== 'undefined') {
      if (!MediaSource.isTypeSupported('video/mp4; codecs="hvc1.1.6.L93.B0"') && !video.canPlayType('video/mp4; codecs="hvc1"')) {
        fail('This stream is H.265/HEVC, which your browser cannot decode. Try Safari or Edge with HEVC support.', false)
        return
      }
    }

    const onWaiting = () => {
      setBuffering(true)
      clearTimeout(stallTimer)
      stallTimer = setTimeout(async () => {
        // One check of the playlist tells "the stream ended" from a slow network.
        let gone = false
        try {
          const r = await fetch(src, { cache: 'no-store' })
          gone = r.status === 404 || r.status === 503
        } catch {}
        fail(gone ? 'Stream is currently offline: it stopped publishing.' : 'Buffering timed out: no video data for 20 seconds.')
      }, 20_000)
    }
    const onPlaying = () => {
      setBuffering(false)
      clearTimeout(stallTimer)
      setPhase('playing')
    }
    const onResize = () => video.videoWidth && setRes(`${video.videoWidth}×${video.videoHeight}`)
    const onMediaError = () => {
      const e = video.error
      if (!e || cancelled) return
      fail(
        e.code === MediaError.MEDIA_ERR_SRC_NOT_SUPPORTED || e.code === MediaError.MEDIA_ERR_DECODE
          ? 'Your browser cannot play this stream (unsupported codec or format).'
          : 'Playback was interrupted.',
      )
    }
    video.addEventListener('waiting', onWaiting)
    video.addEventListener('playing', onPlaying)
    video.addEventListener('resize', onResize)
    video.addEventListener('error', onMediaError)
    const removeListeners = () => {
      video.removeEventListener('waiting', onWaiting)
      video.removeEventListener('playing', onPlaying)
      video.removeEventListener('resize', onResize)
      video.removeEventListener('error', onMediaError)
    }

    const play = () => video.play().catch(() => {}) // muted autoplay; ignore policy rejections

    ;(async () => {
      if (proto === 'hls') {
        const { default: Hls } = await import('hls.js')
        if (cancelled) return
        if (!Hls.isSupported()) {
          if (video.canPlayType('application/vnd.apple.mpegurl')) {
            setEngine('HLS (native)')
            video.src = src
            play()
            ctl.current = {
              setLevel: () => {},
              goLive: () => video.seekable.length && (video.currentTime = video.seekable.end(video.seekable.length - 1) - 1),
              edgeSlack: () => 12,
              engine: 'native',
            }
            destroy = () => {}
            return
          }
          fail('This browser cannot play HLS.', false)
          return
        }
        setEngine('HLS (hls.js)')
        const hls = new Hls({
          // Bounded retries, then a fatal error we show (no endless loop).
          manifestLoadingMaxRetry: 2,
          levelLoadingMaxRetry: 2,
          fragLoadingMaxRetry: 3,
          maxBufferLength: 20,
          backBufferLength: 30,
        })
        let mediaRecovered = false
        hls.on(Hls.Events.MANIFEST_PARSED, (_e, d) => {
          setLevels(d.levels.map((l, i) => ({ index: i, height: l.height || undefined, bitrate: l.bitrate || undefined })))
          play()
        })
        hls.on(Hls.Events.LEVEL_SWITCHED, (_e, d) => setCurrent(d.level))
        hls.on(Hls.Events.ERROR, (_e, d) => {
          if (!d.fatal) return
          const code = d.response?.code
          if (d.type === Hls.ErrorTypes.MEDIA_ERROR && !mediaRecovered) {
            mediaRecovered = true
            hls.recoverMediaError()
            return
          }
          if (d.details === Hls.ErrorDetails.MANIFEST_LOAD_ERROR || d.details === Hls.ErrorDetails.LEVEL_LOAD_ERROR) {
            if (code === 404) fail('Stream is currently offline: the HLS playlist was not found.')
            else if (code === 401 || code === 403) fail('Playback authorization failed.', false)
            else if (code === 503) fail('No variants of this stream are live right now.')
            else if (!code) fail('Could not reach the server (network error or CORS restriction).')
            else fail(`The HLS playlist could not be loaded (HTTP ${code}).`)
          } else if (d.details === Hls.ErrorDetails.MANIFEST_PARSING_ERROR) {
            fail('The HLS playlist could not be parsed.')
          } else if (d.details === Hls.ErrorDetails.MANIFEST_LOAD_TIMEOUT || d.details === Hls.ErrorDetails.LEVEL_LOAD_TIMEOUT) {
            fail('The server took too long to answer (playlist request timed out).')
          } else if (d.type === Hls.ErrorTypes.NETWORK_ERROR) {
            fail('Video segments failed to download.')
          } else if (d.type === Hls.ErrorTypes.MEDIA_ERROR) {
            fail('Your browser cannot decode this stream (unsupported codec or corrupt media).', false)
          } else {
            fail('Playback failed.')
          }
        })
        hls.loadSource(src)
        hls.attachMedia(video)
        ctl.current = {
          setLevel: (i) => {
            hls.currentLevel = i // -1 = auto
          },
          goLive: () => {
            if (hls.liveSyncPosition != null) video.currentTime = hls.liveSyncPosition
          },
          edgeSlack: () => (hls.targetLatency ?? 18) + 6,
          engine: 'hls.js',
        }
        destroy = () => {
          hls.stopLoad()
          hls.detachMedia()
          hls.destroy()
        }
      } else {
        const dashjs = await import('dashjs')
        if (cancelled) return
        setEngine('DASH (dash.js)')
        const player = dashjs.MediaPlayer().create()
        player.updateSettings({
          debug: { logLevel: 0 as never }, // LogLevel.LOG_LEVEL_NONE (a const enum, not importable here)
          streaming: {
            retryAttempts: { MPD: 2, MediaSegment: 3, InitializationSegment: 3 },
            abr: { autoSwitchBitrate: { video: true, audio: true } },
          },
        })
        player.on(dashjs.MediaPlayer.events.ERROR, (e: any) => {
          const err = e?.error ?? e
          const msg = String(err?.message ?? '')
          if (err?.code === 25 || /404/.test(msg)) fail('No DASH manifest: the stream is offline or its application has DASH turned off.')
          else if (/40[13]/.test(msg)) fail('Playback authorization failed.', false)
          else if (err?.code === 31 || /parse|parsing/i.test(msg)) fail('The DASH manifest could not be parsed.')
          else if (/codec|capabilit/i.test(msg)) fail('Your browser cannot decode this stream (unsupported codec).', false)
          else fail('DASH playback failed' + (msg ? `: ${msg}` : '.'))
        })
        player.on(dashjs.MediaPlayer.events.STREAM_INITIALIZED, () => {
          const list = player.getBitrateInfoListFor('video') ?? []
          setLevels(list.map((b) => ({ index: b.qualityIndex, height: b.height || undefined, bitrate: b.bitrate || undefined })))
          setCurrent(player.getQualityFor('video'))
        })
        player.on(dashjs.MediaPlayer.events.QUALITY_CHANGE_RENDERED, (e: any) => {
          if (e.mediaType === 'video') setCurrent(e.newQuality)
        })
        player.initialize(video, src, true)
        ctl.current = {
          setLevel: (i) => {
            player.updateSettings({ streaming: { abr: { autoSwitchBitrate: { video: i < 0 } } } })
            if (i >= 0) player.setQualityFor('video', i, true)
          },
          goLive: () => player.seekToOriginalLive(),
          edgeSlack: () => (player.getTargetLiveDelay?.() || 12) + 6,
          engine: 'dash.js',
        }
        destroy = () => player.reset()
      }
    })().catch(() => fail('The video player could not be loaded.'))

    // Live position for the indicator and the DVR seek bar.
    const tick = setInterval(() => {
      const s = video.seekable
      if (!s.length || !ctl.current) return
      const end = s.end(s.length - 1)
      const start = s.start(0)
      setEdge({ start, end, t: video.currentTime, live: end - video.currentTime <= ctl.current.edgeSlack() })
    }, 1000)

    return () => {
      cancelled = true
      clearInterval(tick)
      clearTimeout(stallTimer)
      removeListeners()
      destroy()
      ctl.current = null
      video.pause()
      video.removeAttribute('src')
      video.load() // release the media element's buffers
    }
  }, [src, proto, attempt, target.live, target.codec])

  const chooseLevel = (i: number) => {
    setSelected(i)
    ctl.current?.setLevel(i)
  }

  const [vCodec, aCodec] = (target.codec ?? '').split('/')
  const cur = levels.find((l) => l.index === current)
  const qualityText =
    levels.length === 0
      ? null
      : selected < 0
        ? `Auto${cur && (cur.height || cur.bitrate) ? ` (${levelLabel(cur)})` : ''}`
        : levelLabel(levels.find((l) => l.index === selected))
  const windowLen = edge ? edge.end - edge.start : 0
  const seekable = edge && windowLen > 60 // DVR-sized window: show a seek bar
  const behind = edge ? Math.max(0, edge.end - edge.t) : 0

  const statusBadge =
    target.live === false ? (
      <StatusBadge status="offline" label={target.unavailable ? 'Unavailable' : 'Offline'} />
    ) : phase === 'error' ? (
      <StatusBadge status="error" label="Error" />
    ) : target.kind === 'dvr' ? (
      <StatusBadge status="info" label="DVR" />
    ) : (
      <StatusBadge status="live" label="Live" />
    )

  return (
    <Modal
      open
      onClose={onClose}
      size="video"
      title={`Stream preview: ${target.app} / ${target.stream}`}
      description={
        <span className="inline-flex flex-wrap items-center gap-2">
          {statusBadge}
          {target.context && <span>{target.context}</span>}
        </span>
      }
    >
      <div className="space-y-4">
        {/* Toolbar: protocol, DVR mode, quality, live position. */}
        {target.live !== false && (
          <div className="flex flex-wrap items-center gap-2">
            {dashAllowed && (
              <div role="radiogroup" aria-label="Playback protocol" className="inline-flex rounded-md border border-border bg-canvas/60 p-0.5">
                {(['hls', 'dash'] as Protocol[]).map((p) => (
                  <button
                    key={p}
                    type="button"
                    role="radio"
                    aria-checked={proto === p}
                    onClick={() => setProto(p)}
                    className={clsx(
                      'h-7 rounded px-3 text-xs font-semibold transition-colors',
                      proto === p ? 'bg-elevated text-fg shadow-sm' : 'text-fg-muted hover:text-fg',
                    )}
                  >
                    {p.toUpperCase()}
                  </button>
                ))}
              </div>
            )}
            {target.kind === 'dvr' && (
              <select
                aria-label="DVR mode"
                value={dvrMode}
                onChange={(e) => setDvrMode(e.target.value)}
                className={clsx(inputClass, '!h-8 !w-auto py-0 text-[13px]')}
              >
                {DVR_MODES.map((m) => (
                  <option key={m.value} value={m.value}>
                    {m.label}
                  </option>
                ))}
              </select>
            )}
            {levels.length > 1 && (
              <select
                aria-label="Quality"
                value={selected}
                onChange={(e) => chooseLevel(Number(e.target.value))}
                className={clsx(inputClass, '!h-8 !w-auto py-0 text-[13px]')}
              >
                <option value={-1}>Auto{cur && selected < 0 ? ` (${levelLabel(cur)})` : ''}</option>
                {[...levels]
                  .sort((a, b) => (b.height ?? 0) - (a.height ?? 0) || (b.bitrate ?? 0) - (a.bitrate ?? 0))
                  .map((l) => (
                    <option key={l.index} value={l.index}>
                      {levelLabel(l)}
                      {l.height && l.bitrate ? ` · ${formatBitrate(l.bitrate)}` : ''}
                    </option>
                  ))}
              </select>
            )}
            <div className="ml-auto flex items-center gap-2">
              {edge && phase === 'playing' && target.kind === 'dvr' && dvrMode !== 'window' ? (
                // Timeshift playlists are a live window shifted back: "live"
                // relative to that window would be misleading.
                <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-info">
                  <Radio size={13} /> DELAYED {DVR_MODES.find((m) => m.value === dvrMode)?.label.replace('Delayed ', '')}
                </span>
              ) : edge && phase === 'playing' && (
                edge.live ? (
                  <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-danger">
                    <Radio size={13} /> LIVE
                  </span>
                ) : (
                  <>
                    <span className="tabular text-xs text-fg-muted">{Math.round(behind)} s behind live</span>
                    <Button size="xs" variant="secondary" onClick={() => ctl.current?.goLive()}>
                      Go live
                    </Button>
                  </>
                )
              )}
            </div>
          </div>
        )}

        {/* Video */}
        <div className="relative aspect-video w-full overflow-hidden rounded-lg bg-black">
          {/* eslint-disable-next-line jsx-a11y/media-has-caption */}
          <video ref={videoRef} className="h-full w-full" controls playsInline muted aria-label={`Preview of ${target.app}/${target.stream}`} />
          {target.live === false && (
            <Overlay
              icon={<WifiOff size={22} />}
              title={target.unavailable?.title ?? 'Stream is currently offline.'}
              text={target.unavailable ? target.unavailable.text : 'Nothing is publishing to this stream, so there is nothing to preview.'}
            />
          )}
          {target.live !== false && phase === 'loading' && !error && (
            <Overlay icon={<Loader2 size={22} className="animate-spin" />} title="Loading stream…" />
          )}
          {phase === 'playing' && buffering && (
            <div className="pointer-events-none absolute left-3 top-3 inline-flex items-center gap-1.5 rounded-md bg-black/70 px-2 py-1 text-xs text-white">
              <Loader2 size={12} className="animate-spin" /> Buffering…
            </div>
          )}
          {error && (
            <Overlay
              icon={<AlertTriangle size={22} className="text-warning" />}
              title={error.msg}
              action={
                error.retry ? (
                  <Button size="sm" variant="secondary" icon={<RefreshCw size={14} />} onClick={() => setAttempt((a) => a + 1)}>
                    Retry
                  </Button>
                ) : undefined
              }
            />
          )}
        </div>

        {/* DVR seek bar (the native control has none for live media). */}
        {seekable && edge && phase !== 'error' && (
          <div className="flex items-center gap-3">
            <span className="tabular w-16 text-xs text-fg-muted">−{fmtDur(edge.end - edge.start)}</span>
            <input
              type="range"
              aria-label="Seek within the recording window"
              min={edge.start}
              max={edge.end}
              step={1}
              value={edge.t}
              onChange={(e) => {
                const v = Number(e.target.value)
                if (videoRef.current) videoRef.current.currentTime = v
                setEdge({ ...edge, t: v, live: edge.end - v <= (ctl.current?.edgeSlack() ?? 12) })
              }}
              className="h-1.5 flex-1 cursor-pointer accent-[rgb(var(--primary-fg))]"
            />
            <span className="tabular w-16 text-right text-xs text-fg-muted">{behind > 1 ? `−${fmtDur(behind)}` : 'live'}</span>
          </div>
        )}

        {/* Technical information: only what the server or player reported. */}
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 rounded-lg border border-border bg-canvas/40 p-4 text-[13px] sm:grid-cols-4">
          <Info label="Application" value={target.app} mono />
          <Info label="Stream" value={target.stream} mono />
          <Info label="Playback" value={engine || (target.live === false ? null : proto.toUpperCase())} />
          <Info label="Current quality" value={qualityText} />
          <Info label="Video codec" value={vCodec || null} mono />
          <Info label="Audio codec" value={aCodec || null} mono />
          <Info label="Resolution" value={res} />
          <Info
            label={cur?.bitrate ? 'Rendition bitrate' : 'Ingest bitrate'}
            value={cur?.bitrate ? formatBitrate(cur.bitrate) : target.bitrateBps ? formatBitrate(target.bitrateBps) : null}
          />
        </dl>
      </div>
    </Modal>
  )
}

function fmtDur(s: number) {
  s = Math.max(0, Math.round(s))
  const m = Math.floor(s / 60)
  return `${m}:${String(s % 60).padStart(2, '0')}`
}

function Info({ label, value, mono }: { label: string; value: React.ReactNode | null; mono?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-fg-muted">{label}</dt>
      <dd className={clsx('mt-0.5 truncate text-fg', mono && 'font-mono text-xs leading-5')}>{value ?? <span className="text-fg-muted">—</span>}</dd>
    </div>
  )
}

function Overlay({
  icon,
  title,
  text,
  action,
}: {
  icon: React.ReactNode
  title: string
  text?: string
  action?: React.ReactNode
}) {
  return (
    <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-black/80 p-6 text-center" role="status">
      <div className="text-white/80">{icon}</div>
      <div className="max-w-md text-sm font-medium text-white">{title}</div>
      {text && <div className="max-w-md text-xs text-white/70">{text}</div>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}
