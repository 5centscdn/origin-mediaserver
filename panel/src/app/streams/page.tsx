'use client'

import { useState } from 'react'
import useSWR from 'swr'
import {
  Radio,
  Copy,
  Check,
  AlertTriangle,
  Loader2,
  ExternalLink,
  RefreshCw,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import { listStreams, kickStream, reloadStream, Stream } from '@/lib/api'
import { formatBytes, formatUptime, copyToClipboard } from '@/lib/utils'

export default function StreamsPage() {
  return (
    <Layout>
      <StreamsContent />
    </Layout>
  )
}

function StreamsContent() {
  const { data, mutate, isLoading } = useSWR('streams', listStreams, {
    refreshInterval: 3000,
  })
  const [kickTarget, setKickTarget] = useState<Stream | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [copied, setCopied] = useState<string | null>(null)
  const [copiedDash, setCopiedDash] = useState<string | null>(null)
  const [copiedTs, setCopiedTs] = useState<string | null>(null)
  const [reloadingId, setReloadingId] = useState<string | null>(null)
  const [reloadedId, setReloadedId] = useState<string | null>(null)
  const [reloadErr, setReloadErr] = useState<string | null>(null)

  const streams = data ?? []

  const doKick = async () => {
    if (!kickTarget) return
    setBusy(true)
    setErr(null)
    try {
      await kickStream(kickTarget.app, kickTarget.name)
      setKickTarget(null)
      mutate()
    } catch (e: any) {
      setErr(e?.message ?? 'Failed to kick stream')
    } finally {
      setBusy(false)
    }
  }

  // Re-applies the stream's application/stream-config (SCTE-35 mode, DASH,
  // HLS overrides, DVR) without touching its ingest connection — safe to
  // click any time after editing an Application, no confirmation needed.
  const doReload = async (s: Stream) => {
    setReloadingId(s.id)
    setReloadErr(null)
    try {
      await reloadStream(s.app, s.name)
      setReloadedId(s.id)
      setTimeout(() => setReloadedId(null), 1500)
      mutate()
    } catch (e: any) {
      setReloadErr(e?.message ?? 'Failed to reload stream')
    } finally {
      setReloadingId(null)
    }
  }

  const copyHLS = async (s: Stream) => {
    const url = `${window.location.origin}/${s.app}/${s.name}/playlist.m3u8`
    if (await copyToClipboard(url)) {
      setCopied(s.id)
      setTimeout(() => setCopied(null), 1500)
    }
  }

  // DASH is opt-in per application (unlike HLS, which defaults on) — this
  // link only resolves if the stream's app has it enabled (Applications →
  // edit → Output Protocols → DASH). No per-stream flag is exposed here to
  // conditionally hide the button, matching how the HLS link/copy button
  // is also always shown regardless of that app's actual HLS setting.
  // No /dash/ path prefix — DASH is mounted at the same unprefixed
  // /{app}/{stream}/ namespace HLS and raw MPEG-TS use.
  const dashURL = (s: Stream) =>
    `${window.location.origin}/${s.app}/${s.name}/manifest.mpd`

  const copyDASH = async (s: Stream) => {
    if (await copyToClipboard(dashURL(s))) {
      setCopiedDash(s.id)
      setTimeout(() => setCopiedDash(null), 1500)
    }
  }

  // Continuous, unsegmented MPEG-TS over one long-lived connection — for a
  // downstream consumer (e.g. a transcoder) that wants to pull raw TS
  // directly rather than poll an HLS/DASH playlist. Always available (no
  // app-level toggle, unlike DASH). Named mpeg.2ts to match the real-world
  // convention other origins (e.g. Nimble) already use for this.
  const tsURL = (s: Stream) => `${window.location.origin}/${s.app}/${s.name}/mpeg.2ts`

  const copyTS = async (s: Stream) => {
    if (await copyToClipboard(tsURL(s))) {
      setCopiedTs(s.id)
      setTimeout(() => setCopiedTs(null), 1500)
    }
  }

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-white">Live Streams</h1>
          <p className="text-sm text-gray-500 mt-1">
            All publishers currently connected to this server
          </p>
        </div>
        <span className="text-xs text-gray-500">
          {streams.length} active · auto-refresh 3s
        </span>
      </div>

      {reloadErr && (
        <div className="flex items-center gap-2 text-sm text-red-400 bg-red-500/10 border border-red-500/30 rounded px-3 py-2">
          <AlertTriangle size={16} className="flex-shrink-0" />
          {reloadErr}
        </div>
      )}

      {isLoading ? (
        <div className="text-gray-500 text-sm">Loading…</div>
      ) : streams.length === 0 ? (
        <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
          <Radio className="mx-auto text-gray-600 mb-3" size={32} />
          <div className="text-gray-400 text-sm font-medium">No active streams</div>
          <div className="mt-3 text-xs text-gray-500">
            Publish via RTMP to{' '}
            <code className="text-gray-300">
              rtmp://&lt;host&gt;:1935/live/&lt;stream-name&gt;
            </code>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-gray-800/50 text-gray-400 text-xs uppercase tracking-wider">
              <tr>
                <th className="px-4 py-2 text-left font-medium">Stream</th>
                <th className="px-4 py-2 text-left font-medium">App</th>
                <th className="px-4 py-2 text-left font-medium">Source</th>
                <th className="px-4 py-2 text-left font-medium">Uptime</th>
                <th className="px-4 py-2 text-left font-medium">Viewers</th>
                <th className="px-4 py-2 text-left font-medium">Bytes In</th>
                <th className="px-4 py-2 text-left font-medium">Codec</th>
                <th className="px-4 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800">
              {streams.map((s) => (
                <tr key={s.id} className="hover:bg-gray-800/30 transition">
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-2">
                      <span className="relative flex h-2 w-2">
                        <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-60" />
                        <span className="relative inline-flex rounded-full h-2 w-2 bg-green-500" />
                      </span>
                      <span className="text-white font-medium">{s.name}</span>
                    </div>
                  </td>
                  <td className="px-4 py-3 text-gray-300">{s.app}</td>
                  <td className="px-4 py-3">
                    <Badge variant="blue">{s.source_type || '—'}</Badge>
                    {s.source_addr && (
                      <div className="text-[10px] text-gray-500 mt-1 truncate max-w-[140px]">
                        {s.source_addr}
                      </div>
                    )}
                  </td>
                  <td className="px-4 py-3 text-gray-300">
                    {formatUptime(s.uptime_s)}
                  </td>
                  <td className="px-4 py-3 text-gray-300">{s.subscribers}</td>
                  <td className="px-4 py-3 text-gray-300">
                    {formatBytes(s.bytes_in)}
                  </td>
                  <td className="px-4 py-3 text-gray-300 font-mono text-xs">
                    {s.codec || '—'}
                  </td>
                  <td className="px-4 py-3 text-right">
                    <div className="inline-flex gap-1.5">
                      <button
                        type="button"
                        onClick={() => copyHLS(s)}
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        {copied === s.id ? (
                          <>
                            <Check size={12} className="text-green-400" />
                            Copied
                          </>
                        ) : (
                          <>
                            <Copy size={12} />
                            HLS
                          </>
                        )}
                      </button>
                      <a
                        href={`/${s.app}/${s.name}/playlist.m3u8`}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        <ExternalLink size={12} />
                      </a>
                      <button
                        type="button"
                        title="Copy DASH manifest URL — only resolves if this stream's application has DASH output enabled"
                        onClick={() => copyDASH(s)}
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        {copiedDash === s.id ? (
                          <>
                            <Check size={12} className="text-green-400" />
                            Copied
                          </>
                        ) : (
                          <>
                            <Copy size={12} />
                            DASH
                          </>
                        )}
                      </button>
                      <a
                        href={`/${s.app}/${s.name}/manifest.mpd`}
                        target="_blank"
                        rel="noreferrer"
                        title="Open DASH manifest — only resolves if this stream's application has DASH output enabled"
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        <ExternalLink size={12} />
                      </a>
                      <button
                        type="button"
                        title="Copy continuous MPEG-TS URL — a live raw TS byte stream for a transcoder or player to pull directly (e.g. ffmpeg -i <url>), not for opening in a browser tab"
                        onClick={() => copyTS(s)}
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        {copiedTs === s.id ? (
                          <>
                            <Check size={12} className="text-green-400" />
                            Copied
                          </>
                        ) : (
                          <>
                            <Copy size={12} />
                            TS
                          </>
                        )}
                      </button>
                      <button
                        type="button"
                        title="Re-apply this stream's application/stream config (SCTE-35, DASH, HLS, DVR) without dropping its ingest connection"
                        disabled={reloadingId === s.id}
                        onClick={() => doReload(s)}
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 disabled:opacity-50 transition"
                      >
                        {reloadingId === s.id ? (
                          <Loader2 size={12} className="animate-spin" />
                        ) : reloadedId === s.id ? (
                          <Check size={12} className="text-green-400" />
                        ) : (
                          <RefreshCw size={12} />
                        )}
                        Reload
                      </button>
                      <button
                        type="button"
                        onClick={() => setKickTarget(s)}
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
                      >
                        Kick
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Modal
        open={!!kickTarget}
        onClose={() => {
          if (!busy) {
            setKickTarget(null)
            setErr(null)
          }
        }}
        title="Kick stream"
      >
        <div className="space-y-3">
          <div className="flex gap-2 text-sm text-gray-300">
            <AlertTriangle className="text-yellow-400 flex-shrink-0" size={18} />
            <div>
              Disconnect publisher for
              <span className="text-white font-medium ml-1">
                {kickTarget?.app}/{kickTarget?.name}
              </span>
              ? Subscribers will be disconnected.
            </div>
          </div>
          {err && (
            <div className="text-sm text-red-400 bg-red-500/10 border border-red-500/30 rounded px-3 py-2">
              {err}
            </div>
          )}
          <div className="flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={() => setKickTarget(null)}
              disabled={busy}
              className="px-3 py-1.5 text-sm rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={doKick}
              disabled={busy}
              className="px-3 py-1.5 text-sm rounded bg-red-600 hover:bg-red-500 disabled:opacity-50 text-white flex items-center gap-2 transition"
            >
              {busy && <Loader2 size={14} className="animate-spin" />}
              Kick
            </button>
          </div>
        </div>
      </Modal>
    </div>
  )
}
