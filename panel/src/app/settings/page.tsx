'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Settings as SettingsIcon, Pencil } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  useStateReset,
} from '@/components/FormHelpers'
import {
  getNodeInfo,
  updateDVRConfig,
  updateLiveCacheConfig,
  LiveCacheSetting,
} from '@/lib/api'
import { listenPort } from '@/lib/utils'

export default function SettingsPage() {
  return (
    <Layout>
      <SettingsContent />
    </Layout>
  )
}

function SettingsContent() {
  const { data: node, mutate } = useSWR('node', getNodeInfo, {
    refreshInterval: 30000,
  })
  const [editDVR, setEditDVR] = useState(false)
  const [editCache, setEditCache] = useState(false)

  const dvr = node?.dvr ?? {}
  const hls = node?.hls ?? {}
  const srt = node?.srt ?? {}

  return (
    <div className="space-y-6 max-w-5xl">
      <div>
        <h1 className="text-2xl font-semibold text-white">Settings</h1>
        <p className="text-sm text-gray-500 mt-1">
          Running server configuration
        </p>
      </div>

      <Section title="Server">
        <Row label="Node name">{node?.node_name ?? '—'}</Row>
        <Row label="Version">
          <code className="text-xs font-mono text-gray-300">
            {node?.version ?? '—'}
          </code>
        </Row>
        <Row label="Data directory">
          <code className="text-xs font-mono text-gray-300 break-all">
            {node?.data_dir ?? '—'}
          </code>
        </Row>
        <Row label="Active streams">{node?.stream_count ?? 0}</Row>
      </Section>

      <Section
        title="Live cache"
        action={
          <button
            type="button"
            onClick={() => setEditCache(true)}
            className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
          >
            <Pencil size={12} />
            Edit
          </button>
        }
      >
        <Row label="Live cache">
          {node?.live_cache ? (
            <span className="flex flex-wrap items-center gap-2">
              <Badge variant="gray">mode: {node.live_cache.policy}</Badge>
              {node.live_cache.mode === 'disk' ? (
                <Badge variant={node.live_cache.policy === 'disk' ? 'gray' : 'yellow'}>
                  {node.live_cache.policy === 'disk' ? 'on disk' : 'on disk (memory high)'}
                </Badge>
              ) : (
                <Badge variant="green">in RAM</Badge>
              )}
              <span>
                {(node.live_cache.bytes / 1048576).toFixed(1)} MB in RAM
                {node.live_cache.disk_files > 0 &&
                  `, ${(node.live_cache.disk_bytes / 1048576).toFixed(1)} MB on disk`}
              </span>
            </span>
          ) : (
            '—'
          )}
        </Row>
        <Row label="Memory used">
          {node?.live_cache && node.live_cache.memory_used_pct >= 0
            ? node.live_cache.policy === 'hybrid'
              ? `${node.live_cache.memory_used_pct}% (disk above ${node.live_cache.memory_high_pct}%, RAM again below ${node.live_cache.memory_low_pct}%)`
              : `${node.live_cache.memory_used_pct}% (warning above ${node.live_cache.memory_high_pct}%)`
            : '—'}
        </Row>
      </Section>

      <Section title="RTMP">
        <Row label="Listen address">
          <code className="text-xs font-mono text-gray-300">
            {node?.rtmp_listen ?? ':1935'}
          </code>
        </Row>
        <Row label="Publish URL">
          <code className="text-xs font-mono text-gray-300">
            rtmp://&lt;host&gt;{listenPort(node?.rtmp_listen, ':1935')}/&lt;app&gt;/&lt;stream&gt;
          </code>
        </Row>
      </Section>

      <Section title="SRT">
        <Row label="Enabled">
          {srt.enabled ? (
            <Badge variant="green">on</Badge>
          ) : (
            <Badge variant="gray">off</Badge>
          )}
        </Row>
        <Row label="Listen address">
          <code className="text-xs font-mono text-gray-300">
            {srt.listen ?? ':4000'}
          </code>
        </Row>
        <Row label="Publish URL">
          <code className="text-xs font-mono text-gray-300">
            srt://&lt;host&gt;{listenPort(srt.listen, ':4000')}?streamid=&lt;app&gt;/&lt;stream&gt;
          </code>
        </Row>
        <Row label="Latency">
          {srt.latency_ms ? `${srt.latency_ms}ms` : '—'}
        </Row>
      </Section>

      <Section title="HTTP">
        <Row label="Listen address">
          <code className="text-xs font-mono text-gray-300">
            {node?.http_listen ?? ':8080'}
          </code>
        </Row>
        <Row label="Playback URL">
          <code className="text-xs font-mono text-gray-300">
            http://&lt;host&gt;{listenPort(node?.http_listen, ':8080')}/&lt;app&gt;/&lt;stream&gt;/playlist.m3u8
          </code>
        </Row>
      </Section>

      <Section title="HLS">
        <Row label="Segment duration">
          {hls.segment_duration ? `${hls.segment_duration}s` : '—'}
        </Row>
        <Row label="Playlist size">
          {hls.playlist_size ? `${hls.playlist_size} segments` : '—'}
        </Row>
      </Section>

      <Section
        title="DVR"
        action={
          <button
            type="button"
            onClick={() => setEditDVR(true)}
            className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
          >
            <Pencil size={12} />
            Edit
          </button>
        }
      >
        <Row label="Enabled">
          {dvr.enabled ? (
            <Badge variant="green">on</Badge>
          ) : (
            <Badge variant="gray">off</Badge>
          )}
        </Row>
        <Row label="Storage path">
          <code className="text-xs font-mono text-gray-300 break-all">
            {dvr.storage_path ?? '—'}
          </code>
        </Row>
        <Row label="Window">
          {dvr.window_seconds
            ? `${dvr.window_seconds}s (${Math.round(dvr.window_seconds / 60)} min)`
            : '—'}
        </Row>
      </Section>

      <LiveCacheModal
        open={editCache}
        current={{
          mode: node?.live_cache?.policy ?? 'hybrid',
          memory_high_pct: node?.live_cache?.memory_high_pct || 90,
          memory_low_pct: node?.live_cache?.memory_low_pct || 80,
        }}
        onClose={() => setEditCache(false)}
        onSaved={() => {
          setEditCache(false)
          mutate()
        }}
      />

      <DVRConfigModal
        open={editDVR}
        current={dvr.window_seconds ?? 3600}
        onClose={() => setEditDVR(false)}
        onSaved={() => {
          setEditDVR(false)
          mutate()
        }}
      />
    </div>
  )
}

function Section({
  title,
  children,
  action,
}: {
  title: string
  children: React.ReactNode
  action?: React.ReactNode
}) {
  return (
    <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-hidden">
      <div className="flex items-center justify-between px-4 py-3 border-b border-gray-800">
        <h2 className="text-sm font-medium text-gray-200 flex items-center gap-2">
          <SettingsIcon size={14} className="text-gray-500" />
          {title}
        </h2>
        {action}
      </div>
      <div className="divide-y divide-gray-800">{children}</div>
    </div>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-3 gap-4 px-4 py-2.5">
      <div className="text-xs text-gray-500 uppercase tracking-wider">{label}</div>
      <div className="col-span-2 text-sm text-gray-300">{children}</div>
    </div>
  )
}

function DVRConfigModal({
  open,
  current,
  onClose,
  onSaved,
}: {
  open: boolean
  current: number
  onClose: () => void
  onSaved: () => void
}) {
  const [windowSeconds, setWindowSeconds] = useState(current)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  // Sync when opened.
  useStateReset(JSON.stringify({ open, current }), () => {
    if (open) {
      setWindowSeconds(current)
      setErr(null)
    }
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      await updateDVRConfig({ window_seconds: windowSeconds })
      onSaved()
    } catch (e: any) {
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={() => !busy && onClose()} title="Edit DVR config">
      <form onSubmit={onSubmit} className="space-y-3">
        <FormField
          label="Window seconds"
          hint="Total retention window — older segments are pruned automatically."
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


const CACHE_MODES: { value: LiveCacheSetting['mode']; label: string; hint: string }[] = [
  {
    value: 'hybrid',
    label: 'Hybrid (recommended)',
    hint: 'RAM, moving to disk while memory is above the high mark, then back to RAM below the low mark.',
  },
  { value: 'memory', label: 'Memory', hint: 'Always RAM. Above the high mark you only get a warning.' },
  { value: 'disk', label: 'Disk', hint: 'Always disk (data directory). Uses the least RAM.' },
]

function LiveCacheModal({
  open,
  current,
  onClose,
  onSaved,
}: {
  open: boolean
  current: LiveCacheSetting
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState<LiveCacheSetting>(current)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(JSON.stringify({ open, current }), () => {
    if (open) {
      setForm(current)
      setErr(null)
    }
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      await updateLiveCacheConfig(form)
      onSaved()
    } catch (e: any) {
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }

  const mode = CACHE_MODES.find((m) => m.value === form.mode)
  return (
    <Modal open={open} onClose={() => !busy && onClose()} title="Edit live cache">
      <form onSubmit={onSubmit} className="space-y-3">
        <FormField label="Mode" hint={mode?.hint}>
          <select
            value={form.mode}
            onChange={(e) => setForm({ ...form, mode: e.target.value as LiveCacheSetting['mode'] })}
            className={inputClass}
          >
            {CACHE_MODES.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
        </FormField>
        {form.mode !== 'disk' && (
          <div className="grid grid-cols-2 gap-3">
            <FormField
              label="High mark (%)"
              hint={form.mode === 'hybrid' ? 'Move to disk at this memory use.' : 'Warn at this memory use.'}
            >
              <input
                type="number"
                min={10}
                max={99}
                required
                value={form.memory_high_pct}
                onChange={(e) => setForm({ ...form, memory_high_pct: Number(e.target.value) })}
                className={inputClass}
              />
            </FormField>
            {form.mode === 'hybrid' && (
              <FormField label="Low mark (%)" hint="Back to RAM below this.">
                <input
                  type="number"
                  min={5}
                  max={form.memory_high_pct - 1}
                  required
                  value={form.memory_low_pct}
                  onChange={(e) => setForm({ ...form, memory_low_pct: Number(e.target.value) })}
                  className={inputClass}
                />
              </FormField>
            )}
          </div>
        )}
        <p className="text-xs text-gray-500">
          Applies immediately, no restart, and is kept across restarts (overrides
          [live_cache] in config.toml). nDVR is always on disk.
        </p>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel="Save" />
      </form>
    </Modal>
  )
}
