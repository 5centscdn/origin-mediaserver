'use client'

import { useState, useEffect } from 'react'
import useSWR from 'swr'
import { HardDrive, Copy, Check, Loader2 } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
} from '@/components/FormHelpers'
import {
  listStreams,
  getStreamDVR,
  getNodeInfo,
  updateDVRConfig,
  DVRInfo,
  Stream,
} from '@/lib/api'
import { formatBytes, formatRelativeMs, formatDuration, copyToClipboard } from '@/lib/utils'

export default function DVRPage() {
  return (
    <Layout>
      <DVRContent />
    </Layout>
  )
}

interface Row {
  stream: Stream
  dvr: DVRInfo | null
}

function DVRContent() {
  const { data: streams } = useSWR('streams', listStreams, {
    refreshInterval: 10000,
  })
  const { data: node, mutate: refreshNode } = useSWR('node', getNodeInfo, {
    refreshInterval: 30000,
  })
  const [editConfig, setEditConfig] = useState(false)
  const [copied, setCopied] = useState<string | null>(null)

  // Fetch DVR info for each stream.
  const { data: rows } = useSWR(
    streams ? ['dvr-rows', streams.map((s) => s.id).join(',')] : null,
    async () => {
      const out: Row[] = []
      for (const s of streams!) {
        try {
          const d = await getStreamDVR(s.app, s.name)
          out.push({ stream: s, dvr: d })
        } catch {
          out.push({ stream: s, dvr: null })
        }
      }
      return out
    },
    { refreshInterval: 10000 },
  )

  const list = rows ?? []

  // nDVR URLs: the whole rewind window as a live playlist/manifest the
  // player can seek back through. Range/timeshift variants are listed in
  // the help box above the table.
  const copyRewind = async (s: Stream, kind: 'hls' | 'dash') => {
    const file = kind === 'hls' ? 'playlist_dvr.m3u8' : 'manifest_dvr.mpd'
    const url = `${window.location.origin}/${s.app}/${s.name}/${file}`
    if (await copyToClipboard(url)) {
      setCopied(`${s.id}:${kind}`)
      setTimeout(() => setCopied(null), 1500)
    }
  }

  const dvrCfg = node?.dvr ?? {}

  return (
    <div className="space-y-6 max-w-7xl">
      <div>
        <h1 className="text-2xl font-semibold text-white">DVR</h1>
        <p className="text-sm text-gray-500 mt-1">
          Time-shifted recording and playback
        </p>
      </div>

      <div className="rounded-lg border border-gray-800 bg-gray-900 p-4">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-medium text-gray-200">DVR Configuration</h2>
          <button
            type="button"
            onClick={() => setEditConfig(true)}
            className="px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
          >
            Edit
          </button>
        </div>
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 text-sm">
          <Info label="Enabled">
            {dvrCfg.enabled ? (
              <Badge variant="green">on</Badge>
            ) : (
              <Badge variant="gray">off</Badge>
            )}
          </Info>
          <Info label="Storage path">
            <span className="text-gray-300 font-mono text-xs truncate block">
              {dvrCfg.storage_path ?? '—'}
            </span>
          </Info>
          <Info label="Window">
            <span className="text-gray-300">
              {dvrCfg.window_seconds
                ? `${dvrCfg.window_seconds}s (${Math.round((dvrCfg.window_seconds ?? 0) / 60)}m)`
                : '—'}
            </span>
          </Info>
          <Info label="Streams with DVR">
            <span className="text-gray-300">
              {list.filter((r) => r.dvr && r.dvr.segment_count > 0).length}
            </span>
          </Info>
        </div>
        <p className="text-xs text-gray-500 mt-3">
          Global switch records every stream. To record only some, leave it off and enable
          nDVR per application (Applications page) or per stream config.
        </p>
      </div>

      <div className="rounded-lg border border-gray-800 bg-gray-900 p-4">
        <h2 className="text-sm font-medium text-gray-200 mb-2">Rewind URLs</h2>
        <div className="space-y-1 text-xs font-mono text-gray-300 break-all">
          <div>
            <span className="text-gray-500">whole window (live): </span>
            /&lt;app&gt;/&lt;stream&gt;/playlist_dvr.m3u8 · manifest_dvr.mpd
          </div>
          <div>
            <span className="text-gray-500">time range (VOD): </span>
            /&lt;app&gt;/&lt;stream&gt;/playlist_dvr_range-&lt;unix_from&gt;-&lt;seconds&gt;.m3u8 ·
            manifest_dvr_range-…mpd
          </div>
          <div>
            <span className="text-gray-500">delayed live: </span>
            /&lt;app&gt;/&lt;stream&gt;/playlist_dvr_timeshift-&lt;delay_s&gt;[-&lt;depth_s&gt;].m3u8 ·
            manifest_dvr_timeshift-…mpd
          </div>
        </div>
      </div>

      <div>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-medium text-gray-200">Active recordings</h2>
          <span className="text-xs text-gray-500">Auto-refresh 10s</span>
        </div>
        {list.length === 0 ? (
          <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
            <HardDrive className="mx-auto text-gray-600 mb-3" size={32} />
            <div className="text-sm text-gray-400">No streams with DVR data</div>
          </div>
        ) : (
          <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="bg-gray-800/50 text-xs uppercase tracking-wider text-gray-500">
                <tr>
                  <th className="px-4 py-2 text-left font-medium">Stream</th>
                  <th className="px-4 py-2 text-left font-medium">Oldest</th>
                  <th className="px-4 py-2 text-left font-medium">Newest</th>
                  <th className="px-4 py-2 text-left font-medium">Window</th>
                  <th className="px-4 py-2 text-left font-medium">Segments</th>
                  <th className="px-4 py-2 text-left font-medium">Size</th>
                  <th className="px-4 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-800">
                {list.map(({ stream, dvr }) => {
                  const window =
                    dvr && dvr.newest_ms && dvr.oldest_ms
                      ? dvr.newest_ms - dvr.oldest_ms
                      : 0
                  return (
                    <tr key={stream.id} className="hover:bg-gray-800/30 transition">
                      <td className="px-4 py-2.5 text-white font-medium">
                        {stream.app}/{stream.name}
                      </td>
                      <td className="px-4 py-2.5 text-gray-300 text-xs">
                        {dvr?.oldest_ms
                          ? formatRelativeMs(dvr.oldest_ms)
                          : '—'}
                      </td>
                      <td className="px-4 py-2.5 text-gray-300 text-xs">
                        {dvr?.newest_ms
                          ? formatRelativeMs(dvr.newest_ms)
                          : '—'}
                      </td>
                      <td className="px-4 py-2.5 text-gray-300">
                        {window ? formatDuration(window) : '—'}
                        {dvr?.window_s ? (
                          <span className="text-gray-500 text-xs"> / {formatDuration(dvr.window_s * 1000)}</span>
                        ) : null}
                      </td>
                      <td className="px-4 py-2.5 text-gray-300">
                        {dvr?.segment_count ?? 0}
                      </td>
                      <td className="px-4 py-2.5 text-gray-300">
                        {dvr ? formatBytes(dvr.total_bytes) : '—'}
                      </td>
                      <td className="px-4 py-2.5 text-right whitespace-nowrap space-x-1">
                        {(['hls', 'dash'] as const).map((kind) => (
                          <button
                            key={kind}
                            type="button"
                            onClick={() => copyRewind(stream, kind)}
                            className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                          >
                            {copied === `${stream.id}:${kind}` ? (
                              <>
                                <Check size={12} className="text-green-400" />
                                Copied
                              </>
                            ) : (
                              <>
                                <Copy size={12} />
                                {kind === 'hls' ? 'HLS' : 'DASH'} rewind
                              </>
                            )}
                          </button>
                        ))}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <DVRConfigModal
        open={editConfig}
        current={dvrCfg.window_seconds ?? 3600}
        currentEnabled={dvrCfg.enabled ?? false}
        onClose={() => setEditConfig(false)}
        onSaved={() => {
          setEditConfig(false)
          refreshNode()
        }}
      />
    </div>
  )
}

function Info({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-[10px] uppercase tracking-wider text-gray-500 mb-1">
        {label}
      </div>
      <div>{children}</div>
    </div>
  )
}

function DVRConfigModal({
  open,
  current,
  currentEnabled,
  onClose,
  onSaved,
}: {
  open: boolean
  current: number
  currentEnabled: boolean
  onClose: () => void
  onSaved: () => void
}) {
  const [windowSeconds, setWindowSeconds] = useState(current)
  const [enabled, setEnabled] = useState(currentEnabled)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useEffect(() => {
    if (open) {
      setWindowSeconds(current)
      setEnabled(currentEnabled)
    }
  }, [open, current, currentEnabled])

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      await updateDVRConfig({ window_seconds: windowSeconds, enabled })
      onSaved()
    } catch (e: any) {
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title="Edit DVR config"
    >
      <form onSubmit={onSubmit} className="space-y-3">
        <label className="flex items-start gap-2 text-sm text-gray-300">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <span>
            Record all streams
            <span className="block text-gray-500 text-xs">
              Applies to streams that start (or are reloaded) after saving
            </span>
          </span>
        </label>
        <FormField
          label="Window seconds"
          hint="Default rewind window. Apps and stream configs can override it. Older data is pruned."
        >
          <input
            type="number"
            min={60}
            required
            value={windowSeconds}
            onChange={(e) => setWindowSeconds(Number(e.target.value))}
            className={inputClass}
          />
        </FormField>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel="Save" />
      </form>
    </Modal>
  )
}
