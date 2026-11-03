'use client'

import React, { memo, useEffect, useMemo, useRef, useState } from 'react'
import clsx from 'clsx'

export interface ChartSeries {
  key: string
  label: string
  /** CSS color, e.g. 'var(--series-1)'. Categorical slots in fixed order. */
  color: string
  values: (number | null)[]
}

interface LineChartProps {
  /** Sample timestamps (unix ms), one per value, ascending. */
  times: number[]
  series: ChartSeries[]
  /** Visible time window [start, end] in unix ms; the chart spans it even with little data. */
  domain: [number, number]
  /** Expected spacing between samples (ms); a bigger gap breaks the line. */
  stepMs: number
  format: (v: number) => string
  /** Fixed y maximum (e.g. 100 for percentages); otherwise from the data. */
  yMax?: number
  height?: number
  /** Accessible name, e.g. "Network throughput, last 15 minutes". */
  label: string
}

const M = { top: 10, right: 12, bottom: 24, left: 60 }

/** Rounds up to a "nice" axis maximum (1, 2, 2.5, 5 × 10^n). */
function niceMax(v: number): number {
  if (v <= 0) return 1
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p
  return 10 * p
}

function timeLabel(t: number, withSeconds: boolean) {
  const d = new Date(t)
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return withSeconds ? `${hh}:${mm}:${String(d.getSeconds()).padStart(2, '0')}` : `${hh}:${mm}`
}

/**
 * Time-series line chart (SVG, no dependencies). One y-axis; 2px lines;
 * gaps where samples are missing (null) or further apart than 2.5 steps;
 * crosshair snapping to the nearest sample with a tooltip listing every
 * series; arrow keys move the crosshair; legend carries the latest values
 * so identity is never color alone; "Table" shows the recent samples.
 */
export const LineChart = memo(function LineChart({
  times,
  series,
  domain,
  stepMs,
  format,
  yMax,
  height = 200,
  label,
}: LineChartProps) {
  const wrap = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  const [hover, setHover] = useState<number | null>(null)
  const [table, setTable] = useState(false)

  useEffect(() => {
    const el = wrap.current
    if (!el) return
    const ro = new ResizeObserver(([e]) => setWidth(Math.floor(e.contentRect.width)))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const plotW = Math.max(0, width - M.left - M.right)
  const plotH = height - M.top - M.bottom
  const [t0, t1] = domain
  const max = useMemo(() => {
    if (yMax) return yMax
    let m = 0
    for (const s of series) for (const v of s.values) if (v != null && v > m) m = v
    return niceMax(m * 1.1)
  }, [series, yMax])

  const x = (t: number) => M.left + ((t - t0) / Math.max(1, t1 - t0)) * plotW
  const y = (v: number) => M.top + plotH - (Math.min(v, max) / max) * plotH

  const paths = useMemo(
    () =>
      series.map((s) => {
        let d = ''
        let prevT = -Infinity
        s.values.forEach((v, i) => {
          const t = times[i]
          if (v == null || t < t0) {
            prevT = -Infinity
            return
          }
          const gap = t - prevT > stepMs * 2.5
          d += `${gap ? 'M' : 'L'}${x(t).toFixed(1)},${y(v).toFixed(1)}`
          prevT = t
        })
        return d
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [series, times, t0, t1, plotW, plotH, max, stepMs],
  )

  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * max)
  const xTicks = useMemo(() => {
    const n = Math.max(2, Math.min(6, Math.floor(plotW / 110)))
    return Array.from({ length: n + 1 }, (_, i) => t0 + ((t1 - t0) * i) / n)
  }, [t0, t1, plotW])
  const withSeconds = t1 - t0 <= 15 * 60 * 1000

  const nearest = (clientX: number) => {
    const r = wrap.current?.getBoundingClientRect()
    if (!r || times.length === 0) return null
    const t = t0 + ((clientX - r.left - M.left) / Math.max(1, plotW)) * (t1 - t0)
    let lo = 0
    let hi = times.length - 1
    while (lo < hi) {
      const mid = (lo + hi) >> 1
      if (times[mid] < t) lo = mid + 1
      else hi = mid
    }
    if (lo > 0 && Math.abs(times[lo - 1] - t) < Math.abs(times[lo] - t)) lo--
    return times[lo] >= t0 ? lo : null
  }

  const onKey = (e: React.KeyboardEvent) => {
    if (times.length === 0) return
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
      e.preventDefault()
      const cur = hover ?? times.length - 1
      setHover(Math.max(0, Math.min(times.length - 1, cur + (e.key === 'ArrowLeft' ? -1 : 1))))
    } else if (e.key === 'Escape') setHover(null)
  }

  const latest = (s: ChartSeries) => {
    for (let i = s.values.length - 1; i >= 0; i--) if (s.values[i] != null) return s.values[i]
    return null
  }

  const hx = hover != null ? x(times[hover]) : 0
  const tipLeft = hover != null && hx > M.left + plotW / 2

  return (
    <div>
      {/* Legend: identity + latest value (direct labels). */}
      <div className="mb-3 flex flex-wrap items-center gap-x-5 gap-y-1">
        {series.map((s) => {
          const v = latest(s)
          return (
            <div key={s.key} className="flex items-center gap-2 text-[13px]">
              <span className="h-0.5 w-4 rounded-full" style={{ background: s.color }} aria-hidden />
              <span className="text-fg-secondary">{s.label}</span>
              <span className="tabular font-medium text-fg">{v == null ? '—' : format(v)}</span>
            </div>
          )
        })}
        <button
          type="button"
          onClick={() => setTable((v) => !v)}
          aria-pressed={table}
          className="ml-auto rounded px-1.5 py-0.5 text-xs text-fg-muted transition-colors hover:bg-elevated hover:text-fg"
        >
          {table ? 'Chart' : 'Table'}
        </button>
      </div>

      {table ? (
        <ChartTable times={times} series={series} format={format} withSeconds={withSeconds} />
      ) : (
        <div
          ref={wrap}
          className="relative select-none focus:outline-none focus-visible:rounded focus-visible:ring-2 focus-visible:ring-primary-fg"
          style={{ height }}
          tabIndex={0}
          role="img"
          aria-label={`${label}. Use left and right arrow keys to read values.`}
          onKeyDown={onKey}
          onPointerMove={(e) => setHover(nearest(e.clientX))}
          onPointerLeave={() => setHover(null)}
          onBlur={() => setHover(null)}
        >
          {width > 0 && (
            <svg width={width} height={height} className="block overflow-visible">
              {/* Recessive grid + y labels */}
              {ticks.map((v, i) => (
                <g key={i}>
                  <line
                    x1={M.left}
                    x2={M.left + plotW}
                    y1={y(v)}
                    y2={y(v)}
                    stroke="rgb(var(--border))"
                    strokeDasharray={i === 0 ? undefined : '2 4'}
                  />
                  <text x={M.left - 8} y={y(v)} dy="0.32em" textAnchor="end" className="tabular fill-fg-muted text-[11px]">
                    {format(v)}
                  </text>
                </g>
              ))}
              {xTicks.map((t, i) => (
                <text
                  key={i}
                  x={x(t)}
                  y={height - 6}
                  textAnchor={i === 0 ? 'start' : i === xTicks.length - 1 ? 'end' : 'middle'}
                  className="tabular fill-fg-muted text-[11px]"
                >
                  {timeLabel(t, false)}
                </text>
              ))}
              {/* Lines */}
              {series.map((s, i) => (
                <path key={s.key} d={paths[i]} fill="none" stroke={s.color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
              ))}
              {/* Crosshair + markers (2px surface ring) */}
              {hover != null && (
                <g>
                  <line x1={hx} x2={hx} y1={M.top} y2={M.top + plotH} stroke="rgb(var(--fg-muted))" strokeWidth={1} />
                  {series.map((s) => {
                    const v = s.values[hover]
                    return v == null ? null : (
                      <circle key={s.key} cx={hx} cy={y(v)} r={4} fill={s.color} stroke="rgb(var(--surface))" strokeWidth={2} />
                    )
                  })}
                </g>
              )}
            </svg>
          )}
          {times.length === 0 && (
            <div className="absolute inset-0 flex items-center justify-center text-[13px] text-fg-muted">
              Collecting data… the first points appear within 10 seconds.
            </div>
          )}
          {hover != null && (
            <div
              className="pointer-events-none absolute top-1 z-10 min-w-[150px] rounded-md border border-border bg-elevated px-3 py-2 text-xs shadow-pop"
              style={tipLeft ? { right: width - hx + 10 } : { left: hx + 10 }}
            >
              <div className="tabular mb-1 text-fg-muted">
                {new Date(times[hover]).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}{' '}
                {timeLabel(times[hover], true)}
              </div>
              {series.map((s) => {
                const v = s.values[hover]
                return (
                  <div key={s.key} className="flex items-center justify-between gap-4">
                    <span className="flex items-center gap-1.5 text-fg-secondary">
                      <span className="h-2 w-2 rounded-full" style={{ background: s.color }} aria-hidden />
                      {s.label}
                    </span>
                    <span className="tabular font-medium text-fg">{v == null ? 'no data' : format(v)}</span>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      )}
    </div>
  )
})

/** Accessible table view of the most recent samples. */
function ChartTable({
  times,
  series,
  format,
  withSeconds,
}: {
  times: number[]
  series: ChartSeries[]
  format: (v: number) => string
  withSeconds: boolean
}) {
  const rows = Math.min(times.length, 15)
  const idx = Array.from({ length: rows }, (_, i) => times.length - 1 - i)
  return (
    <div className="max-h-[220px] overflow-auto rounded-md border border-border">
      <table className="w-full text-xs">
        <thead className="sticky top-0 bg-elevated">
          <tr>
            <th className="px-3 py-1.5 text-left font-medium text-fg-muted">Time</th>
            {series.map((s) => (
              <th key={s.key} className="px-3 py-1.5 text-right font-medium text-fg-muted">
                {s.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {idx.map((i) => (
            <tr key={times[i]}>
              <td className="tabular px-3 py-1.5 text-fg-secondary">{timeLabel(times[i], true)}</td>
              {series.map((s) => (
                <td key={s.key} className="tabular px-3 py-1.5 text-right text-fg">
                  {s.values[i] == null ? '—' : format(s.values[i] as number)}
                </td>
              ))}
            </tr>
          ))}
          {rows === 0 && (
            <tr>
              <td colSpan={series.length + 1} className="px-3 py-4 text-center text-fg-muted">
                No samples yet.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

/** Horizontal meter: one ratio against a limit (CPU, RAM, disk). */
export function Meter({
  label,
  pct,
  detail,
  warnAt = 80,
  critAt = 90,
}: {
  label: string
  /** 0-100, or null when unavailable. */
  pct: number | null
  detail?: React.ReactNode
  warnAt?: number
  critAt?: number
}) {
  const tone = pct == null ? '' : pct >= critAt ? 'bg-danger' : pct >= warnAt ? 'bg-warning' : 'bg-primary-fg'
  const state = pct == null ? null : pct >= critAt ? 'critical' : pct >= warnAt ? 'high' : null
  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between gap-3">
        <span className="text-[13px] text-fg-secondary">{label}</span>
        <span className="tabular text-[13px] font-medium text-fg">
          {pct == null ? <span className="font-normal text-fg-muted">Unavailable</span> : `${pct.toFixed(1)}%`}
          {state && <span className={clsx('ml-1.5 text-xs font-medium', pct! >= critAt ? 'text-danger' : 'text-warning')}>{state}</span>}
        </span>
      </div>
      <div
        className="h-1.5 overflow-hidden rounded-full bg-elevated"
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct ?? undefined}
      >
        {pct != null && <div className={clsx('h-full rounded-full transition-[width] duration-500', tone)} style={{ width: `${Math.max(1, Math.min(100, pct))}%` }} />}
      </div>
      {detail && <div className="tabular mt-1 text-xs text-fg-muted">{detail}</div>}
    </div>
  )
}
