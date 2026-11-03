'use client'

import { useState } from 'react'
import Link from 'next/link'
import useSWR from 'swr'
import { Radio, Layers, Eye, Activity, ExternalLink, Power, ArrowRight } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { StatCard } from '@/components/StatCard'
import { Badge } from '@/components/Badge'
import { ConfirmDelete } from '@/components/FormHelpers'
import {
  PageHeader,
  SectionTitle,
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
  Button,
  EmptyState,
  TableSkeleton,
  StatusDot,
  StatusBadge,
  buttonClass,
  useToast,
} from '@/components/ui'
import { getNodeInfo, listStreams, listABRGroups, kickStream, Stream } from '@/lib/api'
import { formatBytes, formatUptime, listenPort } from '@/lib/utils'

// Source type → badge color: push ingest vs pulled sources (same mapping
// as the Live Streams page).
function sourceVariant(t: string): 'blue' | 'purple' | 'gray' {
  if (t.endsWith('_pull') || t === 'pull') return 'purple'
  return t ? 'blue' : 'gray'
}

export default function DashboardPage() {
  return (
    <Layout>
      <DashboardContent />
    </Layout>
  )
}

function DashboardContent() {
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 5000 })
  const {
    data: streams,
    mutate: refresh,
    isLoading,
  } = useSWR('streams', listStreams, {
    refreshInterval: 3000,
  })
  const { data: groups } = useSWR('abr-groups', listABRGroups, {
    refreshInterval: 10000,
  })
  const toast = useToast()

  const [kickTarget, setKickTarget] = useState<Stream | null>(null)

  const list = streams ?? []
  const totalBytesIn = list.reduce((s, st) => s + (st.bytes_in || 0), 0)
  const totalSubs = list.reduce((s, st) => s + (st.subscribers || 0), 0)

  const doKick = async () => {
    if (!kickTarget) return
    await kickStream(kickTarget.app, kickTarget.name) // ConfirmDelete shows a thrown error
    toast.success(`Disconnected ${kickTarget.app}/${kickTarget.name}`)
    setKickTarget(null)
    refresh()
  }

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const hlsPath = (s: Stream) => `/${s.app}/${s.name}/playlist.m3u8`

  return (
    <>
      <PageHeader
        title={node?.node_name ?? 'Dashboard'}
        description={`Origin Media Server v${node?.version ?? '—'}`}
      />

      <div className="grid grid-cols-2 gap-3 sm:gap-4 lg:grid-cols-4">
        <StatCard label="Live Streams" value={list.length} color="green" icon={<Radio size={18} />} />
        <StatCard label="ABR Groups" value={groups?.length ?? 0} color="blue" icon={<Layers size={18} />} />
        <StatCard label="Total Viewers" value={totalSubs} color="purple" icon={<Eye size={18} />} />
        <StatCard label="Bytes In" value={formatBytes(totalBytesIn)} color="yellow" icon={<Activity size={18} />} />
      </div>

      <section className="space-y-3">
        <SectionTitle
          description={
            list.length > 0 ? (
              <span className="inline-flex items-center gap-2">
                <StatusDot status="live" />
                {list.length} active · auto-refresh 3s
              </span>
            ) : undefined
          }
          actions={
            <Link href="/streams" className={buttonClass('ghost', 'sm')}>
              View all <ArrowRight size={14} />
            </Link>
          }
        >
          Live Streams
        </SectionTitle>

        {isLoading ? (
          <TableSkeleton rows={3} cols={7} />
        ) : list.length === 0 ? (
          <TableCard>
            <EmptyState
              icon={<Radio size={20} />}
              title="No active streams"
              description={
                <div className="space-y-2">
                  <p>Streams appear here as soon as a publisher connects. Publish via RTMP:</p>
                  <code className="block break-all rounded-md border border-border bg-canvas/60 px-3 py-1.5 font-mono text-xs text-fg-secondary">
                    rtmp://&lt;host&gt;{listenPort(node?.rtmp_listen, ':1935')}/live/&lt;stream-key&gt;
                  </code>
                  {node?.srt?.enabled && (
                    <>
                      <p>Or via SRT:</p>
                      <code className="block break-all rounded-md border border-border bg-canvas/60 px-3 py-1.5 font-mono text-xs text-fg-secondary">
                        srt://&lt;host&gt;{listenPort(node?.srt?.listen, ':4000')}?streamid=live/&lt;stream-key&gt;
                      </code>
                    </>
                  )}
                </div>
              }
            />
          </TableCard>
        ) : (
          <TableCard>
            <Table>
              <THead>
                <tr>
                  <TH>Stream</TH>
                  <TH>Application</TH>
                  <TH>Source</TH>
                  <TH>Status</TH>
                  <TH align="right">Uptime</TH>
                  <TH align="right">Viewers</TH>
                  <TH align="right">Bytes in</TH>
                  <TH align="right">
                    <span className="sr-only">Actions</span>
                  </TH>
                </tr>
              </THead>
              <TBody>
                {list.map((s) => (
                  <TR key={s.id}>
                    <TD>
                      <span className="font-medium text-fg">{s.name}</span>
                    </TD>
                    <TD>{s.app}</TD>
                    <TD>
                      <div className="flex flex-col items-start gap-1">
                        <Badge variant={sourceVariant(s.source_type || '')} mono>
                          {s.source_type || '—'}
                        </Badge>
                        {s.source_addr && (
                          <Truncate mono max="max-w-[200px]" className="text-fg-muted">
                            {s.source_addr}
                          </Truncate>
                        )}
                      </div>
                    </TD>
                    <TD>
                      <StatusBadge status="live" />
                    </TD>
                    <TD numeric>{formatUptime(s.uptime_s)}</TD>
                    <TD numeric className={s.subscribers > 0 ? 'text-fg' : undefined}>
                      {s.subscribers}
                    </TD>
                    <TD numeric>{formatBytes(s.bytes_in)}</TD>
                    <TD align="right">
                      <RowActions>
                        <a
                          href={hlsPath(s)}
                          target="_blank"
                          rel="noreferrer"
                          title="Open HLS playlist in a new tab"
                          className={buttonClass('secondary', 'xs')}
                        >
                          <ExternalLink size={13} />
                          HLS
                        </a>
                        <CopyButton text={`${origin}${hlsPath(s)}`} title="Copy HLS URL" toastMessage="HLS URL copied" />
                        <Button
                          variant="danger-outline"
                          size="xs"
                          icon={<Power size={13} />}
                          onClick={() => setKickTarget(s)}
                          title={`Disconnect the publisher of ${s.app}/${s.name}`}
                        >
                          Kick
                        </Button>
                      </RowActions>
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableCard>
        )}
      </section>

      <ConfirmDelete
        open={!!kickTarget}
        title="Kick stream"
        message={`Disconnect the publisher of ${kickTarget?.app}/${kickTarget?.name}? Subscribers will be disconnected.`}
        confirmLabel="Kick"
        onCancel={() => setKickTarget(null)}
        onConfirm={doKick}
      />
    </>
  )
}
