'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Pencil, Server as ServerIcon, MemoryStick, Radio, Zap, Globe, ListVideo, History } from 'lucide-react'
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
  PageHeader,
  Card,
  CardHeader,
  DescriptionList,
  DescriptionRow,
  Button,
  CopyButton,
  StatusBadge,
  Skeleton,
  useToast,
} from '@/components/ui'
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

// Mono value (address, path, URL template), optionally with a copy button.
function Mono({ value, copy, breakAll }: { value: string; copy?: boolean; breakAll?: boolean }) {
  return (
    <span className="flex min-w-0 items-center gap-1">
      <code className={'min-w-0 font-mono text-[13px] text-fg-secondary ' + (breakAll ? 'break-all' : 'truncate')} title={value}>
        {value}
      </code>
      {copy && <CopyButton text={value} title="Copy" toastMessage="Copied to clipboard" />}
    </span>
  )
}

function OnOff({ on }: { on?: boolean }) {
  return on ? <StatusBadge status="success" label="Enabled" /> : <StatusBadge status="disabled" label="Disabled" />
}

function EditButton({ onClick, what }: { onClick: () => void; what: string }) {
  return (
    <Button size="sm" variant="secondary" icon={<Pencil size={13} />} onClick={onClick} aria-label={`Edit ${what}`}>
      Edit
    </Button>
  )
}

function SettingsSkeleton() {
  return (
    <div className="grid grid-cols-1 items-start gap-6 xl:grid-cols-2" aria-busy="true" aria-label="Loading">
      {Array.from({ length: 4 }).map((_, i) => (
        <Card key={i}>
          <div className="border-b border-border px-5 py-4">
            <Skeleton className="h-4 w-32" />
            <Skeleton className="mt-2 h-3 w-56" />
          </div>
          <div className="space-y-4 px-5 py-4">
            <Skeleton className="h-3.5 w-3/4" />
            <Skeleton className="h-3.5 w-2/3" />
            <Skeleton className="h-3.5 w-1/2" />
          </div>
        </Card>
      ))}
    </div>
  )
}

function SettingsContent() {
  const { data: node, mutate, isLoading } = useSWR('node', getNodeInfo, {
    refreshInterval: 30000,
  })
  const toast = useToast()
  const [editDVR, setEditDVR] = useState(false)
  const [editCache, setEditCache] = useState(false)

  const dvr = node?.dvr ?? {}
  const hls = node?.hls ?? {}
  const srt = node?.srt ?? {}

  const rtmpURL = `rtmp://<host>${listenPort(node?.rtmp_listen, ':1935')}/<app>/<stream>`
  const srtURL = `srt://<host>${listenPort(srt.listen, ':4000')}?streamid=<app>/<stream>`
  const httpURL = `http://<host>${listenPort(node?.http_listen, ':8080')}/<app>/<stream>/playlist.m3u8`

  return (
    <>
      <PageHeader
        title="Settings"
        description="Running server configuration. Live cache and DVR can be changed here and apply without a restart; the rest comes from config.toml."
      />

      {isLoading && !node ? (
        <SettingsSkeleton />
      ) : (
        <div className="grid grid-cols-1 items-start gap-6 xl:grid-cols-2">
          <Card>
            <CardHeader
              icon={<ServerIcon size={18} />}
              title="Server information"
              description="Identity and storage of this node."
            />
            <DescriptionList>
              <DescriptionRow label="Node name">{node?.node_name ?? '—'}</DescriptionRow>
              <DescriptionRow label="Version">
                <Mono value={node?.version ?? '—'} />
              </DescriptionRow>
              <DescriptionRow label="Data directory">
                <Mono value={node?.data_dir ?? '—'} copy={!!node?.data_dir} breakAll />
              </DescriptionRow>
              <DescriptionRow label="Active streams">
                <span className="tabular">{node?.stream_count ?? 0}</span>
              </DescriptionRow>
            </DescriptionList>
          </Card>

          <Card>
            <CardHeader
              icon={<MemoryStick size={18} />}
              title="Live cache"
              description="Where live HLS/DASH segments are kept."
              actions={<EditButton what="live cache" onClick={() => setEditCache(true)} />}
            />
            <DescriptionList>
              <DescriptionRow label="Live cache">
                {node?.live_cache ? (
                  <span className="flex flex-wrap items-center gap-2">
                    <Badge variant="gray" mono>
                      mode: {node.live_cache.policy}
                    </Badge>
                    {node.live_cache.mode === 'disk' ? (
                      <Badge variant={node.live_cache.policy === 'disk' ? 'gray' : 'yellow'}>
                        {node.live_cache.policy === 'disk' ? 'on disk' : 'on disk (memory high)'}
                      </Badge>
                    ) : (
                      <Badge variant="green">in RAM</Badge>
                    )}
                    <span className="tabular text-[13px] text-fg-secondary">
                      {(node.live_cache.bytes / 1048576).toFixed(1)} MB in RAM
                      {node.live_cache.disk_files > 0 &&
                        `, ${(node.live_cache.disk_bytes / 1048576).toFixed(1)} MB on disk`}
                    </span>
                  </span>
                ) : (
                  '—'
                )}
              </DescriptionRow>
              <DescriptionRow label="Memory used">
                <span className="tabular">
                  {node?.live_cache && node.live_cache.memory_used_pct >= 0
                    ? node.live_cache.policy === 'hybrid'
                      ? `${node.live_cache.memory_used_pct}% (disk above ${node.live_cache.memory_high_pct}%, RAM again below ${node.live_cache.memory_low_pct}%)`
                      : `${node.live_cache.memory_used_pct}% (warning above ${node.live_cache.memory_high_pct}%)`
                    : '—'}
                </span>
              </DescriptionRow>
              {node?.live_cache?.warning && (
                <DescriptionRow label="Warning">
                  <span className="text-warning">{node.live_cache.warning}</span>
                </DescriptionRow>
              )}
            </DescriptionList>
          </Card>

          <Card>
            <CardHeader icon={<Radio size={18} />} title="RTMP" description="RTMP/RTMPS ingest listener." />
            <DescriptionList>
              <DescriptionRow label="Listen address">
                <Mono value={node?.rtmp_listen ?? ':1935'} />
              </DescriptionRow>
              <DescriptionRow label="Publish URL">
                <Mono value={rtmpURL} copy />
              </DescriptionRow>
            </DescriptionList>
          </Card>

          <Card>
            <CardHeader icon={<Zap size={18} />} title="SRT" description="SRT listener for low-latency contribution." />
            <DescriptionList>
              <DescriptionRow label="Enabled">
                <OnOff on={srt.enabled} />
              </DescriptionRow>
              <DescriptionRow label="Listen address">
                <Mono value={srt.listen ?? ':4000'} />
              </DescriptionRow>
              <DescriptionRow label="Publish URL">
                <Mono value={srtURL} copy />
              </DescriptionRow>
              <DescriptionRow label="Latency">
                <span className="tabular">{srt.latency_ms ? `${srt.latency_ms}ms` : '—'}</span>
              </DescriptionRow>
            </DescriptionList>
          </Card>

          <Card>
            <CardHeader
              icon={<Globe size={18} />}
              title="HTTP"
              description="One port for HLS, DASH, WHIP/WHEP, the API and this panel."
            />
            <DescriptionList>
              <DescriptionRow label="Listen address">
                <Mono value={node?.http_listen ?? ':8080'} />
              </DescriptionRow>
              <DescriptionRow label="Playback URL">
                <Mono value={httpURL} copy />
              </DescriptionRow>
            </DescriptionList>
          </Card>

          <Card>
            <CardHeader icon={<ListVideo size={18} />} title="HLS" description="Default segmenting for live playlists." />
            <DescriptionList>
              <DescriptionRow label="Segment duration">
                <span className="tabular">{hls.segment_duration ? `${hls.segment_duration}s` : '—'}</span>
              </DescriptionRow>
              <DescriptionRow label="Playlist size">
                <span className="tabular">{hls.playlist_size ? `${hls.playlist_size} segments` : '—'}</span>
              </DescriptionRow>
            </DescriptionList>
          </Card>

          <Card>
            <CardHeader
              icon={<History size={18} />}
              title="DVR"
              description="nDVR recording and rewind window."
              actions={<EditButton what="DVR" onClick={() => setEditDVR(true)} />}
            />
            <DescriptionList>
              <DescriptionRow label="Enabled">
                <OnOff on={dvr.enabled} />
              </DescriptionRow>
              <DescriptionRow label="Storage path">
                <Mono value={dvr.storage_path ?? '—'} copy={!!dvr.storage_path} breakAll />
              </DescriptionRow>
              <DescriptionRow label="Window">
                <span className="tabular">
                  {dvr.window_seconds
                    ? `${dvr.window_seconds}s (${Math.round(dvr.window_seconds / 60)} min)`
                    : '—'}
                </span>
              </DescriptionRow>
            </DescriptionList>
          </Card>
        </div>
      )}

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
          toast.success('Live cache settings saved')
          mutate()
        }}
      />

      <DVRConfigModal
        open={editDVR}
        current={dvr.window_seconds ?? 3600}
        onClose={() => setEditDVR(false)}
        onSaved={() => {
          setEditDVR(false)
          toast.success('DVR settings saved')
          mutate()
        }}
      />
    </>
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
    <Modal open={open} onClose={() => !busy && onClose()} title="Edit DVR config" description="Rewind window for nDVR recordings.">
      <form onSubmit={onSubmit} className="space-y-4">
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
    <Modal open={open} onClose={() => !busy && onClose()} title="Edit live cache" description="Where live segments are kept: RAM, disk, or both.">
      <form onSubmit={onSubmit} className="space-y-4">
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
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
        <p className="text-xs leading-relaxed text-fg-muted">
          Applies immediately, no restart, and is kept across restarts (overrides
          [live_cache] in config.toml). nDVR is always on disk.
        </p>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel="Save" />
      </form>
    </Modal>
  )
}
