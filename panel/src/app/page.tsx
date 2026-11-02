'use client'

import { useState } from 'react'
import useSWR from 'swr'
import {
  Radio,
  Layers,
  Eye,
  Activity,
  ExternalLink,
  AlertTriangle,
  Loader2,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { StatCard } from '@/components/StatCard'
import { Badge } from '@/components/Badge'
import { Modal } from '@/components/Modal'
import {
  getNodeInfo,
  listStreams,
  listABRGroups,
  kickStream,
  Stream,
} from '@/lib/api'
import { formatBytes, formatUptime } from '@/lib/utils'

export default function DashboardPage() {
  return (
    <Layout>
      <DashboardContent />
    </Layout>
  )
}

function DashboardContent() {
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 5000 })
  const { data: streams, mutate: refresh } = useSWR('streams', listStreams, {
    refreshInterval: 3000,
  })
  const { data: groups } = useSWR('abr-groups', listABRGroups, {
    refreshInterval: 10000,
  })

  const [kickTarget, setKickTarget] = useState<Stream | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const list = streams ?? []
  const totalBytesIn = list.reduce((s, st) => s + (st.bytes_in || 0), 0)
  const totalSubs = list.reduce((s, st) => s + (st.subscribers || 0), 0)

  const doKick = async () => {
    if (!kickTarget) return
    setBusy(true)
    setErr(null)
    try {
      await kickStream(kickTarget.app, kickTarget.name)
      setKickTarget(null)
      refresh()
    } catch (e: any) {
      setErr(e?.message ?? 'Failed to kick stream')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-6 max-w-7xl">
      <div>
        <h1 className="text-2xl font-semibold text-white">
          {node?.node_name ?? 'Dashboard'}
        </h1>
        <p className="text-sm text-gray-500 mt-1">
          Origin Media Server v{node?.version ?? '—'}
        </p>
      </div>

      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <StatCard
          label="Live Streams"
          value={list.length}
          color="green"
          icon={<Radio size={18} />}
        />
        <StatCard
          label="ABR Groups"
          value={groups?.length ?? 0}
          color="blue"
          icon={<Layers size={18} />}
        />
        <StatCard
          label="Total Viewers"
          value={totalSubs}
          color="purple"
          icon={<Eye size={18} />}
        />
        <StatCard
          label="Bytes In"
          value={formatBytes(totalBytesIn)}
          color="yellow"
          icon={<Activity size={18} />}
        />
      </div>

      <div>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-base font-medium text-gray-200">Live Streams</h2>
          {list.length > 0 && (
            <span className="text-xs text-gray-500">
              Auto-refresh 3s · {list.length} active
            </span>
          )}
        </div>

        {list.length === 0 ? (
          <div className="rounded-lg border border-gray-800 bg-gray-900 p-8 text-center">
            <Radio className="mx-auto text-gray-600 mb-2" size={28} />
            <div className="text-gray-400 text-sm font-medium">No active streams</div>
            <div className="text-xs text-gray-500 mt-2">
              Publish via RTMP:
            </div>
            <code className="inline-block mt-2 px-3 py-1.5 rounded bg-gray-800 text-gray-300 text-xs font-mono border border-gray-700">
              rtmp://&lt;host&gt;{node?.rtmp_listen ?? ':1935'}/live/&lt;stream-key&gt;
            </code>
            {node?.srt?.enabled && (
              <>
                <div className="text-xs text-gray-500 mt-3">Or via SRT:</div>
                <code className="inline-block mt-2 px-3 py-1.5 rounded bg-gray-800 text-gray-300 text-xs font-mono border border-gray-700">
                  srt://&lt;host&gt;{node?.srt?.listen ?? ':4000'}?streamid=live/&lt;stream-key&gt;
                </code>
              </>
            )}
          </div>
        ) : (
          <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="bg-gray-800/50 text-gray-400 text-xs uppercase tracking-wider">
                <tr>
                  <th className="px-4 py-2 text-left font-medium">Stream</th>
                  <th className="px-4 py-2 text-left font-medium">Source</th>
                  <th className="px-4 py-2 text-left font-medium">Uptime</th>
                  <th className="px-4 py-2 text-left font-medium">Viewers</th>
                  <th className="px-4 py-2 text-left font-medium">Bytes In</th>
                  <th className="px-4 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-800">
                {list.map((s) => (
                  <tr key={s.id} className="hover:bg-gray-800/30 transition">
                    <td className="px-4 py-3">
                      <div className="flex items-center gap-2">
                        <span className="relative flex h-2 w-2">
                          <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-60" />
                          <span className="relative inline-flex rounded-full h-2 w-2 bg-green-500" />
                        </span>
                        <span className="text-white font-medium">
                          {s.app}/{s.name}
                        </span>
                      </div>
                    </td>
                    <td className="px-4 py-3">
                      <Badge variant="blue">{s.source_type || '—'}</Badge>
                    </td>
                    <td className="px-4 py-3 text-gray-300">
                      {formatUptime(s.uptime_s)}
                    </td>
                    <td className="px-4 py-3 text-gray-300">{s.subscribers}</td>
                    <td className="px-4 py-3 text-gray-300">
                      {formatBytes(s.bytes_in)}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <div className="inline-flex gap-1.5">
                        <a
                          href={`/${s.app}/${s.name}/playlist.m3u8`}
                          target="_blank"
                          rel="noreferrer"
                          className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                        >
                          <ExternalLink size={12} />
                          HLS
                        </a>
                        <button
                          type="button"
                          onClick={() => setKickTarget(s)}
                          className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
                        >
                          Kick
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <Modal
        open={!!kickTarget}
        onClose={() => {
          if (!busy) {
            setKickTarget(null)
            setErr(null)
          }
        }}
        title="Kick stream"
      >
        <div className="space-y-3">
          <div className="flex gap-2 text-sm text-gray-300">
            <AlertTriangle className="text-yellow-400 flex-shrink-0" size={18} />
            <div>
              Disconnect publisher for
              <span className="text-white font-medium ml-1">
                {kickTarget?.app}/{kickTarget?.name}
              </span>
              ? Subscribers will be disconnected.
            </div>
          </div>
          {err && (
            <div className="text-sm text-red-400 bg-red-500/10 border border-red-500/30 rounded px-3 py-2">
              {err}
            </div>
          )}
          <div className="flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={() => setKickTarget(null)}
              disabled={busy}
              className="px-3 py-1.5 text-sm rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={doKick}
              disabled={busy}
              className="px-3 py-1.5 text-sm rounded bg-red-600 hover:bg-red-500 disabled:opacity-50 text-white flex items-center gap-2 transition"
            >
              {busy && <Loader2 size={14} className="animate-spin" />}
              Kick
            </button>
          </div>
        </div>
      </Modal>
    </div>
  )
}
