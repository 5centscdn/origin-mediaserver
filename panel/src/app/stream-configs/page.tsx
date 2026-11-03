'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import clsx from 'clsx'
import { Plus, Pencil, Trash2, Copy, ListVideo, ExternalLink, Play } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Badge } from '@/components/Badge'
import { ConfirmDelete, inputClass } from '@/components/FormHelpers'
import { StreamConfigModal } from '@/components/StreamConfigModal'
import {
  PageHeader,
  Button,
  IconButton,
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
  EmptyState,
  TableSkeleton,
  StatusBadge,
  SearchInput,
  useToast,
} from '@/components/ui'
import {
  listApplications,
  listStreamConfigs,
  deleteStreamConfig,
  listStreams,
  Application,
  StreamConfig,
  getNodeInfo,
} from '@/lib/api'
import { copyToClipboard, matchesStreamPattern, listenPort } from '@/lib/utils'
import { usePreview } from '@/components/preview/StreamPreview'

// Streams: every stream defined under an application, in one list, like the
// standalone DVR server's Streams page. A stream is created by picking its
// parent application and naming it (several at once for an ABR set, e.g.
// test1..test5); exact-name streams can then be picked in the ABR editor
// even while offline. Live status comes from the publishers currently
// connected (Live Streams page). These are the same stream configs the
// Applications page shows under each application.

export default function StreamConfigsPage() {
  return (
    <Layout>
      <StreamConfigsContent />
    </Layout>
  )
}

type Row = { app: Application; cfg: StreamConfig }

// matches reports whether a live stream name fits a config's pattern.
function matches(cfg: StreamConfig, name: string): boolean {
  return cfg.is_wildcard ? matchesStreamPattern(cfg.name_pattern, name) : cfg.name_pattern === name
}

async function loadAll(): Promise<Row[]> {
  const apps = await listApplications()
  const per = await Promise.all(apps.map(async (app) => (await listStreamConfigs(app.id)).map((cfg) => ({ app, cfg }))))
  return per.flat()
}

type SortKey = 'name' | 'status'
type SortState = { key: SortKey; dir: 'asc' | 'desc' }

// Segmented control for the All / Live / Idle filter.
function Segmented<T extends string>({
  value,
  options,
  onChange,
  label,
}: {
  value: T
  options: { value: T; label: string }[]
  onChange: (v: T) => void
  label: string
}) {
  return (
    <div role="group" aria-label={label} className="inline-flex h-8 overflow-hidden rounded-md border border-border">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          onClick={() => onChange(o.value)}
          className={clsx(
            'border-r border-border px-3 text-[13px] transition-colors last:border-r-0 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary-fg',
            value === o.value ? 'bg-primary/15 font-medium text-primary-fg' : 'text-fg-secondary hover:bg-elevated hover:text-fg',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

function StreamConfigsContent() {
  const { data: rows, mutate, isLoading } = useSWR('all-stream-configs', loadAll)
  const preview = usePreview()
  const { data: live } = useSWR('streams', listStreams, { refreshInterval: 3000 })
  // Same key/interval as the dashboard, so SWR shares the request.
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 30000 })
  const toast = useToast()
  const [modal, setModal] = useState<{ open: boolean; edit: Row | null }>({ open: false, edit: null })
  const [del, setDel] = useState<Row | null>(null)
  const [status, setStatus] = useState<'all' | 'live' | 'idle'>('all')
  const [appFilter, setAppFilter] = useState('')
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<SortState>({ key: 'name', dir: 'asc' })

  const liveFor = (r: Row) => (live ?? []).filter((s) => s.app === r.app.name && matches(r.cfg, s.name))

  const apps = useMemo(() => Array.from(new Set((rows ?? []).map((r) => r.app.name))).sort(), [rows])

  // Default order: app, then name with numeric collation (test2 before test10).
  const byName = (a: Row, b: Row) =>
    a.app.name === b.app.name
      ? a.cfg.name_pattern.localeCompare(b.cfg.name_pattern, undefined, { numeric: true })
      : a.app.name.localeCompare(b.app.name)
  // Status rank for sorting: live, idle, disabled.
  const rank = (r: Row) => (!r.cfg.enabled ? 2 : liveFor(r).length > 0 ? 0 : 1)

  const shown = (rows ?? [])
    .filter((r) => !appFilter || r.app.name === appFilter)
    .filter((r) => !q || `${r.app.name}/${r.cfg.name_pattern}`.toLowerCase().includes(q.toLowerCase()))
    .filter((r) => {
      if (status === 'all') return true
      const isLive = liveFor(r).length > 0
      return status === 'live' ? isLive : !isLive
    })
    .sort((a, b) => {
      const mul = sort.dir === 'asc' ? 1 : -1
      if (sort.key === 'status') return (rank(a) - rank(b)) * mul || byName(a, b)
      return byName(a, b) * mul
    })

  const onSort = (key: SortKey) =>
    setSort((s) => (s.key === key ? { key, dir: s.dir === 'asc' ? 'desc' : 'asc' } : { key, dir: 'asc' }))
  const sortProps = (key: SortKey) => ({
    sort: sort.key === key ? sort.dir : (false as const),
    onSort: () => onSort(key),
  })

  const copyFromMenu = async (url: string, what: string) => {
    if (await copyToClipboard(url)) toast.success(`${what} copied`)
    else toast.error('Copy failed')
  }

  const tri = (v: number) => (v === 1 ? 'on' : v === 0 ? 'off' : null)

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const host = typeof window !== 'undefined' ? window.location.hostname : '<host>'
  const playURL = (r: Row) => `${origin}/${r.app.name}/${r.cfg.name_pattern}/playlist.m3u8`
  // Include the configured RTMP port (e.g. :1935) so the copied URL works as is.
  const rtmpPort = listenPort(node?.rtmp_listen, ':1935')
  const publishURL = (r: Row) => `rtmp://${host}${rtmpPort}/${r.app.name}/${r.cfg.name_pattern}`

  const total = (rows ?? []).length
  const filtered = !!(q || appFilter || status !== 'all')

  return (
    <>
      <PageHeader
        title="Streams"
        description="Streams defined under each application. Exact-name streams can be used in ABR groups before they go live."
        actions={
          <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={() => setModal({ open: true, edit: null })}>
            Add Stream
          </Button>
        }
      />

      {isLoading ? (
        <TableSkeleton rows={4} cols={6} />
      ) : total === 0 ? (
        <TableCard>
          <EmptyState
            icon={<ListVideo size={20} />}
            title="No streams yet"
            description="Choose the application and name the stream (several names at once for an ABR set)."
            action={
              <Button variant="primary" icon={<Plus size={14} />} onClick={() => setModal({ open: true, edit: null })}>
                Add Stream
              </Button>
            }
          />
        </TableCard>
      ) : (
        <TableCard
          header={
            <>
              <Segmented
                label="Filter by status"
                value={status}
                onChange={setStatus}
                options={[
                  { value: 'all', label: 'All' },
                  { value: 'live', label: 'Live' },
                  { value: 'idle', label: 'Idle' },
                ]}
              />
              <select
                value={appFilter}
                onChange={(e) => setAppFilter(e.target.value)}
                aria-label="Filter by application"
                className={clsx(inputClass, '!h-8 w-full text-[13px] sm:w-48')}
              >
                <option value="">All applications</option>
                {apps.map((a) => (
                  <option key={a} value={a}>
                    {a}
                  </option>
                ))}
              </select>
              <SearchInput value={q} onChange={setQ} placeholder="Filter by name…" />
              <span className="text-xs text-fg-muted sm:ml-auto">
                {shown.length} of {total} streams
              </span>
            </>
          }
        >
          <Table>
            <THead>
              <tr>
                <TH {...sortProps('name')}>Name</TH>
                <TH {...sortProps('status')}>Status</TH>
                <TH>Outputs</TH>
                <TH>DVR</TH>
                <TH>Publish / Play</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {shown.length === 0 && (
                <tr>
                  <TD colSpan={6} muted className="py-8 text-center">
                    {filtered ? 'No streams match the current filters.' : 'No streams.'}
                  </TD>
                </tr>
              )}
              {shown.map((r) => {
                const l = liveFor(r)
                const outs = (
                  [
                    ['HLS', r.cfg.out_hls],
                    ['DASH', r.cfg.out_dash],
                    ['MPEG-TS', r.cfg.out_mpegts],
                    ['LL-HLS', r.cfg.out_ll_hls],
                  ] as const
                ).filter(([, v]) => tri(v))
                const name = `${r.app.name}/${r.cfg.name_pattern}`
                return (
                  <TR key={r.cfg.id}>
                    <TD>
                      <div className="whitespace-nowrap font-mono text-xs text-fg">
                        <span className="text-fg-muted">{r.app.name}/</span>
                        {r.cfg.name_pattern}
                      </div>
                    </TD>
                    <TD>
                      <div className="flex flex-wrap items-center gap-1.5">
                        {!r.cfg.enabled ? (
                          <StatusBadge status="disabled" />
                        ) : l.length > 0 ? (
                          <StatusBadge status="live" label={`Live${r.cfg.is_wildcard ? ` · ${l.length}` : ''}`} />
                        ) : (
                          <StatusBadge status="idle" />
                        )}
                        {r.cfg.is_wildcard && (
                          <Badge variant="purple" title="Pattern: matches any stream name that fits">
                            pattern
                          </Badge>
                        )}
                      </div>
                    </TD>
                    <TD>
                      {outs.length === 0 ? (
                        <span className="text-xs text-fg-muted">app defaults</span>
                      ) : (
                        <div className="flex flex-wrap gap-1">
                          {outs.map(([n, v]) => (
                            <Badge key={n} variant={v === 1 ? 'green' : 'gray'} mono>
                              {n} {tri(v)}
                            </Badge>
                          ))}
                        </div>
                      )}
                    </TD>
                    <TD className="whitespace-nowrap text-xs">
                      {r.cfg.dvr_enabled === -1 ? (
                        <span className="text-fg-muted">app default</span>
                      ) : r.cfg.dvr_enabled === 1 ? (
                        `on${r.cfg.dvr_window_s ? ` · ${r.cfg.dvr_window_s}s` : ''}`
                      ) : (
                        'off'
                      )}
                    </TD>
                    <TD>
                      {r.cfg.is_wildcard ? (
                        <span className="text-xs text-fg-muted">
                          matches <code className="font-mono text-fg-secondary">{r.cfg.name_pattern}</code>
                        </span>
                      ) : (
                        <div className="space-y-1">
                          <div className="flex items-center gap-1.5">
                            <span className="w-11 shrink-0 text-xs text-fg-muted">RTMP</span>
                            <Truncate mono max="max-w-[220px]" className="min-w-0">
                              {publishURL(r)}
                            </Truncate>
                            <CopyButton text={publishURL(r)} title="Copy RTMP publish URL" toastMessage="Publish URL copied" />
                          </div>
                          <div className="flex items-center gap-1.5">
                            <span className="w-11 shrink-0 text-xs text-fg-muted">HLS</span>
                            <Truncate mono max="max-w-[220px]" className="min-w-0" title={playURL(r)}>
                              /{r.app.name}/{r.cfg.name_pattern}/playlist.m3u8
                            </Truncate>
                            <CopyButton text={playURL(r)} title="Copy HLS playback URL" toastMessage="Playback URL copied" />
                          </div>
                        </div>
                      )}
                    </TD>
                    <TD align="right">
                      <RowActions>
                        <IconButton
                          label={l.length ? `Preview ${l[0].app}/${l[0].name}` : `Preview ${name} (offline)`}
                          size="xs"
                          variant="secondary"
                          onClick={() =>
                            l.length
                              ? preview.open({ app: l[0].app, stream: l[0].name, codec: l[0].codec, bitrateBps: l[0].bitrate_bps })
                              : preview.open({ app: r.app.name, stream: r.cfg.name_pattern, live: false })
                          }
                        >
                          <Play size={13} />
                        </IconButton>
                        <IconButton label={`Edit ${name}`} size="xs" onClick={() => setModal({ open: true, edit: r })}>
                          <Pencil size={14} />
                        </IconButton>
                        <DropdownMenu
                          label={`More actions for ${name}`}
                          items={[
                            // Pattern rows: preview any other live match.
                            ...l.slice(1, 6).map((m) => ({
                              label: `Preview ${m.name}`,
                              icon: <Play size={14} />,
                              onClick: () => preview.open({ app: m.app, stream: m.name, codec: m.codec, bitrateBps: m.bitrate_bps }),
                            })),
                            ...(r.cfg.is_wildcard
                              ? []
                              : [
                                  {
                                    label: 'Open HLS playlist',
                                    icon: <ExternalLink size={14} />,
                                    href: `/${r.app.name}/${r.cfg.name_pattern}/playlist.m3u8`,
                                    external: true,
                                  },
                                  {
                                    label: 'Copy publish URL',
                                    icon: <Copy size={14} />,
                                    onClick: () => copyFromMenu(publishURL(r), 'Publish URL'),
                                  },
                                  {
                                    label: 'Copy playback URL',
                                    icon: <Copy size={14} />,
                                    onClick: () => copyFromMenu(playURL(r), 'Playback URL'),
                                  },
                                ]),
                            {
                              label: 'Delete stream',
                              icon: <Trash2 size={14} />,
                              danger: true,
                              separator: !r.cfg.is_wildcard || l.length > 1,
                              onClick: () => setDel(r),
                            },
                          ]}
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

      <StreamConfigModal
        open={modal.open}
        app={modal.edit?.app ?? null}
        edit={modal.edit?.cfg ?? null}
        onClose={() => setModal({ open: false, edit: null })}
        onSaved={() => {
          toast.success(modal.edit ? `Saved ${modal.edit.app.name}/${modal.edit.cfg.name_pattern}` : 'Stream added')
          setModal({ open: false, edit: null })
          mutate()
        }}
      />

      <ConfirmDelete
        open={!!del}
        title="Delete stream"
        message={`Delete ${del?.app.name}/${del?.cfg.name_pattern}? A live publisher isn't disconnected, but with "Allow Any Stream" off it can't reconnect, and ABR groups listing it will skip it.`}
        onCancel={() => setDel(null)}
        onConfirm={async () => {
          if (!del) return
          await deleteStreamConfig(del.app.id, del.cfg.id)
          toast.success(`Deleted ${del.app.name}/${del.cfg.name_pattern}`)
          setDel(null)
          mutate()
        }}
      />
    </>
  )
}
