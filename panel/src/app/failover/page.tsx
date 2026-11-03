'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Plus, Trash2, Pencil, Zap, ShieldAlert } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge, BadgeVariant } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import {
  listFailoverRules,
  createFailoverRule,
  updateFailoverRule,
  deleteFailoverRule,
  triggerFailoverRule,
  FailoverRule,
} from '@/lib/api'

export default function FailoverPage() {
  return (
    <Layout>
      <FailoverContent />
    </Layout>
  )
}

function stateBadge(state: string): { variant: BadgeVariant; label: string } {
  switch (state) {
    case 'active_primary':
      return { variant: 'green', label: 'primary' }
    case 'active_backup':
      return { variant: 'yellow', label: 'backup' }
    case 'idle':
      return { variant: 'gray', label: 'idle' }
    default:
      return { variant: 'gray', label: state || '—' }
  }
}

function FailoverContent() {
  const { data, mutate } = useSWR('failover-rules', listFailoverRules, {
    refreshInterval: 5000,
  })
  const [newOpen, setNewOpen] = useState(false)
  const [editing, setEditing] = useState<FailoverRule | null>(null)
  const [deleting, setDeleting] = useState<FailoverRule | null>(null)
  const [triggering, setTriggering] = useState<FailoverRule | null>(null)

  const rules = data ?? []

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-white">Failover Rules</h1>
          <p className="text-sm text-gray-500 mt-1">
            Automatic switch-over when a primary stream stalls
          </p>
        </div>
        <button
          type="button"
          onClick={() => setNewOpen(true)}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded bg-blue-600 hover:bg-blue-500 text-white transition"
        >
          <Plus size={16} />
          New Rule
        </button>
      </div>

      {rules.length === 0 ? (
        <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
          <ShieldAlert className="mx-auto text-gray-600 mb-3" size={32} />
          <div className="text-sm text-gray-400">No failover rules defined</div>
          <div className="text-xs text-gray-500 mt-1">
            Create a rule to automatically pull a backup feed when the primary
            stalls
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-gray-800/50 text-xs uppercase tracking-wider text-gray-500">
              <tr>
                <th className="px-4 py-2 text-left font-medium">App</th>
                <th className="px-4 py-2 text-left font-medium">Primary</th>
                <th className="px-4 py-2 text-left font-medium">Backup URL</th>
                <th className="px-4 py-2 text-left font-medium">Protocol</th>
                <th className="px-4 py-2 text-left font-medium">Trigger</th>
                <th className="px-4 py-2 text-left font-medium">State</th>
                <th className="px-4 py-2 text-left font-medium">Enabled</th>
                <th className="px-4 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800">
              {rules.map((r) => {
                const sb = stateBadge(r.state)
                return (
                  <tr key={r.id} className="hover:bg-gray-800/30 transition">
                    <td className="px-4 py-2.5 text-white font-medium">
                      {r.app_name}
                    </td>
                    <td className="px-4 py-2.5 text-gray-300">{r.primary_stream}</td>
                    <td className="px-4 py-2.5 text-gray-300 text-xs font-mono truncate max-w-[220px]">
                      {r.backup_url}
                    </td>
                    <td className="px-4 py-2.5">
                      <Badge variant="blue">{r.backup_protocol}</Badge>
                    </td>
                    <td className="px-4 py-2.5 text-gray-300 text-xs">
                      {r.trigger_type}
                    </td>
                    <td className="px-4 py-2.5">
                      <Badge variant={sb.variant}>{sb.label}</Badge>
                    </td>
                    <td className="px-4 py-2.5">
                      {r.enabled ? (
                        <Badge variant="green">on</Badge>
                      ) : (
                        <Badge variant="gray">off</Badge>
                      )}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <div className="inline-flex gap-1.5">
                        <button
                          type="button"
                          onClick={() => setTriggering(r)}
                          className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-yellow-500/20 hover:bg-yellow-500/30 text-yellow-400 border border-yellow-500/40 transition"
                          title="Trigger manually"
                        >
                          <Zap size={12} />
                          Trigger
                        </button>
                        <button
                          type="button"
                          onClick={() => setEditing(r)}
                          className="px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                        >
                          <Pencil size={12} />
                        </button>
                        <button
                          type="button"
                          onClick={() => setDeleting(r)}
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

      <RuleModal
        open={newOpen}
        edit={null}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          mutate()
        }}
      />
      <RuleModal
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
        title="Delete failover rule"
        message={`Delete failover rule for "${deleting?.app_name ?? ''}/${deleting?.primary_stream ?? ''}"?`}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return
          await deleteFailoverRule(deleting.id)
          setDeleting(null)
          mutate()
        }}
      />

      <ConfirmDelete
        open={!!triggering}
        title="Trigger failover"
        message={`Manually trigger failover for "${triggering?.app_name ?? ''}/${triggering?.primary_stream ?? ''}"? The backup will be activated.`}
        confirmLabel="Trigger"
        onCancel={() => setTriggering(null)}
        onConfirm={async () => {
          if (!triggering) return
          await triggerFailoverRule(triggering.id)
          setTriggering(null)
          mutate()
        }}
      />
    </div>
  )
}

function RuleModal({
  open,
  edit,
  onClose,
  onSaved,
}: {
  open: boolean
  edit: FailoverRule | null
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState({
    app_name: 'live',
    primary_stream: '',
    backup_url: '',
    backup_protocol: 'rtmp',
    trigger_type: 'automatic',
    stall_timeout_ms: 5000,
    warmup_ms: 2000,
    enabled: true,
    auto_restore: false,
  })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(JSON.stringify({ open, eid: edit?.id ?? null }), () => {
    if (!open) return
    if (edit) {
      setForm({
        app_name: edit.app_name,
        primary_stream: edit.primary_stream,
        backup_url: edit.backup_url,
        backup_protocol: edit.backup_protocol,
        trigger_type: edit.trigger_type,
        stall_timeout_ms: edit.stall_timeout_ms,
        warmup_ms: edit.warmup_ms,
        enabled: edit.enabled,
        auto_restore: edit.auto_restore,
      })
    } else {
      setForm({
        app_name: 'live',
        primary_stream: '',
        backup_url: '',
        backup_protocol: 'rtmp',
        trigger_type: 'automatic',
        stall_timeout_ms: 5000,
        warmup_ms: 2000,
        enabled: true,
        auto_restore: false,
      })
    }
    setErr(null)
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      if (edit) {
        await updateFailoverRule(edit.id, form)
      } else {
        await createFailoverRule(form)
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
      title={edit ? 'Edit failover rule' : 'New failover rule'}
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
          <FormField label="Primary stream">
            <input
              required
              value={form.primary_stream}
              onChange={(e) =>
                setForm({ ...form, primary_stream: e.target.value })
              }
              className={inputClass}
              placeholder="main"
            />
          </FormField>
        </div>
        <FormField label="Backup URL">
          <input
            required
            value={form.backup_url}
            onChange={(e) => {
              const url = e.target.value
              // A .mp4/.mov URL is a file backup; pick the protocol for it.
              const isFile = /\.(mp4|mov|m4v)(\?.*)?$/i.test(url.trim())
              setForm({
                ...form,
                backup_url: url,
                backup_protocol: isFile ? 'mp4' : form.backup_protocol === 'mp4' ? 'rtmp' : form.backup_protocol,
              })
            }}
            className={inputClass}
            placeholder={
              form.backup_protocol === 'mp4'
                ? 'https://cdn.example.com/slate.mp4 or /var/lib/origin-mediaserver/slate.mp4'
                : 'rtmp://backup.example.com:1935/live/main'
            }
          />
          {form.backup_protocol === 'mp4' && (
            <div className="mt-1 text-xs text-gray-500">
              Plays the file in a loop at the primary&apos;s URLs while the primary is down. H.264 +
              AAC MP4 (not fragmented); an http(s) file is downloaded once and cached on the
              server, a path must be readable by the server.
            </div>
          )}
        </FormField>
        <div className="grid grid-cols-2 gap-3">
          <FormField label="Backup protocol">
            <select
              value={form.backup_protocol}
              onChange={(e) =>
                setForm({ ...form, backup_protocol: e.target.value })
              }
              className={inputClass}
            >
              <option value="rtmp">RTMP pull</option>
              <option value="mp4">MP4 file (loop)</option>
              {/* Not implemented by the failover manager yet; kept visible
                  (and selectable for rules saved earlier) but disabled. */}
              <option value="rtsp" disabled={form.backup_protocol !== 'rtsp'}>
                RTSP (not supported yet)
              </option>
              <option value="srt" disabled={form.backup_protocol !== 'srt'}>
                SRT (not supported yet)
              </option>
              <option value="hls" disabled={form.backup_protocol !== 'hls'}>
                HLS (not supported yet)
              </option>
            </select>
          </FormField>
          <FormField label="Trigger type">
            <select
              value={form.trigger_type}
              onChange={(e) =>
                setForm({ ...form, trigger_type: e.target.value })
              }
              className={inputClass}
            >
              <option value="automatic">Automatic</option>
              <option value="manual">Manual only</option>
            </select>
          </FormField>
          <FormField label="Stall timeout (ms)">
            <input
              type="number"
              value={form.stall_timeout_ms}
              onChange={(e) =>
                setForm({ ...form, stall_timeout_ms: Number(e.target.value) })
              }
              className={inputClass}
            />
          </FormField>
          <FormField label="Warmup (ms)">
            <input
              type="number"
              value={form.warmup_ms}
              onChange={(e) =>
                setForm({ ...form, warmup_ms: Number(e.target.value) })
              }
              className={inputClass}
            />
          </FormField>
        </div>
        <label className="flex items-center gap-2 text-sm text-gray-300">
          <input
            type="checkbox"
            checked={form.enabled}
            onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
          />
          Enabled
        </label>
        <label className="flex items-center gap-2 text-sm text-gray-300">
          <input
            type="checkbox"
            checked={form.auto_restore}
            onChange={(e) =>
              setForm({ ...form, auto_restore: e.target.checked })
            }
          />
          Auto restore to primary once it recovers
        </label>
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
