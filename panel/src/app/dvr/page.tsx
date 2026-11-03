'use client'

import { useState, useEffect } from 'react'
import useSWR from 'swr'
import { HardDrive, Pencil, Link2, Play } from 'lucide-react'
import { usePreview } from '@/components/preview/StreamPreview'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  CheckboxField,
} from '@/components/FormHelpers'
import {
  PageHeader,
  Card,
  CardHeader,
  DescriptionList,
  DescriptionRow,
  Button,
  TableCard,
  Table,
  THead,
  TH,
  TBody,
  TR,
  TD,
  RowActions,
  CopyButton,
  EmptyState,
  TableSkeleton,
  StatusBadge,
  StatusDot,
  useToast,
  IconButton,
} from '@/components/ui'
import {
  listStreams,
  getStreamDVR,
  getNodeInfo,
  updateDVRConfig,
  DVRInfo,
  Stream,
} from '@/lib/api'
import { formatBytes, formatRelativeMs, formatDuration } from '@/lib/utils'

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
  const { data: streams, error: streamsErr } = useSWR('streams', listStreams, {
    refreshInterval: 10000,
  })
  const { data: node, mutate: refreshNode } = useSWR('node', getNodeInfo, {
    refreshInterval: 30000,
  })
  const [editConfig, setEditConfig] = useState(false)
  const toast = useToast()
  const preview = usePreview()

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

  // Skeleton only on the very first load; later key changes (a stream
  // joining or leaving) keep showing the table/empty state.
  const [loaded, setLoaded] = useState(false)
  useEffect(() => {
    if (rows) setLoaded(true)
  }, [rows])
  const loading = !loaded && !streamsErr

  const list = rows ?? []

  // nDVR URLs: the whole rewind window as a live playlist/manifest the
  // player can seek back through. Range/timeshift variants are listed in
  // the Rewind URLs card.
  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const rewindURL = (s: Stream, kind: 'hls' | 'dash') =>
    `${origin}/${s.app}/${s.name}/${kind === 'hls' ? 'playlist_dvr.m3u8' : 'manifest_dvr.mpd'}`

  const dvrCfg = node?.dvr ?? {}
  const recordingCount = list.filter((r) => r.dvr && r.dvr.segment_count > 0).length

  return (
    <>
      <PageHeader
        title="DVR"
        description="Time-shifted recording and rewind playback (nDVR) for live streams."
      />

      <div className="grid items-stretch gap-4 lg:grid-cols-2">
      <Card className="h-full">
        <CardHeader
          icon={<HardDrive size={16} />}
          title="DVR configuration"
          description="Global switch records every stream. To record only some, leave it off and enable nDVR per application (Applications page) or per stream config."
          actions={
            <Button size="sm" variant="secondary" icon={<Pencil size={14} />} onClick={() => setEditConfig(true)}>
              Edit
            </Button>
          }
        />
        <DescriptionList>
          <DescriptionRow label="Record all streams">
            {dvrCfg.enabled ? (
              <StatusBadge status="success" label="Enabled" />
            ) : (
              <StatusBadge status="disabled" label="Disabled" />
            )}
          </DescriptionRow>
          <DescriptionRow label="Default rewind window">
            <span className="tabular">
              {dvrCfg.window_seconds
                ? `${dvrCfg.window_seconds}s (${Math.round((dvrCfg.window_seconds ?? 0) / 60)}m)`
                : '—'}
            </span>
          </DescriptionRow>
          <DescriptionRow label="Storage path">
            <code className="block break-all font-mono text-[13px] text-fg-secondary">
              {dvrCfg.storage_path ?? '—'}
            </code>
          </DescriptionRow>
          <DescriptionRow label="Streams with DVR">
            <span className="tabular">{recordingCount}</span>
          </DescriptionRow>
        </DescriptionList>
      </Card>

      <Card className="h-full">
        <CardHeader
          icon={<Link2 size={16} />}
          title="Rewind URLs"
          description="Append these to the server origin. Use the per-stream copy buttons below for the whole-window URLs."
        />
        <DescriptionList>
          <DescriptionRow stacked label="Whole window (live)">
            <UrlTemplates
              items={['/<app>/<stream>/playlist_dvr.m3u8', '/<app>/<stream>/manifest_dvr.mpd']}
            />
          </DescriptionRow>
          <DescriptionRow stacked label="Time range (VOD)">
            <UrlTemplates
              items={[
                '/<app>/<stream>/playlist_dvr_range-<unix_from>-<seconds>.m3u8',
                '/<app>/<stream>/manifest_dvr_range-…mpd',
              ]}
            />
          </DescriptionRow>
          <DescriptionRow stacked label="Delayed live">
            <UrlTemplates
              items={[
                '/<app>/<stream>/playlist_dvr_timeshift-<delay_s>[-<depth_s>].m3u8',
                '/<app>/<stream>/manifest_dvr_timeshift-…mpd',
              ]}
            />
          </DescriptionRow>
        </DescriptionList>
      </Card>
      </div>

      <section className="space-y-3">
        <div className="flex flex-wrap items-end justify-between gap-3">
          <h2 className="text-base font-semibold text-fg">Active recordings</h2>
          <span className="text-xs text-fg-muted">Auto-refresh 10s</span>
        </div>
        {loading ? (
          <TableSkeleton rows={3} cols={7} />
        ) : list.length === 0 ? (
          <TableCard>
            <EmptyState
              icon={<HardDrive size={20} />}
              title="No streams with DVR data"
              description="Recordings show up here while a stream with nDVR enabled (globally, per application or per stream config) is live."
              action={
                <Button size="sm" variant="secondary" icon={<Pencil size={14} />} onClick={() => setEditConfig(true)}>
                  Edit DVR config
                </Button>
              }
            />
          </TableCard>
        ) : (
          <TableCard>
            <Table dense>
              <THead>
                <tr>
                  <TH>Stream</TH>
                  <TH>Status</TH>
                  <TH>Oldest</TH>
                  <TH>Newest</TH>
                  <TH align="right">Window</TH>
                  <TH align="right">Segments</TH>
                  <TH align="right">Size</TH>
                  <TH align="right">
                    <span className="sr-only">Actions</span>
                  </TH>
                </tr>
              </THead>
              <TBody>
                {list.map(({ stream, dvr }) => {
                  const window =
                    dvr && dvr.newest_ms && dvr.oldest_ms ? dvr.newest_ms - dvr.oldest_ms : 0
                  const recording = !!dvr && dvr.segment_count > 0
                  return (
                    <TR key={stream.id}>
                      <TD>
                        <div className="flex items-center gap-2.5">
                          <StatusDot status="live" />
                          <span className="font-medium text-fg">
                            {stream.app}/{stream.name}
                          </span>
                        </div>
                      </TD>
                      <TD>
                        {recording ? (
                          <StatusBadge status="success" label="Recording" />
                        ) : (
                          <StatusBadge status="idle" label="No DVR data" />
                        )}
                      </TD>
                      <TD className="whitespace-nowrap">
                        {dvr?.oldest_ms ? formatRelativeMs(dvr.oldest_ms) : '—'}
                      </TD>
                      <TD className="whitespace-nowrap">
                        {dvr?.newest_ms ? formatRelativeMs(dvr.newest_ms) : '—'}
                      </TD>
                      <TD numeric>
                        {window ? formatDuration(window) : '—'}
                        {dvr?.window_s ? (
                          <span className="text-fg-muted"> / {formatDuration(dvr.window_s * 1000)}</span>
                        ) : null}
                      </TD>
                      <TD numeric>{dvr?.segment_count ?? 0}</TD>
                      <TD numeric>{dvr ? formatBytes(dvr.total_bytes) : '—'}</TD>
                      <TD align="right">
                        <RowActions>
                          <IconButton
                            label={recording ? `Preview recording of ${stream.app}/${stream.name}` : `Preview ${stream.app}/${stream.name} (no DVR data)`}
                            size="xs"
                            variant="secondary"
                            onClick={() =>
                              preview.open({
                                app: stream.app,
                                stream: stream.name,
                                kind: 'dvr',
                                live: recording,
                                codec: stream.codec,
                                context: 'nDVR recording',
                                unavailable: {
                                  title: 'No DVR recording for this stream yet.',
                                  text: 'Enable nDVR for its application or stream config; rewind becomes available once segments are recorded.',
                                },
                              })
                            }
                          >
                            <Play size={13} />
                          </IconButton>
                          <CopyButton
                            text={rewindURL(stream, 'hls')}
                            label="HLS rewind"
                            variant="secondary"
                            title="Copy HLS rewind URL (whole window, live)"
                            toastMessage="HLS rewind URL copied"
                          />
                          <CopyButton
                            text={rewindURL(stream, 'dash')}
                            label="DASH rewind"
                            variant="secondary"
                            title="Copy DASH rewind URL (whole window, live)"
                            toastMessage="DASH rewind URL copied"
                          />
                        </RowActions>
                      </TD>
                    </TR>
                  )
                })}
              </TBody>
            </Table>
          </TableCard>
        )}
      </section>

      <DVRConfigModal
        open={editConfig}
        current={dvrCfg.window_seconds ?? 3600}
        currentEnabled={dvrCfg.enabled ?? false}
        onClose={() => setEditConfig(false)}
        onSaved={() => {
          setEditConfig(false)
          toast.success('DVR config saved')
          refreshNode()
        }}
      />
    </>
  )
}

// UrlTemplates lists the HLS and DASH forms of one rewind URL pattern.
// items: [HLS template, DASH template].
function UrlTemplates({ items }: { items: string[] }) {
  return (
    <div className="space-y-1.5">
      {items.map((t, i) => (
        <div key={t} className="flex items-start gap-2 rounded-md border border-border bg-canvas/60 px-2.5 py-1.5">
          <span className="mt-px w-10 shrink-0 text-xs font-semibold text-fg-muted">{i === 0 ? 'HLS' : 'DASH'}</span>
          <code className="min-w-0 break-all font-mono text-xs leading-5 text-fg-secondary">{t}</code>
        </div>
      ))}
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
      description="Global nDVR settings. Applications and stream configs can override them."
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <CheckboxField
          checked={enabled}
          onChange={setEnabled}
          label="Record all streams"
          description="Applies to streams that start (or are reloaded) after saving"
        />
        <FormField
          label="Window seconds"
          required
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
