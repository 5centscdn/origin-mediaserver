'use client'

import React, { useMemo, useState } from 'react'
import Link from 'next/link'
import useSWR from 'swr'
import {
  Radio,
  Layers,
  Eye,
  Download,
  AlertTriangle,
  Activity,
  Cpu,
  Server as ServerIcon,
  HeartPulse,
  ArrowRight,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { StatCard } from '@/components/StatCard'
import {
  PageHeader,
  Card,
  CardHeader,
  CardBody,
  StatusBadge,
  StatusDot,
  LineChart,
  Meter,
  Skeleton,
  CardSkeleton,
  ChartSeries,
} from '@/components/ui'
import {
  getMonitor,
  getMonitorHistory,
  listABRGroups,
  listPullSources,
  listStreams,
  MonitorRange,
  MonitorSample,
  MonitorSnapshot,
} from '@/lib/api'
import { formatBytes, formatBitrate, formatUptime } from '@/lib/utils'

export default function DashboardPage() {
  return (
    <Layout>
      <DashboardContent />
    </Layout>
  )
}

const RANGES: { value: MonitorRange; label: string; ms: number }[] = [
  { value: '5m', label: '5 min', ms: 5 * 60_000 },
  { value: '15m', label: '15 min', ms: 15 * 60_000 },
  { value: '1h', label: '1 hour', ms: 60 * 60_000 },
  { value: '24h', label: '24 hours', ms: 24 * 60 * 60_000 },
]

const pct = (v: number) => `${v.toFixed(v >= 10 ? 0 : 1)}%`
const mbps = (v: number) => formatBitrate(v)
const count = (v: number) => (Number.isInteger(v) ? String(v) : v.toFixed(1))

function DashboardContent() {
  // Polling: the snapshot is a cached copy on the server (no I/O), so 5 s
  // matches the collector's sample interval; history every 5 s for the
  // short ranges, every minute for 24 h (1-minute points).
  const [range, setRange] = useState<MonitorRange>('15m')
  const { data: mon, error: monErr } = useSWR('monitor', getMonitor, { refreshInterval: 5000 })
  const { data: hist } = useSWR(['monitor-history', range], () => getMonitorHistory(range), {
    refreshInterval: range === '24h' ? 60_000 : 5000,
    keepPreviousData: true,
  })
  const { data: groups } = useSWR('abr-groups', listABRGroups, { refreshInterval: 30_000 })
  const { data: pulls } = useSWR('pull-sources', listPullSources, { refreshInterval: 15_000 })
  const { data: streams } = useSWR('streams', listStreams, { refreshInterval: 10_000 })

  const r = RANGES.find((x) => x.value === range)!
  const samples = hist?.samples ?? []
  const stepMs = (hist?.step_s ?? 5) * 1000
  // Window end follows the newest sample so the chart doesn't jitter
  // between polls; before any data, now.
  const end = samples.length ? samples[samples.length - 1].t : mon?.time ?? Date.now()
  const domain: [number, number] = [end - r.ms, end]

  const times = useMemo(() => samples.map((s) => s.t), [samples])
  const netSeries = useMemo<ChartSeries[]>(
    () => [
      { key: 'in', label: 'Inbound', color: 'var(--series-1)', values: samples.map((s) => s.net_in_bps) },
      { key: 'out', label: 'Outbound', color: 'var(--series-2)', values: samples.map((s) => s.net_out_bps) },
    ],
    [samples],
  )
  const sysSeries = useMemo<ChartSeries[]>(
    () => [
      { key: 'cpu', label: 'CPU', color: 'var(--series-1)', values: samples.map((s) => s.cpu_pct) },
      { key: 'mem', label: 'Memory', color: 'var(--series-2)', values: samples.map((s) => s.mem_pct) },
    ],
    [samples],
  )
  const streamSeries = useMemo<ChartSeries[]>(
    () => [{ key: 'streams', label: 'Active streams', color: 'var(--series-1)', values: samples.map((s) => s.streams) }],
    [samples],
  )
  const rangeEvents = useMemo(() => sumEvents(samples), [samples])

  const liveKeys = useMemo(() => new Set((streams ?? []).map((s) => `${s.app}/${s.name}`)), [streams])
  const pullRunning = (pulls ?? []).filter((p) => p.status === 'active')
  const pullLive = pullRunning.filter((p) => liveKeys.has(`${p.app_name}/${p.stream_name}`)).length

  if (monErr && !mon) {
    return (
      <>
        <PageHeader title="Dashboard" description="Server health, network activity and streaming services." />
        <Card>
          <CardBody className="flex items-center gap-3 text-sm text-fg-secondary">
            <AlertTriangle size={16} className="text-warning" />
            Monitoring data is unavailable: {monErr.message}. Stream management still works from Live Streams.
          </CardBody>
        </Card>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title={mon?.host.hostname || 'Dashboard'}
        description="Server health, network activity and streaming services."
        meta={
          mon ? (
            <span className="inline-flex flex-wrap items-center gap-x-3 gap-y-1">
              <StatusBadge status="online" />
              <span>Up {formatUptime(mon.process.uptime_s)}</span>
              <span>· refreshes every {mon.interval_s}s</span>
            </span>
          ) : undefined
        }
      />

      {/* KPIs */}
      {!mon ? (
        <div className="grid grid-cols-2 gap-3 sm:gap-4 lg:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => (
            <CardSkeleton key={i} />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-3 sm:gap-4 lg:grid-cols-5">
          <StatCard
            label="Active streams"
            value={mon.streams.active}
            color="green"
            icon={<Radio size={18} />}
            sub={<Link href="/streams" className="hover:text-fg">View live streams →</Link>}
          />
          <StatCard
            label="ABR groups"
            value={groups?.length ?? '—'}
            color="purple"
            icon={<Layers size={18} />}
            sub={`${mon.abr_groups_live} with live variants`}
          />
          <StatCard label="Viewers" value={mon.streams.viewers} color="blue" icon={<Eye size={18} />} sub="HLS/DASH, last 20 s" />
          <StatCard
            label="Bytes received"
            value={formatBytes(mon.totals.bytes_in)}
            color="yellow"
            icon={<Download size={18} />}
            sub={`${formatBitrate(mon.network.ingest_bps)} ingest now`}
          />
          <StatCard
            label="Stalled streams"
            value={mon.streams.stalled}
            color={mon.streams.stalled > 0 ? 'red' : 'green'}
            icon={<AlertTriangle size={18} />}
            sub={mon.streams.stalled > 0 ? 'No media for over 10 s' : 'All streams receiving media'}
          />
        </div>
      )}

      {/* Range selector: one row above the charts, applies to all of them. */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-base font-semibold text-fg">Monitoring</h2>
        <div role="radiogroup" aria-label="Time range" className="inline-flex rounded-md border border-border bg-surface p-0.5">
          {RANGES.map((x) => (
            <button
              key={x.value}
              type="button"
              role="radio"
              aria-checked={range === x.value}
              onClick={() => setRange(x.value)}
              className={
                'h-7 rounded px-3 text-xs font-medium transition-colors ' +
                (range === x.value ? 'bg-elevated text-fg shadow-sm' : 'text-fg-muted hover:text-fg')
              }
            >
              {x.label}
            </button>
          ))}
        </div>
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2">
          <CardHeader
            icon={<Activity size={16} />}
            title="Network throughput"
            description="Host network interfaces, in and out (loopback and virtual bridges excluded)."
          />
          <CardBody>
            {mon && mon.network.in_bps == null && samples.every((s) => s.net_in_bps == null) && samples.length > 1 ? (
              <Unavailable what="Network counters" />
            ) : (
              <LineChart
                label={`Network throughput, last ${r.label}`}
                times={times}
                series={netSeries}
                domain={domain}
                stepMs={stepMs}
                format={mbps}
                height={220}
              />
            )}
          </CardBody>
        </Card>

        <ServiceHealth mon={mon} pullRunning={pullRunning.length} pullLive={pullLive} pullsLoaded={!!pulls} />
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2">
          <CardHeader icon={<Cpu size={16} />} title="CPU and memory" description="Whole-machine utilization." />
          <CardBody>
            <LineChart
              label={`CPU and memory utilization, last ${r.label}`}
              times={times}
              series={sysSeries}
              domain={domain}
              stepMs={stepMs}
              format={pct}
              yMax={100}
              height={180}
            />
          </CardBody>
        </Card>
        <Resources mon={mon} />
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2">
          <CardHeader
            icon={<HeartPulse size={16} />}
            title="Stream availability"
            description={range === '24h' ? 'Active streams, 1-minute averages.' : 'Active streams over time.'}
          />
          <CardBody>
            <LineChart
              label={`Active streams, last ${r.label}`}
              times={times}
              series={streamSeries}
              domain={domain}
              stepMs={stepMs}
              format={count}
              height={160}
            />
          </CardBody>
        </Card>
        <StreamEvents mon={mon} range={r.label} events={rangeEvents} />
      </div>
    </>
  )
}

function sumEvents(samples: MonitorSample[]) {
  const e = { published: 0, ended: 0, pull_attempts: 0, pull_failures: 0 }
  for (const s of samples) {
    e.published += s.published
    e.ended += s.ended
    e.pull_attempts += s.pull_attempts
    e.pull_failures += s.pull_failures
  }
  return e
}

function Unavailable({ what }: { what: string }) {
  return (
    <div className="flex h-[160px] items-center justify-center gap-2 text-[13px] text-fg-muted">
      <AlertTriangle size={15} />
      {what} unavailable on this host.
    </div>
  )
}

const statusKind = (s: string) => (s === 'up' ? 'online' : s === 'down' ? 'offline' : s === 'disabled' ? 'disabled' : 'idle')
const statusText = (s: string) => (s === 'up' ? 'Running' : s === 'down' ? 'Not listening' : s === 'disabled' ? 'Disabled' : 'Unknown')

function ServiceHealth({
  mon,
  pullRunning,
  pullLive,
  pullsLoaded,
}: {
  mon?: MonitorSnapshot
  pullRunning: number
  pullLive: number
  pullsLoaded: boolean
}) {
  return (
    <Card>
      <CardHeader icon={<ServerIcon size={16} />} title="Service health" description="Listeners checked against the kernel's socket tables." />
      {!mon ? (
        <CardBody className="space-y-3">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-5" />
          ))}
        </CardBody>
      ) : (
        <ul className="divide-y divide-border">
          {mon.services.map((s) => (
            <li key={s.name} className="flex items-center justify-between gap-3 px-5 py-3">
              <div className="min-w-0">
                <div className="text-[13px] font-medium text-fg">{s.name}</div>
                <div className="truncate font-mono text-xs text-fg-muted">
                  {s.enabled ? `${s.proto} ${s.listen}` : 'not enabled in config'}
                </div>
              </div>
              <StatusBadge status={statusKind(s.status)} label={statusText(s.status)} />
            </li>
          ))}
          <li className="flex items-center justify-between gap-3 px-5 py-3">
            <div>
              <div className="text-[13px] font-medium text-fg">Pull sources</div>
              <div className="text-xs text-fg-muted">
                {pullsLoaded ? `${pullRunning} running · ${pullLive} receiving` : 'Loading…'}
              </div>
            </div>
            {pullsLoaded &&
              (pullRunning === 0 ? (
                <StatusBadge status="idle" label="None running" />
              ) : pullLive === pullRunning ? (
                <StatusBadge status="online" label="All receiving" />
              ) : (
                <StatusBadge status="warning" label={`${pullRunning - pullLive} not receiving`} />
              ))}
          </li>
          <li className="flex items-center justify-between gap-3 px-5 py-3">
            <div className="text-[13px] font-medium text-fg">Active streams</div>
            <Link href="/streams" className="tabular inline-flex items-center gap-1 text-sm font-medium text-fg hover:text-primary-fg">
              {mon.streams.active}
              <ArrowRight size={13} />
            </Link>
          </li>
        </ul>
      )}
    </Card>
  )
}

function Resources({ mon }: { mon?: MonitorSnapshot }) {
  if (!mon) {
    return (
      <Card>
        <CardHeader title="Resources" />
        <CardBody className="space-y-4">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-8" />
          ))}
        </CardBody>
      </Card>
    )
  }
  const lc = mon.live_cache
  const dvrDetail =
    mon.dvr_bytes == null
      ? undefined
      : `${formatBytes(mon.dvr_bytes)} recorded` + (mon.dvr_disk ? ` · ${formatBytes(mon.dvr_disk.total - mon.dvr_disk.used)} free` : '')
  return (
    <Card>
      <CardHeader icon={<Cpu size={16} />} title="Resources" description={`${mon.host.cores} cores`} />
      <CardBody className="space-y-4">
        <Meter
          label="CPU"
          pct={mon.cpu_pct}
          detail={mon.process.cpu_pct != null ? `Media server process: ${mon.process.cpu_pct.toFixed(1)}%` : undefined}
        />
        <Meter
          label="Memory"
          pct={mon.memory?.pct ?? null}
          detail={
            mon.memory
              ? `${formatBytes(mon.memory.used)} of ${formatBytes(mon.memory.total)}` +
                (mon.process.rss_bytes != null ? ` · server ${formatBytes(mon.process.rss_bytes)}` : '')
              : undefined
          }
        />
        <Meter
          label="Disk (data directory)"
          pct={mon.disk?.pct ?? null}
          detail={mon.disk ? `${formatBytes(mon.disk.used)} of ${formatBytes(mon.disk.total)}` : undefined}
        />
        <Meter label="DVR storage" pct={mon.dvr_disk?.pct ?? null} detail={dvrDetail} />
        <div className="grid grid-cols-2 gap-3 border-t border-border pt-4 text-[13px]">
          <div>
            <div className="text-fg-muted">Live cache</div>
            <div className="mt-0.5 flex items-center gap-1.5 font-medium text-fg">
              <StatusDot status={lc.mode === 'disk' && lc.policy !== 'disk' ? 'warning' : 'online'} />
              {lc.mode === 'disk' ? 'Disk' : 'RAM'}
              <span className="tabular font-normal text-fg-muted">
                {formatBytes(lc.mode === 'disk' ? lc.disk_bytes : lc.bytes)}
              </span>
            </div>
          </div>
          <div>
            <div className="text-fg-muted">Connections</div>
            <div className="tabular mt-0.5 font-medium text-fg">
              {mon.connections ? (
                <>
                  {mon.connections.total}{' '}
                  <span className="font-normal text-fg-muted">
                    HTTP {mon.connections.http} · RTMP {mon.connections.rtmp}
                  </span>
                </>
              ) : (
                <span className="font-normal text-fg-muted">Unavailable</span>
              )}
            </div>
          </div>
          <div>
            <div className="text-fg-muted">Server uptime</div>
            <div className="tabular mt-0.5 font-medium text-fg">{formatUptime(mon.process.uptime_s)}</div>
          </div>
          <div>
            <div className="text-fg-muted">Host uptime</div>
            <div className="tabular mt-0.5 font-medium text-fg">
              {mon.host.uptime_s != null ? formatUptime(mon.host.uptime_s) : <span className="font-normal text-fg-muted">Unavailable</span>}
            </div>
          </div>
        </div>
      </CardBody>
    </Card>
  )
}

function StreamEvents({
  mon,
  range,
  events,
}: {
  mon?: MonitorSnapshot
  range: string
  events: ReturnType<typeof sumEvents>
}) {
  const rows: { label: string; hint: string; range: number; total?: number; bad?: boolean }[] = [
    { label: 'Streams started', hint: 'Publishes, pull connects, failover backups', range: events.published, total: mon?.totals.published },
    { label: 'Streams ended', hint: 'Disconnects of any kind', range: events.ended, total: mon?.totals.ended },
    { label: 'Pull attempts', hint: 'Connects, reconnects and fallback switches', range: events.pull_attempts, total: mon?.totals.pull_attempts },
    { label: 'Pull failures', hint: 'Errors or no media for 10 s', range: events.pull_failures, total: mon?.totals.pull_failures, bad: true },
  ]
  return (
    <Card>
      <CardHeader icon={<HeartPulse size={16} />} title="Stream events" description={`Last ${range}, and since the server started.`} />
      <table className="w-full text-[13px]">
        <thead>
          <tr className="border-b border-border text-xs text-fg-muted">
            <th className="px-5 py-2 text-left font-medium">Event</th>
            <th className="px-3 py-2 text-right font-medium">{range}</th>
            <th className="px-5 py-2 text-right font-medium">Total</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {rows.map((row) => (
            <tr key={row.label}>
              <td className="px-5 py-2.5">
                <div className="font-medium text-fg">{row.label}</div>
                <div className="text-xs text-fg-muted">{row.hint}</div>
              </td>
              <td className={'tabular px-3 py-2.5 text-right font-medium ' + (row.bad && row.range > 0 ? 'text-danger' : 'text-fg')}>
                {row.range}
              </td>
              <td className="tabular px-5 py-2.5 text-right text-fg-secondary">{row.total ?? '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  )
}
