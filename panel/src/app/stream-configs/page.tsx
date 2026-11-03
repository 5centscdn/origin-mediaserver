'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import { Plus, Pencil, Trash2, Copy, Check, ListVideo } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Badge } from '@/components/Badge'
import { ConfirmDelete } from '@/components/FormHelpers'
import { StreamConfigModal } from '@/components/StreamConfigModal'
import {
  listApplications,
  listStreamConfigs,
  deleteStreamConfig,
  listStreams,
  Application,
  StreamConfig,
} from '@/lib/api'
import { copyToClipboard, matchesStreamPattern } from '@/lib/utils'

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

function StreamConfigsContent() {
  const { data: rows, mutate, isLoading } = useSWR('all-stream-configs', loadAll)
  const { data: live } = useSWR('streams', listStreams, { refreshInterval: 3000 })
  const [modal, setModal] = useState<{ open: boolean; edit: Row | null }>({ open: false, edit: null })
  const [del, setDel] = useState<Row | null>(null)
  const [status, setStatus] = useState<'all' | 'live' | 'idle'>('all')
  const [appFilter, setAppFilter] = useState('')
  const [q, setQ] = useState('')
  const [copied, setCopied] = useState<string | null>(null)

  const liveFor = (r: Row) => (live ?? []).filter((s) => s.app === r.app.name && matches(r.cfg, s.name))

  const apps = useMemo(
    () => Array.from(new Set((rows ?? []).map((r) => r.app.name))).sort(),
    [rows],
  )
  const shown = (rows ?? [])
    .filter((r) => !appFilter || r.app.name === appFilter)
    .filter((r) => !q || `${r.app.name}/${r.cfg.name_pattern}`.toLowerCase().includes(q.toLowerCase()))
    .filter((r) => {
      if (status === 'all') return true
      const isLive = liveFor(r).length > 0
      return status === 'live' ? isLive : !isLive
    })
    .sort((a, b) =>
      a.app.name === b.app.name
        ? a.cfg.name_pattern.localeCompare(b.cfg.name_pattern, undefined, { numeric: true })
        : a.app.name.localeCompare(b.app.name),
    )

  const copy = async (url: string, id: string) => {
    if (await copyToClipboard(url)) {
      setCopied(id)
      setTimeout(() => setCopied(null), 1500)
    }
  }

  const tab = (active: boolean) =>
    'px-3 py-1.5 text-xs border-r border-gray-700 last:border-r-0 transition ' +
    (active ? 'bg-blue-600/20 text-blue-300' : 'text-gray-400 hover:bg-gray-800')

  const tri = (v: number) => (v === 1 ? 'on' : v === 0 ? 'off' : null)

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-center justify-between gap-4 flex-wrap">
        <div>
          <h1 className="text-2xl font-semibold text-white">Streams</h1>
          <p className="text-sm text-gray-500 mt-1">
            Streams defined under each application. Exact-name streams can be used in ABR groups
            before they go live.
          </p>
        </div>
        <button
          type="button"
          onClick={() => setModal({ open: true, edit: null })}
          className="inline-flex items-center gap-1.5 px-3 py-2 text-sm rounded bg-blue-600 hover:bg-blue-500 text-white transition"
        >
          <Plus size={16} /> Add Stream
        </button>
      </div>

      <div className="flex items-center gap-3 flex-wrap">
        <div className="inline-flex rounded border border-gray-700 overflow-hidden">
          {(['all', 'live', 'idle'] as const).map((s) => (
            <button key={s} type="button" onClick={() => setStatus(s)} className={tab(status === s)}>
              {s === 'all' ? 'All' : s === 'live' ? 'Live' : 'Idle'}
            </button>
          ))}
        </div>
        <select
          value={appFilter}
          onChange={(e) => setAppFilter(e.target.value)}
          className="bg-gray-900 border border-gray-700 rounded px-2 py-1.5 text-xs text-gray-300"
        >
          <option value="">All applications</option>
          {apps.map((a) => (
            <option key={a} value={a}>
              {a}
            </option>
          ))}
        </select>
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Filter by name"
          className="bg-gray-900 border border-gray-700 rounded px-2 py-1.5 text-xs text-gray-300 w-56"
        />
        <span className="text-xs text-gray-500 ml-auto">
          {shown.length} of {(rows ?? []).length} streams
        </span>
      </div>

      {isLoading ? (
        <div className="text-gray-500 text-sm">Loading…</div>
      ) : (rows ?? []).length === 0 ? (
        <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
          <ListVideo className="mx-auto text-gray-600 mb-3" size={32} />
          <div className="text-gray-400 text-sm font-medium">No streams yet</div>
          <div className="mt-2 text-xs text-gray-500">
            Click <span className="text-gray-300">Add Stream</span>, choose the application, and
            name the stream (several names at once for an ABR set).
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-gray-800/50 text-gray-400 text-xs uppercase tracking-wider">
              <tr>
                <th className="px-4 py-2 text-left font-medium">Name</th>
                <th className="px-4 py-2 text-left font-medium">Status</th>
                <th className="px-4 py-2 text-left font-medium">Outputs</th>
                <th className="px-4 py-2 text-left font-medium">DVR</th>
                <th className="px-4 py-2 text-left font-medium">Publish / Play</th>
                <th className="px-4 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800">
              {shown.map((r) => {
                const l = liveFor(r)
                const id = r.cfg.id
                const play = `${typeof window !== 'undefined' ? window.location.origin : ''}/${r.app.name}/${r.cfg.name_pattern}/playlist.m3u8`
                const outs = (
                  [
                    ['HLS', r.cfg.out_hls],
                    ['DASH', r.cfg.out_dash],
                    ['MPEG-TS', r.cfg.out_mpegts],
                    ['LL-HLS', r.cfg.out_ll_hls],
                  ] as const
                ).filter(([, v]) => tri(v))
                return (
                  <tr key={id} className="hover:bg-gray-800/20 transition">
                    <td className="px-4 py-2.5">
                      <div className="font-mono text-gray-100">
                        <span className="text-gray-500">{r.app.name}/</span>
                        {r.cfg.name_pattern}
                      </div>
                    </td>
                    <td className="px-4 py-2.5">
                      <div className="flex flex-wrap items-center gap-1">
                        {!r.cfg.enabled ? (
                          <Badge variant="gray">disabled</Badge>
                        ) : l.length > 0 ? (
                          <Badge variant="green">
                            live{r.cfg.is_wildcard ? ` · ${l.length}` : ''}
                          </Badge>
                        ) : (
                          <Badge variant="gray">idle</Badge>
                        )}
                        {r.cfg.is_wildcard && <Badge variant="purple">pattern</Badge>}
                      </div>
                    </td>
                    <td className="px-4 py-2.5 text-xs text-gray-400">
                      {outs.length === 0
                        ? 'app defaults'
                        : outs.map(([n, v]) => `${n} ${tri(v)}`).join(' · ')}
                    </td>
                    <td className="px-4 py-2.5 text-xs text-gray-400">
                      {r.cfg.dvr_enabled === -1
                        ? 'app default'
                        : r.cfg.dvr_enabled === 1
                          ? `on${r.cfg.dvr_window_s ? ` · ${r.cfg.dvr_window_s}s` : ''}`
                          : 'off'}
                    </td>
                    <td className="px-4 py-2.5 text-xs">
                      {r.cfg.is_wildcard ? (
                        <span className="text-gray-500">matches {r.cfg.name_pattern}</span>
                      ) : (
                        <div className="space-y-0.5 font-mono">
                          <div className="text-gray-400">
                            rtmp://&lt;host&gt;/{r.app.name}/{r.cfg.name_pattern}
                          </div>
                          <button
                            type="button"
                            onClick={() => copy(play, id)}
                            className="inline-flex items-center gap-1 text-blue-400 hover:text-blue-300"
                            title={play}
                          >
                            {copied === id ? <Check size={11} /> : <Copy size={11} />}
                            /{r.app.name}/{r.cfg.name_pattern}/playlist.m3u8
                          </button>
                        </div>
                      )}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <div className="inline-flex gap-1.5">
                        <button
                          type="button"
                          onClick={() => setModal({ open: true, edit: r })}
                          className="px-2 py-1 rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                          title="Edit"
                        >
                          <Pencil size={12} />
                        </button>
                        <button
                          type="button"
                          onClick={() => setDel(r)}
                          className="px-2 py-1 rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
                          title="Delete"
                        >
                          <Trash2 size={12} />
                        </button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <StreamConfigModal
        open={modal.open}
        app={modal.edit?.app ?? null}
        edit={modal.edit?.cfg ?? null}
        onClose={() => setModal({ open: false, edit: null })}
        onSaved={() => {
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
          setDel(null)
          mutate()
        }}
      />
    </div>
  )
}
