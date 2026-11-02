'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Plus, Trash2, Pencil, Play, Square, Download, Lock } from 'lucide-react'
import { Layout } from '@/components/Layout'
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
import {
  listPullSources,
  createPullSource,
  updatePullSource,
  deletePullSource,
  startPull,
  stopPull,
  PullSource,
} from '@/lib/api'

export default function PullPage() {
  return (
    <Layout>
      <PullContent />
    </Layout>
  )
}

function PullContent() {
  const { data, mutate } = useSWR('pull-sources', listPullSources, {
    refreshInterval: 5000,
  })
  const [newOpen, setNewOpen] = useState(false)
  const [editing, setEditing] = useState<PullSource | null>(null)
  const [deleting, setDeleting] = useState<PullSource | null>(null)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [opErr, setOpErr] = useState<string | null>(null)

  const sources = data ?? []

  const op = async (id: string, fn: () => Promise<void>) => {
    setBusyId(id)
    setOpErr(null)
    try {
      await fn()
      mutate()
    } catch (e: any) {
      setOpErr(e?.message ?? 'Operation failed')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-white">Pull Sources</h1>
          <p className="text-sm text-gray-500 mt-1">
            Ingest remote streams into this server
          </p>
        </div>
        <button
          type="button"
          onClick={() => setNewOpen(true)}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded bg-blue-600 hover:bg-blue-500 text-white transition"
        >
          <Plus size={16} />
          New Source
        </button>
      </div>

      {opErr && <ErrorBox msg={opErr} />}

      {sources.length === 0 ? (
        <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
          <Download className="mx-auto text-gray-600 mb-3" size={32} />
          <div className="text-sm text-gray-400">No pull sources configured</div>
          <div className="text-xs text-gray-500 mt-1">
            Pull a remote RTMP / RTSP / SRT / HLS feed into a local app
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-gray-800/50 text-xs uppercase tracking-wider text-gray-500">
              <tr>
                <th className="px-4 py-2 text-left font-medium">App</th>
                <th className="px-4 py-2 text-left font-medium">Stream</th>
                <th className="px-4 py-2 text-left font-medium">URL</th>
                <th className="px-4 py-2 text-left font-medium">Protocol</th>
                <th className="px-4 py-2 text-left font-medium">Status</th>
                <th className="px-4 py-2 text-left font-medium">On Demand</th>
                <th className="px-4 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800">
              {sources.map((s) => {
                const isActive = s.status === 'active'
                return (
                  <tr key={s.id} className="hover:bg-gray-800/30 transition">
                    <td className="px-4 py-2.5 text-gray-300">{s.app_name}</td>
                    <td className="px-4 py-2.5 text-white font-medium">
                      {s.stream_name}
                    </td>
                    <td className="px-4 py-2.5 text-gray-300 text-xs font-mono truncate max-w-[240px]">
                      {s.url}
                    </td>
                    <td className="px-4 py-2.5">
                      <div className="inline-flex items-center gap-1.5">
                        <Badge variant="blue">{s.protocol}</Badge>
                        {s.protocol === 'srt' && s.has_passphrase && (
                          <Lock size={11} className="text-gray-500" />
                        )}
                      </div>
                    </td>
                    <td className="px-4 py-2.5">
                      {isActive ? (
                        <Badge variant="green">active</Badge>
                      ) : (
                        <Badge variant="gray">{s.status || 'inactive'}</Badge>
                      )}
                    </td>
                    <td className="px-4 py-2.5">
                      {s.on_demand ? (
                        <Badge variant="purple">on-demand</Badge>
                      ) : (
                        <Badge variant="gray">always</Badge>
                      )}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <div className="inline-flex gap-1.5">
                        {isActive ? (
                          <button
                            type="button"
                            disabled={busyId === s.id}
                            onClick={() => op(s.id, () => stopPull(s.id))}
                            className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-yellow-500/20 hover:bg-yellow-500/30 text-yellow-400 border border-yellow-500/40 disabled:opacity-50 transition"
                          >
                            <Square size={11} />
                            Stop
                          </button>
                        ) : (
                          <button
                            type="button"
                            disabled={busyId === s.id}
                            onClick={() => op(s.id, () => startPull(s.id))}
                            className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-green-500/20 hover:bg-green-500/30 text-green-400 border border-green-500/40 disabled:opacity-50 transition"
                          >
                            <Play size={11} />
                            Start
                          </button>
                        )}
                        <button
                          type="button"
                          onClick={() => setEditing(s)}
                          className="px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                        >
                          <Pencil size={12} />
                        </button>
                        <button
                          type="button"
                          onClick={() => setDeleting(s)}
                          className="px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
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

      <PullModal
        open={newOpen}
        edit={null}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          mutate()
        }}
      />
      <PullModal
        open={!!editing}
        edit={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          mutate()
        }}
      />

      <ConfirmDelete
        open={!!deleting}
        title="Delete pull source"
        message={`Delete pull source for "${deleting?.app_name ?? ''}/${deleting?.stream_name ?? ''}"?`}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return
          await deletePullSource(deleting.id)
          setDeleting(null)
          mutate()
        }}
      />
    </div>
  )
}

function PullModal({
  open,
  edit,
  onClose,
  onSaved,
}: {
  open: boolean
  edit: PullSource | null
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState({
    app_name: 'live',
    stream_name: '',
    url: '',
    protocol: 'rtmp',
    on_demand: false,
    reconnect: true,
    reconnect_delay_s: 5,
    passphrase: '',
    pbkeylen: 0,
  })
  const [clearPassphrase, setClearPassphrase] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(JSON.stringify({ open, eid: edit?.id ?? null }), () => {
    if (!open) return
    if (edit) {
      setForm({
        app_name: edit.app_name,
        stream_name: edit.stream_name,
        url: edit.url,
        protocol: edit.protocol,
        on_demand: edit.on_demand,
        reconnect: edit.reconnect,
        reconnect_delay_s: edit.reconnect_delay_s,
        passphrase: '',
        pbkeylen: edit.pbkeylen ?? 0,
      })
    } else {
      setForm({
        app_name: 'live',
        stream_name: '',
        url: '',
        protocol: 'rtmp',
        on_demand: false,
        reconnect: true,
        reconnect_delay_s: 5,
        passphrase: '',
        pbkeylen: 0,
      })
    }
    setClearPassphrase(false)
    setErr(null)
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      if (edit) {
        await updatePullSource(edit.id, { ...form, clear_passphrase: clearPassphrase })
      } else {
        await createPullSource(form)
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
      title={edit ? 'Edit pull source' : 'New pull source'}
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-3">
        <div className="grid grid-cols-2 gap-3">
          <FormField label="App name">
            <input
              required
              value={form.app_name}
              onChange={(e) => setForm({ ...form, app_name: e.target.value })}
              className={inputClass}
              placeholder="live"
            />
          </FormField>
          <FormField label="Stream name">
            <input
              required
              value={form.stream_name}
              onChange={(e) =>
                setForm({ ...form, stream_name: e.target.value })
              }
              className={inputClass}
              placeholder="cam1"
            />
          </FormField>
        </div>
        <FormField label="Source URL">
          <input
            required
            value={form.url}
            onChange={(e) => setForm({ ...form, url: e.target.value })}
            className={inputClass}
            placeholder="rtsp://camera.example.com/stream"
          />
        </FormField>
        <div className="grid grid-cols-2 gap-3">
          <FormField label="Protocol">
            <select
              value={form.protocol}
              onChange={(e) => setForm({ ...form, protocol: e.target.value })}
              className={inputClass}
            >
              <option value="rtmp">RTMP</option>
              <option value="rtsp">RTSP</option>
              <option value="srt">SRT</option>
              <option value="hls">HLS</option>
              <option value="udp">UDP</option>
            </select>
          </FormField>
          <FormField label="Reconnect delay (s)">
            <input
              type="number"
              min={1}
              value={form.reconnect_delay_s}
              onChange={(e) =>
                setForm({ ...form, reconnect_delay_s: Number(e.target.value) })
              }
              className={inputClass}
            />
          </FormField>
        </div>
        {form.protocol === 'srt' && (
          <div className="grid grid-cols-2 gap-3">
            <FormField
              label={
                edit?.has_passphrase
                  ? 'Passphrase (leave blank to keep existing)'
                  : 'Passphrase (optional — for an encrypted upstream)'
              }
            >
              <input
                type="password"
                value={form.passphrase}
                onChange={(e) => {
                  setForm({ ...form, passphrase: e.target.value })
                  if (e.target.value) setClearPassphrase(false)
                }}
                className={inputClass}
                placeholder={
                  edit?.has_passphrase ? '••••••••••' : '10-79 characters'
                }
                minLength={form.passphrase ? 10 : undefined}
                maxLength={79}
              />
              {edit?.has_passphrase && (
                <label className="flex items-center gap-2 text-xs text-gray-400 mt-1.5">
                  <input
                    type="checkbox"
                    checked={clearPassphrase}
                    onChange={(e) => {
                      setClearPassphrase(e.target.checked)
                      if (e.target.checked) setForm({ ...form, passphrase: '' })
                    }}
                  />
                  Remove passphrase (pull unencrypted)
                </label>
              )}
            </FormField>
            <FormField label="Key length (bytes)">
              <select
                value={form.pbkeylen}
                onChange={(e) =>
                  setForm({ ...form, pbkeylen: Number(e.target.value) })
                }
                className={inputClass}
              >
                <option value={0}>Default (16)</option>
                <option value={16}>16</option>
                <option value={24}>24</option>
                <option value={32}>32</option>
              </select>
            </FormField>
          </div>
        )}
        <div className="flex flex-col gap-2">
          <label className="flex items-center gap-2 text-sm text-gray-300">
            <input
              type="checkbox"
              checked={form.on_demand}
              onChange={(e) => setForm({ ...form, on_demand: e.target.checked })}
            />
            On demand (start pull only when a subscriber connects)
          </label>
          <label className="flex items-center gap-2 text-sm text-gray-300">
            <input
              type="checkbox"
              checked={form.reconnect}
              onChange={(e) => setForm({ ...form, reconnect: e.target.checked })}
            />
            Reconnect on failure
          </label>
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
