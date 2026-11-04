'use client'

import { useEffect, useMemo, useState } from 'react'
import useSWR from 'swr'
import { Radio, ExternalLink, RefreshCw, Copy, Power, Check, Play } from 'lucide-react'
import Link from 'next/link'
import { usePreview } from '@/components/preview/StreamPreview'
import { Layout } from '@/components/Layout'
import { Badge } from '@/components/Badge'
import { ConfirmDelete } from '@/components/FormHelpers'
import {
  PageHeader,
  TableCard,
  Table,
  THead,
  TH,
  TBody,
  TR,
  TD,
  RowActions,
  Truncate,
  CopyButton,
  DropdownMenu,
  IconButton,
  EmptyState,
  TableSkeleton,
  StatusDot,
  SearchInput,
  useToast,
} from '@/components/ui'
import { listStreams, kickStream, reloadStream, getNodeInfo, Stream } from '@/lib/api'
import { formatBytes, formatUptime, formatBitrate, copyToClipboard, redactUrl, listenPort } from '@/lib/utils'

type SortKey = 'name' | 'app' | 'source' | 'uptime' | 'viewers' | 'bitrate' | 'bytes' | 'codec'
type SortState = { key: SortKey; dir: 'asc' | 'desc' }

const SORT_STORAGE = 'liveStreams.sort'
const DEFAULT_SORT: SortState = { key: 'app', dir: 'asc' }

// sortValue is what each sortable column compares on.
function sortValue(s: Stream, key: SortKey): string | number {
  switch (key) {
    case 'name':
      return s.name.toLowerCase()
    case 'app':
      return s.app.toLowerCase()
    case 'source':
      return (s.source_type || '').toLowerCase()
    case 'uptime':
      return s.uptime_s
    case 'viewers':
      return s.subscribers
    case 'bitrate':
      return s.bitrate_bps ?? 0
    case 'bytes':
      return s.bytes_in
    case 'codec':
      return (s.codec || '').toLowerCase()
  }
}

// sortStreams orders rows by the chosen column, then app/name so equal
// values (e.g. 0 viewers) keep a fixed order across refreshes instead of
// jumping around.
function sortStreams(list: Stream[], sort: SortState): Stream[] {
  const mul = sort.dir === 'asc' ? 1 : -1
  return [...list].sort((a, b) => {
    const va = sortValue(a, sort.key)
    const vb = sortValue(b, sort.key)
    if (va < vb) return -1 * mul
    if (va > vb) return 1 * mul
    return a.app.localeCompare(b.app) || a.name.localeCompare(b.name)
  })
}

// Source type → badge color: push ingest vs pulled sources.
function sourceVariant(t: string): 'blue' | 'purple' | 'gray' {
  if (t.endsWith('_pull') || t === 'pull') return 'purple'
  return t ? 'blue' : 'gray'
}

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
  // Same SWR key as the sidebar, so no extra polling: ports for the
  // RTMP/SRT playback URLs.
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 15000 })
  const toast = useToast()
  const preview = usePreview()
  const [kickTarget, setKickTarget] = useState<Stream | null>(null)
  const [reloadingId, setReloadingId] = useState<string | null>(null)
  const [reloadedId, setReloadedId] = useState<string | null>(null)
  const [query, setQuery] = useState('')

  const [sort, setSort] = useState<SortState>(DEFAULT_SORT)
  // Remember the chosen column across visits (per browser).
  useEffect(() => {
    try {
      const saved = JSON.parse(localStorage.getItem(SORT_STORAGE) || 'null')
      if (saved?.key && saved?.dir) setSort(saved)
    } catch {}
  }, [])
  const onSort = (key: SortKey) => {
    // Numbers start high-to-low (busiest first), text A-Z; a second click flips.
    const numeric = ['uptime', 'viewers', 'bitrate', 'bytes'].includes(key)
    const next: SortState =
      sort.key === key
        ? { key, dir: sort.dir === 'asc' ? 'desc' : 'asc' }
        : { key, dir: numeric ? 'desc' : 'asc' }
    setSort(next)
    try {
      localStorage.setItem(SORT_STORAGE, JSON.stringify(next))
    } catch {}
  }
  const sortProps = (key: SortKey) => ({
    sort: sort.key === key ? sort.dir : (false as const),
    onSort: () => onSort(key),
  })

  const all = data ?? []
  const streams = useMemo(() => {
    const q = query.trim().toLowerCase()
    const filtered = q
      ? all.filter((s) =>
          [s.name, s.app, s.source_type, s.source_addr, s.codec].some((f) => (f || '').toLowerCase().includes(q)),
        )
      : all
    return sortStreams(filtered, sort)
  }, [all, sort, query])

  const doKick = async () => {
    if (!kickTarget) return
    await kickStream(kickTarget.app, kickTarget.name) // ConfirmDelete shows a thrown error
    toast.success(`Disconnected ${kickTarget.app}/${kickTarget.name}`)
    setKickTarget(null)
    mutate()
  }

  // Re-applies the stream's application/stream-config (SCTE-35 mode, DASH,
  // HLS overrides, DVR) without touching its ingest connection — safe to
  // click any time after editing an Application, no confirmation needed.
  const doReload = async (s: Stream) => {
    setReloadingId(s.id)
    try {
      await reloadStream(s.app, s.name)
      setReloadedId(s.id)
      setTimeout(() => setReloadedId(null), 1500)
      toast.success(`Reloaded config for ${s.app}/${s.name}`)
      mutate()
    } catch (e: any) {
      toast.error(e?.message ?? 'Failed to reload stream')
    } finally {
      setReloadingId(null)
    }
  }

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const hlsURL = (s: Stream) => `${origin}/${s.app}/${s.name}/playlist.m3u8`
  // DASH is opt-in per application (unlike HLS, which defaults on) — this
  // link only resolves if the stream's app has it enabled (Applications →
  // edit → Output Protocols → DASH). Mounted at the same unprefixed
  // /{app}/{stream}/ namespace HLS and raw MPEG-TS use.
  const dashURL = (s: Stream) => `${origin}/${s.app}/${s.name}/manifest.mpd`
  // Continuous, unsegmented MPEG-TS over one long-lived connection — for a
  // downstream consumer (e.g. a transcoder) that wants to pull raw TS
  // directly rather than poll an HLS/DASH playlist. Named mpeg.2ts to match
  // the real-world convention other origins (e.g. Nimble) already use.
  const tsURL = (s: Stream) => `${origin}/${s.app}/${s.name}/mpeg.2ts`
  // RTMP and SRT playback (any live stream, whatever it was ingested over).
  // SRT uses request mode in the standard Access Control streamid form.
  const host = typeof window !== 'undefined' ? window.location.hostname : ''
  const rtmpURL = (s: Stream) => `rtmp://${host}${listenPort(node?.rtmp_listen, ':1935')}/${s.app}/${s.name}`
  const srtURL = (s: Stream) =>
    `srt://${host}${listenPort(node?.srt?.listen, ':4000')}?streamid=#!::r=${s.app}/${s.name},m=request`

  const copyFromMenu = async (url: string, what: string) => {
    if (await copyToClipboard(url)) toast.success(`${what} URL copied`)
    else toast.error('Copy failed')
  }

  return (
    <>
      <PageHeader
        title="Live Streams"
        description="Publishers and pull sources currently connected to this server."
        meta={
          <span className="inline-flex items-center gap-2">
            <StatusDot status={all.length ? 'live' : 'idle'} />
            {all.length} active · auto-refresh 3s
          </span>
        }
      />

      {isLoading ? (
        <TableSkeleton rows={3} cols={7} />
      ) : all.length === 0 ? (
        <TableCard>
          <EmptyState
            icon={<Radio size={20} />}
            title="No live streams"
            description={
              <>
                Streams appear here as soon as a publisher connects or a pull source starts. Publish via RTMP to{' '}
                <code className="font-mono text-fg-secondary">rtmp://&lt;host&gt;:1935/&lt;app&gt;/&lt;stream&gt;</code>.
              </>
            }
          />
        </TableCard>
      ) : (
        <TableCard
          header={
            <>
              <SearchInput value={query} onChange={setQuery} placeholder="Filter streams…" />
              {query && (
                <span className="text-xs text-fg-muted">
                  {streams.length} of {all.length}
                </span>
              )}
            </>
          }
        >
          <Table dense>
            <THead>
              <tr>
                <TH {...sortProps('app')}>Application</TH>
                <TH {...sortProps('name')}>Stream</TH>
                <TH {...sortProps('source')}>Source</TH>
                <TH {...sortProps('codec')}>Codec</TH>
                <TH align="right" {...sortProps('uptime')}>Uptime</TH>
                <TH align="right" {...sortProps('viewers')}>Viewers</TH>
                <TH align="right" {...sortProps('bitrate')}>Bitrate</TH>
                <TH align="right" {...sortProps('bytes')}>Bytes in</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {streams.length === 0 && (
                <tr>
                  <TD colSpan={9} muted className="py-8 text-center">
                    No streams match “{query}”.
                  </TD>
                </tr>
              )}
              {streams.map((s) => (
                <TR key={s.id}>
                  <TD>
                    <Link href="/applications" className="text-fg-secondary hover:text-primary-fg hover:underline">
                      {s.app}
                    </Link>
                  </TD>
                  <TD>
                    <div className="flex items-center gap-2.5">
                      <StatusDot status="live" />
                      <span className="font-medium text-fg">{s.name}</span>
                      <span className="sr-only">live</span>
                    </div>
                  </TD>
                  <TD>
                    <div className="flex flex-col items-start gap-1">
                      <Badge variant={sourceVariant(s.source_type || '')} mono>
                        {s.source_type || 'unknown'}
                      </Badge>
                      {s.source_addr && (
                        <Truncate mono max="max-w-[170px]" className="text-xs text-fg-muted">
                          {redactUrl(s.source_addr)}
                        </Truncate>
                      )}
                    </div>
                  </TD>
                  <TD mono>{s.codec || '—'}</TD>
                  <TD numeric>{formatUptime(s.uptime_s)}</TD>
                  <TD numeric className={s.subscribers > 0 ? 'text-fg' : undefined}>
                    {s.subscribers}
                  </TD>
                  <TD numeric>{s.bitrate_bps ? formatBitrate(s.bitrate_bps) : '—'}</TD>
                  <TD numeric>{formatBytes(s.bytes_in)}</TD>
                  <TD align="right">
                    <RowActions>
                      <IconButton
                        label={`Preview ${s.app}/${s.name}`}
                        size="xs"
                        variant="secondary"
                        onClick={() =>
                          preview.open({ app: s.app, stream: s.name, codec: s.codec, bitrateBps: s.bitrate_bps, context: s.source_type })
                        }
                      >
                        <Play size={13} />
                      </IconButton>
                      <CopyButton text={hlsURL(s)} label="HLS" variant="secondary" toastMessage="HLS URL copied" />
                      <CopyButton
                        text={dashURL(s)}
                        label="DASH"
                        variant="secondary"
                        title="Copy DASH manifest URL — only resolves if this stream's application has DASH output enabled"
                        toastMessage="DASH URL copied"
                      />
                      <IconButton
                        label="Reload config (SCTE-35, DASH, HLS, DVR) without dropping the ingest connection"
                        size="xs"
                        loading={reloadingId === s.id}
                        onClick={() => doReload(s)}
                      >
                        {reloadedId === s.id ? <Check size={14} className="text-success" /> : <RefreshCw size={14} />}
                      </IconButton>
                      <DropdownMenu
                        label={`More actions for ${s.app}/${s.name}`}
                        items={[
                          { label: 'Open HLS playlist', icon: <ExternalLink size={14} />, href: `/${s.app}/${s.name}/playlist.m3u8`, external: true },
                          { label: 'Open DASH manifest', icon: <ExternalLink size={14} />, href: `/${s.app}/${s.name}/manifest.mpd`, external: true },
                          { label: 'Copy MPEG-TS URL', icon: <Copy size={14} />, onClick: () => copyFromMenu(tsURL(s), 'MPEG-TS') },
                          { label: 'Copy RTMP URL', icon: <Copy size={14} />, onClick: () => copyFromMenu(rtmpURL(s), 'RTMP') },
                          ...(node?.srt?.enabled
                            ? [{ label: 'Copy SRT URL', icon: <Copy size={14} />, onClick: () => copyFromMenu(srtURL(s), 'SRT') }]
                            : []),
                          { label: 'Kick publisher', icon: <Power size={14} />, danger: true, separator: true, onClick: () => setKickTarget(s) },
                        ]}
                      />
                    </RowActions>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableCard>
      )}

      <ConfirmDelete
        open={!!kickTarget}
        title="Kick stream"
        message={`Disconnect the publisher of ${kickTarget?.app}/${kickTarget?.name}? All viewers will be disconnected too.`}
        confirmLabel="Kick"
        onCancel={() => setKickTarget(null)}
        onConfirm={doKick}
      />
    </>
  )
}
