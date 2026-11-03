'use client'

import { useState } from 'react'
import useSWR from 'swr'
import {
  Plus,
  Trash2,
  Pencil,
  ChevronDown,
  ChevronRight,
  AppWindow,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { StreamConfigModal } from '@/components/StreamConfigModal'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import { listenPort } from '@/lib/utils'
import {
  listApplications,
  createApplication,
  updateApplication,
  deleteApplication,
  listStreamConfigs,
  deleteStreamConfig,
  getNodeInfo,
  Application,
  StreamConfig,
} from '@/lib/api'

export default function ApplicationsPage() {
  return (
    <Layout>
      <ApplicationsContent />
    </Layout>
  )
}

// ---------- helpers ----------

function maskKey(key: string): string {
  if (!key) return '—'
  if (key.length <= 4) return '••••'
  return key.slice(0, 3) + '•'.repeat(Math.min(key.length - 3, 8))
}

function protocolChips(app: Application) {
  const chips: { label: string; on: boolean; color: string }[] = [
    { label: 'HLS', on: app.out_hls, color: 'blue' },
    { label: 'DASH', on: app.out_dash, color: 'purple' },
    { label: 'MPEG-TS', on: app.out_mpegts, color: 'orange' },
    { label: 'LL-HLS', on: app.out_ll_hls, color: 'green' },
    { label: 'DVB-DASH', on: app.dvb_dash_enabled, color: 'purple' },
    { label: 'EBU-TT-D', on: app.dvb_subtitles_enabled, color: 'green' },
    { label: 'nDVR', on: app.dvr_enabled, color: 'orange' },
  ]
  return chips.filter((c) => c.on)
}

type TriState = -1 | 0 | 1

function triLabel(v: TriState, inheritLabel: string): string {
  if (v === -1) return inheritLabel
  if (v === 1) return 'Enabled'
  return 'Disabled'
}

// ---------- main content ----------

function ApplicationsContent() {
  const { data: apps, mutate: refreshApps } = useSWR(
    'applications',
    listApplications,
    { refreshInterval: 15000 },
  )

  const [appModalOpen, setAppModalOpen] = useState(false)
  const [editApp, setEditApp] = useState<Application | null>(null)
  const [deleteAppTarget, setDeleteAppTarget] = useState<Application | null>(null)
  const [expandedAppId, setExpandedAppId] = useState<string | null>(null)

  const [streamModalApp, setStreamModalApp] = useState<Application | null>(null)
  const [editStream, setEditStream] = useState<StreamConfig | null>(null)
  const [deleteStreamTarget, setDeleteStreamTarget] = useState<{
    app: Application
    stream: StreamConfig
  } | null>(null)

  const toggleExpand = (appId: string) => {
    setExpandedAppId((prev) => (prev === appId ? null : appId))
  }

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold text-white">Applications</h1>
          <p className="text-sm text-gray-500 mt-1">
            Manage publishing applications, output protocols, and per-stream configs
          </p>
        </div>
        <button
          type="button"
          onClick={() => { setEditApp(null); setAppModalOpen(true) }}
          className="inline-flex items-center gap-1.5 px-3 py-2 text-sm rounded bg-blue-600 hover:bg-blue-500 text-white transition flex-shrink-0"
        >
          <Plus size={16} />
          New Application
        </button>
      </div>

      {!apps || apps.length === 0 ? (
        <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
          <AppWindow className="mx-auto text-gray-600 mb-3" size={32} />
          <div className="text-sm text-gray-400">No applications yet</div>
          <div className="text-xs text-gray-500 mt-1">
            Create an application to start accepting RTMP publish streams
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-hidden">
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="bg-gray-800/40 text-xs uppercase tracking-wider text-gray-500">
                <tr>
                  <th className="px-4 py-2 text-left font-medium w-8"></th>
                  <th className="px-4 py-2 text-left font-medium">Name</th>
                  <th className="px-4 py-2 text-left font-medium">Status</th>
                  <th className="px-4 py-2 text-left font-medium">Publish Key</th>
                  <th className="px-4 py-2 text-left font-medium">Allow Any Stream</th>
                  <th className="px-4 py-2 text-left font-medium">Output Protocols</th>
                  <th className="px-4 py-2 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-800">
                {apps.map((app) => (
                  <AppRows
                    key={app.id}
                    app={app}
                    expanded={expandedAppId === app.id}
                    onToggleExpand={() => toggleExpand(app.id)}
                    onEdit={() => { setEditApp(app); setAppModalOpen(true) }}
                    onDelete={() => setDeleteAppTarget(app)}
                    onAddStream={() => { setStreamModalApp(app); setEditStream(null) }}
                    onEditStream={(s) => { setStreamModalApp(app); setEditStream(s) }}
                    onDeleteStream={(s) => setDeleteStreamTarget({ app, stream: s })}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Application create/edit modal */}
      <AppModal
        open={appModalOpen}
        edit={editApp}
        onClose={() => setAppModalOpen(false)}
        onSaved={() => { setAppModalOpen(false); refreshApps() }}
      />

      {/* Delete application confirm */}
      <ConfirmDelete
        open={!!deleteAppTarget}
        title="Delete application"
        message={`Delete application "${deleteAppTarget?.name ?? ''}"? All stream configs for this application will also be removed.`}
        onCancel={() => setDeleteAppTarget(null)}
        onConfirm={async () => {
          if (!deleteAppTarget) return
          await deleteApplication(deleteAppTarget.id)
          if (expandedAppId === deleteAppTarget.id) setExpandedAppId(null)
          setDeleteAppTarget(null)
          refreshApps()
        }}
      />

      {/* Stream config create/edit modal */}
      {streamModalApp && (
        <StreamConfigModal
          open={!!streamModalApp}
          app={streamModalApp}
          edit={editStream}
          onClose={() => { setStreamModalApp(null); setEditStream(null) }}
          onSaved={() => { setStreamModalApp(null); setEditStream(null); refreshApps() }}
        />
      )}

      {/* Delete stream config confirm */}
      <ConfirmDelete
        open={!!deleteStreamTarget}
        title="Delete stream config"
        message={`Delete stream config "${deleteStreamTarget?.stream.name_pattern ?? ''}"?`}
        onCancel={() => setDeleteStreamTarget(null)}
        onConfirm={async () => {
          if (!deleteStreamTarget) return
          await deleteStreamConfig(
            deleteStreamTarget.app.id,
            deleteStreamTarget.stream.id,
          )
          setDeleteStreamTarget(null)
          refreshApps()
        }}
      />
    </div>
  )
}

// ---------- App row + inline stream sub-table ----------

function AppRows({
  app,
  expanded,
  onToggleExpand,
  onEdit,
  onDelete,
  onAddStream,
  onEditStream,
  onDeleteStream,
}: {
  app: Application
  expanded: boolean
  onToggleExpand: () => void
  onEdit: () => void
  onDelete: () => void
  onAddStream: () => void
  onEditStream: (s: StreamConfig) => void
  onDeleteStream: (s: StreamConfig) => void
}) {
  const chips = protocolChips(app)

  const { data: streams } = useSWR(
    expanded ? ['stream-configs', app.id] : null,
    () => listStreamConfigs(app.id),
    { refreshInterval: 10000 },
  )

  return (
    <>
      <tr className="hover:bg-gray-800/30 transition">
        {/* expand toggle */}
        <td className="px-4 py-2.5">
          <button
            type="button"
            onClick={onToggleExpand}
            className="text-gray-400 hover:text-white transition"
            title={expanded ? 'Collapse streams' : 'Expand streams'}
          >
            {expanded ? <ChevronDown size={16} /> : <ChevronRight size={16} />}
          </button>
        </td>
        <td className="px-4 py-2.5 text-white font-medium">{app.name}</td>
        <td className="px-4 py-2.5">
          <Badge variant={app.enabled ? 'green' : 'gray'}>
            {app.enabled ? 'Enabled' : 'Disabled'}
          </Badge>
        </td>
        <td className="px-4 py-2.5 font-mono text-xs text-gray-300">
          {maskKey(app.publish_key)}
        </td>
        <td className="px-4 py-2.5">
          <Badge variant={app.allow_any_stream ? 'blue' : 'yellow'}>
            {app.allow_any_stream ? 'Yes' : 'No'}
          </Badge>
        </td>
        <td className="px-4 py-2.5">
          {chips.length === 0 ? (
            <span className="text-gray-600 text-xs">none</span>
          ) : (
            <div className="flex flex-wrap gap-1">
              {chips.map((c) => (
                <Badge key={c.label} variant={c.color as any}>
                  {c.label}
                </Badge>
              ))}
            </div>
          )}
        </td>
        <td className="px-4 py-2.5 text-right">
          <div className="inline-flex gap-1.5">
            <button
              type="button"
              onClick={onEdit}
              className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
            >
              <Pencil size={12} />
              Edit
            </button>
            <button
              type="button"
              onClick={onDelete}
              className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
            >
              <Trash2 size={12} />
              Delete
            </button>
          </div>
        </td>
      </tr>

      {/* Inline stream configs sub-table */}
      {expanded && (
        <tr>
          <td colSpan={7} className="px-0 py-0 bg-gray-950/60">
            <div className="border-t border-gray-800 border-b border-gray-800">
              <div className="flex items-center justify-between px-8 py-2.5 bg-gray-800/20">
                <span className="text-xs font-medium text-gray-400 uppercase tracking-wider">
                  Stream Configs — {app.name}
                </span>
                <button
                  type="button"
                  onClick={onAddStream}
                  className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-blue-600 hover:bg-blue-500 text-white transition"
                >
                  <Plus size={12} />
                  Add Stream Config
                </button>
              </div>

              {!streams || streams.length === 0 ? (
                <div className="px-8 py-6 text-sm text-gray-500 text-center">
                  No stream configs.{' '}
                  {app.allow_any_stream
                    ? 'Any stream can publish (allow_any_stream is on).'
                    : 'Add a config to allow specific streams to publish.'}
                </div>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-xs">
                    <thead className="bg-gray-800/20 text-gray-600 uppercase tracking-wider">
                      <tr>
                        <th className="pl-8 pr-4 py-2 text-left font-medium">Pattern</th>
                        <th className="px-4 py-2 text-left font-medium">Wildcard</th>
                        <th className="px-4 py-2 text-left font-medium">Protocols</th>
                        <th className="px-4 py-2 text-left font-medium">DVR</th>
                        <th className="px-4 py-2 text-left font-medium">Status</th>
                        <th className="px-4 py-2 text-right font-medium pr-6">Actions</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-gray-800/60">
                      {streams.map((s) => (
                        <StreamConfigRow
                          key={s.id}
                          stream={s}
                          onEdit={() => onEditStream(s)}
                          onDelete={() => onDeleteStream(s)}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

// ---------- Stream config row ----------

function StreamConfigRow({
  stream,
  onEdit,
  onDelete,
}: {
  stream: StreamConfig
  onEdit: () => void
  onDelete: () => void
}) {
  const protoStr = (['HLS', 'DASH', 'MPEG-TS', 'LL-HLS'] as const)
    .map((label, i) => {
      const v = [stream.out_hls, stream.out_dash, stream.out_mpegts, stream.out_ll_hls][i]
      if (v === -1) return `${label}:inherit`
      if (v === 1) return `${label}:on`
      return `${label}:off`
    })
    .filter((x) => !x.endsWith(':inherit') || stream.out_hls === -1) // show all

  const dvrLabel =
    stream.dvr_enabled === -1
      ? 'DVR: inherit'
      : stream.dvr_enabled === 1
        ? `DVR: on${stream.dvr_window_s ? ` (${stream.dvr_window_s}s)` : ''}`
        : 'DVR: off'

  // Protocol summary chips
  const protoChips: { label: string; v: number }[] = [
    { label: 'HLS', v: stream.out_hls },
    { label: 'DASH', v: stream.out_dash },
    { label: 'MPEG-TS', v: stream.out_mpegts },
    { label: 'LL-HLS', v: stream.out_ll_hls },
    { label: 'DVB-DASH', v: stream.dvb_dash_enabled },
    { label: 'EBU-TT-D', v: stream.dvb_subtitles_enabled },
  ]

  return (
    <tr className="hover:bg-gray-800/20 transition">
      <td className="pl-8 pr-4 py-2 font-mono text-gray-200">{stream.name_pattern}</td>
      <td className="px-4 py-2">
        {stream.is_wildcard ? (
          <Badge variant="purple">wildcard</Badge>
        ) : (
          <Badge variant="gray">exact</Badge>
        )}
      </td>
      <td className="px-4 py-2">
        <div className="flex flex-wrap gap-1">
          {protoChips.map(({ label, v }) => (
            <span
              key={label}
              className={
                'inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-medium border ' +
                (v === 1
                  ? 'bg-blue-500/15 text-blue-400 border-blue-500/30'
                  : v === 0
                    ? 'bg-gray-700/30 text-gray-500 border-gray-700/30 line-through'
                    : 'bg-gray-700/20 text-gray-600 border-gray-700/20 italic')
              }
            >
              {label}
            </span>
          ))}
        </div>
      </td>
      <td className="px-4 py-2 text-gray-400">{dvrLabel}</td>
      <td className="px-4 py-2">
        <Badge variant={stream.enabled ? 'green' : 'gray'}>
          {stream.enabled ? 'on' : 'off'}
        </Badge>
      </td>
      <td className="px-4 py-2 text-right pr-6">
        <div className="inline-flex gap-1.5">
          <button
            type="button"
            onClick={onEdit}
            className="px-1.5 py-1 rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
          >
            <Pencil size={11} />
          </button>
          <button
            type="button"
            onClick={onDelete}
            className="px-1.5 py-1 rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
          >
            <Trash2 size={11} />
          </button>
        </div>
      </td>
    </tr>
  )
}

// ---------- Application create/edit modal ----------

const defaultAppForm = {
  name: '',
  publish_key: '',
  push_login: '',
  push_password: '',
  enabled: true,
  allow_any_stream: true,
  out_hls: true,
  out_dash: false,
  out_mpegts: false,
  out_ll_hls: false,
  dvb_dash_enabled: false,
  dvb_subtitles_enabled: false,
  hls_seg_duration_s: 0,
  hls_playlist_window: 0,
  scte35_mode: 'disabled',
  ad_marker_format: 'none',
  dvr_enabled: false,
  dvr_window_s: 0,
}

function AppModal({
  open,
  edit,
  onClose,
  onSaved,
}: {
  open: boolean
  edit: Application | null
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState(defaultAppForm)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  // Shares the 'node' SWR cache key with Dashboard/Settings — no extra request.
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 30000 })

  useStateReset(JSON.stringify({ open, eid: edit?.id ?? null }), () => {
    if (!open) return
    if (edit) {
      setForm({
        name: edit.name,
        publish_key: edit.publish_key ?? '',
        push_login: edit.push_login ?? '',
        push_password: '',
        enabled: edit.enabled,
        allow_any_stream: edit.allow_any_stream,
        out_hls: edit.out_hls,
        out_dash: edit.out_dash,
        out_mpegts: edit.out_mpegts,
        out_ll_hls: edit.out_ll_hls,
        dvb_dash_enabled: edit.dvb_dash_enabled,
        dvb_subtitles_enabled: edit.dvb_subtitles_enabled,
        hls_seg_duration_s: edit.hls_seg_duration_s ?? 0,
        hls_playlist_window: edit.hls_playlist_window ?? 0,
        scte35_mode: edit.scte35_mode || 'disabled',
        ad_marker_format: edit.ad_marker_format || 'none',
        dvr_enabled: edit.dvr_enabled ?? false,
        dvr_window_s: edit.dvr_window_s ?? 0,
      })
    } else {
      setForm(defaultAppForm)
    }
    setErr(null)
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      // An empty password field on edit means "keep the current one".
      const { push_password, ...rest } = form
      const data = push_password || !edit ? form : rest
      if (edit) {
        await updateApplication(edit.id, data)
      } else {
        await createApplication(data)
      }
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
      title={edit ? 'Edit Application' : 'New Application'}
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-4">
        {/* Basic info */}
        <div className="grid grid-cols-2 gap-3">
          <FormField label="App Name">
            <input
              required
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              className={inputClass}
              placeholder="live"
            />
          </FormField>
          <FormField label="Publish Key" hint="Leave blank for no key">
            <input
              value={form.publish_key}
              onChange={(e) => setForm({ ...form, publish_key: e.target.value })}
              className={inputClass}
              placeholder="leave blank for no key"
            />
          </FormField>
          <FormField label="Push Login" hint="RTMP username; leave blank for no login">
            <input
              value={form.push_login}
              onChange={(e) => setForm({ ...form, push_login: e.target.value })}
              className={inputClass}
              placeholder="no login"
              autoComplete="off"
            />
          </FormField>
          <FormField
            label="Push Password"
            hint={
              edit?.has_push_password
                ? 'Set. Leave blank to keep the current password'
                : 'Encoder password (FMLE prompts for it)'
            }
          >
            <input
              type="password"
              value={form.push_password}
              onChange={(e) => setForm({ ...form, push_password: e.target.value })}
              className={inputClass}
              placeholder={edit?.has_push_password ? '••••••••' : ''}
              autoComplete="new-password"
              required={!!form.push_login && !edit?.has_push_password}
            />
          </FormField>
        </div>

        {/* Publish URLs — informational, derived from the app name above */}
        <div className="rounded border border-gray-700 bg-gray-800/40 px-3 py-2 space-y-1">
          <div className="text-xs uppercase tracking-wider text-gray-500">
            Publish via
          </div>
          <div className="text-xs font-mono text-gray-300 break-all">
            RTMP: rtmp://&lt;host&gt;{listenPort(node?.rtmp_listen, ':1935')}/
            {form.name || '<app>'}/&lt;stream&gt;
          </div>
          {node?.srt?.enabled && (
            <div className="text-xs font-mono text-gray-300 break-all">
              SRT: srt://&lt;host&gt;{listenPort(node?.srt?.listen, ':4000')}?streamid=
              {form.name || '<app>'}/&lt;stream&gt;
            </div>
          )}
        </div>

        {/* Checkboxes */}
        <div className="space-y-2">
          <label className="flex items-center gap-2 text-sm text-gray-300">
            <input
              type="checkbox"
              checked={form.enabled}
              onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
            />
            Enabled
          </label>
          <label className="flex items-start gap-2 text-sm text-gray-300">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={form.allow_any_stream}
              onChange={(e) => setForm({ ...form, allow_any_stream: e.target.checked })}
            />
            <span>
              Allow Any Stream{' '}
              <span className="text-gray-500 text-xs">
                — if unchecked, only streams defined in stream configs can publish
              </span>
            </span>
          </label>
        </div>

        {/* Output protocols */}
        <div>
          <div className="text-xs uppercase tracking-wider text-gray-500 mb-2">
            Output Protocols
          </div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            {(
              [
                { key: 'out_hls', label: 'HLS' },
                { key: 'out_dash', label: 'DASH' },
                { key: 'out_mpegts', label: 'MPEG-TS' },
                { key: 'out_ll_hls', label: 'LL-HLS' },
                { key: 'dvb_dash_enabled', label: 'DVB-DASH Profile' },
                { key: 'dvb_subtitles_enabled', label: 'EBU-TT-D Subtitles' },
              ] as const
            ).map(({ key, label }) => (
              <label
                key={key}
                className="flex items-center gap-2 text-sm text-gray-300 px-3 py-2 rounded border border-gray-700 bg-gray-800/40 cursor-pointer hover:border-gray-600 transition"
              >
                <input
                  type="checkbox"
                  checked={form[key]}
                  onChange={(e) => setForm({ ...form, [key]: e.target.checked })}
                />
                {label}
              </label>
            ))}
          </div>
        </div>

        {/* SCTE-35 */}
        <div>
          <div className="text-xs uppercase tracking-wider text-gray-500 mb-2">
            SCTE-35
          </div>
          <div className="grid grid-cols-2 gap-3">
            <FormField
              label="Mode"
              hint="Passthrough forwards markers already in the source; Insert allows server-triggered breaks with no source markers; Hybrid does both"
            >
              <select
                value={form.scte35_mode}
                onChange={(e) => setForm({ ...form, scte35_mode: e.target.value })}
                className={inputClass}
              >
                <option value="disabled">Disabled</option>
                <option value="passthrough">Passthrough</option>
                <option value="insert">Insert</option>
                <option value="hybrid">Hybrid</option>
              </select>
            </FormField>
            <FormField label="Ad marker format">
              <select
                value={form.ad_marker_format}
                onChange={(e) =>
                  setForm({ ...form, ad_marker_format: e.target.value })
                }
                className={inputClass}
                disabled={form.scte35_mode === 'disabled'}
              >
                <option value="none">None</option>
                <option value="cue">CUE-OUT/-IN/-CONT</option>
                <option value="splicepoint">SPLICEPOINT-SCTE35</option>
                <option value="daterange">DATERANGE</option>
              </select>
            </FormField>
          </div>
        </div>

        {/* nDVR */}
        <div>
          <div className="text-xs uppercase tracking-wider text-gray-500 mb-2">
            nDVR (rewind)
          </div>
          <div className="grid grid-cols-2 gap-3">
            <label className="flex items-start gap-2 text-sm text-gray-300 px-3 py-2 rounded border border-gray-700 bg-gray-800/40 cursor-pointer hover:border-gray-600 transition">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={form.dvr_enabled}
                onChange={(e) => setForm({ ...form, dvr_enabled: e.target.checked })}
              />
              <span>
                Record &amp; allow rewind
                <span className="block text-gray-500 text-xs">
                  One fMP4 recording serves both HLS and DASH rewind
                </span>
              </span>
            </label>
            <FormField label="Rewind window (seconds)" hint="0 = global DVR window">
              <input
                type="number"
                min={0}
                value={form.dvr_window_s}
                onChange={(e) => setForm({ ...form, dvr_window_s: Number(e.target.value) })}
                className={inputClass}
                disabled={!form.dvr_enabled}
              />
            </FormField>
          </div>
          {form.dvr_enabled && (
            <div className="mt-2 rounded border border-gray-700 bg-gray-800/40 px-3 py-2 space-y-1 text-xs font-mono text-gray-300 break-all">
              <div>/{form.name || '<app>'}/&lt;stream&gt;/playlist_dvr.m3u8</div>
              <div>/{form.name || '<app>'}/&lt;stream&gt;/manifest_dvr.mpd</div>
            </div>
          )}
        </div>

        {/* HLS overrides */}
        <div>
          <div className="text-xs uppercase tracking-wider text-gray-500 mb-2">
            HLS Override{' '}
            <span className="normal-case text-gray-600 text-[10px]">
              (0 = use global defaults)
            </span>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <FormField label="Segment Duration (s)">
              <input
                type="number"
                min={0}
                value={form.hls_seg_duration_s}
                onChange={(e) =>
                  setForm({ ...form, hls_seg_duration_s: Number(e.target.value) })
                }
                className={inputClass}
              />
            </FormField>
            <FormField label="Playlist Window (segments)">
              <input
                type="number"
                min={0}
                value={form.hls_playlist_window}
                onChange={(e) =>
                  setForm({ ...form, hls_playlist_window: Number(e.target.value) })
                }
                className={inputClass}
              />
            </FormField>
          </div>
        </div>

        <ErrorBox msg={err} />
        <ModalActions
          busy={busy}
          onCancel={onClose}
          submitLabel={edit ? 'Save' : 'Create'}
        />
      </form>
    </Modal>
  )
}
