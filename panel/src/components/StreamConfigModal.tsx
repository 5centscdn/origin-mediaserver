'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Modal } from '@/components/Modal'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  useStateReset,
} from '@/components/FormHelpers'
import {
  listApplications,
  createStreamConfig,
  updateStreamConfig,
  Application,
  StreamConfig,
} from '@/lib/api'

type TriState = -1 | 0 | 1

const selectClass =
  'w-full bg-gray-800 border border-gray-700 rounded-md px-3 py-2 text-sm text-white focus:outline-none focus:border-blue-500'


const defaultStreamForm = {
  name_pattern: '',
  is_wildcard: false,
  out_hls: -1 as TriState,
  out_dash: -1 as TriState,
  out_mpegts: -1 as TriState,
  out_ll_hls: -1 as TriState,
  dvb_dash_enabled: -1 as TriState,
  dvb_subtitles_enabled: -1 as TriState,
  dvr_enabled: -1 as TriState,
  dvr_window_s: 0,
  abr_group_id: '',
  enabled: true,
}

/**
 * Create or edit a stream config (a stream defined under an application).
 * Used from the Applications page (app fixed) and the Streams page, where
 * `app` is omitted and the parent application is picked in the form. When
 * creating, several names can be entered at once (one per line or comma
 * separated), e.g. test1..test5 for an ABR group's renditions; each becomes
 * its own exact-name config, selectable in the ABR editor even while offline.
 */
export function StreamConfigModal({
  open,
  app,
  edit,
  onClose,
  onSaved,
}: {
  open: boolean
  app?: Application | null
  edit: StreamConfig | null
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState(defaultStreamForm)
  const [appId, setAppId] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const { data: apps } = useSWR(open && !app ? 'applications' : null, listApplications)
  const parent = app ?? (apps ?? []).find((a) => a.id === appId) ?? null
  // Several names only when creating: "test1, test2" or one per line.
  const names = edit
    ? [form.name_pattern.trim()]
    : form.name_pattern
        .split(/[\n,]+/)
        .map((n) => n.trim())
        .filter(Boolean)

  useStateReset(JSON.stringify({ open, eid: edit?.id ?? null }), () => {
    if (!open) return
    setAppId(edit?.app_id ?? app?.id ?? '')
    if (edit) {
      setForm({
        name_pattern: edit.name_pattern,
        is_wildcard: edit.is_wildcard,
        out_hls: edit.out_hls as TriState,
        out_dash: edit.out_dash as TriState,
        out_mpegts: edit.out_mpegts as TriState,
        out_ll_hls: edit.out_ll_hls as TriState,
        dvb_dash_enabled: edit.dvb_dash_enabled as TriState,
        dvb_subtitles_enabled: edit.dvb_subtitles_enabled as TriState,
        dvr_enabled: edit.dvr_enabled as TriState,
        dvr_window_s: edit.dvr_window_s ?? 0,
        abr_group_id: edit.abr_group_id ?? '',
        enabled: edit.enabled,
      })
    } else {
      setForm(defaultStreamForm)
    }
    setErr(null)
  })

  // Auto-detect wildcard when pattern changes
  const onPatternChange = (pattern: string) => {
    const isWild = pattern.includes('*') || pattern.includes('%i')
    setForm((prev) => ({ ...prev, name_pattern: pattern, is_wildcard: isWild }))
  }

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!parent) {
      setErr('Select the parent application')
      return
    }
    if (names.length === 0) {
      setErr('Enter a stream name')
      return
    }
    setBusy(true)
    setErr(null)
    const done: string[] = []
    try {
      if (edit) {
        await updateStreamConfig(parent.id, edit.id, form)
      } else {
        for (const name of names) {
          const isWild = name.includes('*') || name.includes('%i')
          await createStreamConfig(parent.id, {
            ...form,
            name_pattern: name,
            is_wildcard: names.length > 1 ? isWild : form.is_wildcard,
          })
          done.push(name)
        }
      }
      onSaved()
    } catch (e: any) {
      const msg = e?.message ?? 'Save failed'
      setErr(done.length ? `Created ${done.join(', ')}; failed on the next one: ${msg}` : msg)
    } finally {
      setBusy(false)
    }
  }

  const triOptions: { value: TriState; label: string }[] = [
    { value: -1, label: 'Inherit from app' },
    { value: 1, label: 'Enabled' },
    { value: 0, label: 'Disabled' },
  ]

  const dvrTriOptions: { value: TriState; label: string }[] = [
    { value: -1, label: 'Inherit from app' },
    { value: 1, label: 'Enabled' },
    { value: 0, label: 'Disabled' },
  ]

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title={
        edit
          ? `Edit Stream — ${parent?.name ?? ''}`
          : parent
            ? `Add Stream — ${parent.name}`
            : 'Add Stream'
      }
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-4">
        {/* Parent application (Streams page) */}
        {!app && (
          <FormField label="Parent Application">
            <select
              required
              value={appId}
              onChange={(e) => setAppId(e.target.value)}
              disabled={!!edit}
              className={selectClass}
            >
              <option value="" disabled>
                — select an application —
              </option>
              {(apps ?? []).map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </FormField>
        )}

        {/* Name(s) */}
        <FormField label={edit ? 'Stream Name / Pattern' : 'Stream Name(s)'}>
          {edit ? (
            <input
              required
              value={form.name_pattern}
              onChange={(e) => onPatternChange(e.target.value)}
              className={inputClass}
              placeholder="* or cam_* or cam_%i or mystream"
            />
          ) : (
            <textarea
              required
              rows={3}
              value={form.name_pattern}
              onChange={(e) => onPatternChange(e.target.value)}
              className={inputClass + ' font-mono'}
              placeholder={'mystream\nor several, one per line: test1, test2, test3'}
            />
          )}
          {!edit && names.length > 1 && (
            <div className="mt-1 text-xs text-blue-400">
              Creates {names.length} streams: {names.join(', ')}
              {parent && (
                <span className="text-gray-500">
                  {' '}
                  (publish to rtmp://&lt;host&gt;/{parent.name}/{names[0]})
                </span>
              )}
            </div>
          )}
          <div className="mt-1.5 text-xs text-gray-500 space-y-0.5 bg-gray-800/50 rounded px-3 py-2 border border-gray-700/50">
            <div>
              <code className="text-gray-400">*</code> — any stream name
            </div>
            <div>
              <code className="text-gray-400">cam_*</code> — any stream starting with{' '}
              <code className="text-gray-400">cam_</code>
            </div>
            <div>
              <code className="text-gray-400">cam_%i</code> — FMLE multi-bitrate (cam_1,
              cam_2, …)
            </div>
            <div>
              <code className="text-gray-400">mystream</code> — only that exact stream
            </div>
          </div>
        </FormField>

        {/* Wildcard (single name only) */}
        {names.length <= 1 && (
        <label className="flex items-center gap-2 text-sm text-gray-300">
          <input
            type="checkbox"
            checked={form.is_wildcard}
            onChange={(e) => setForm({ ...form, is_wildcard: e.target.checked })}
          />
          Is Wildcard{' '}
          <span className="text-gray-500 text-xs">
            (auto-detected from pattern containing * or %i)
          </span>
        </label>
        )}

        {/* Protocol overrides */}
        <div>
          <div className="text-xs uppercase tracking-wider text-gray-500 mb-2">
            Output Protocol Overrides
          </div>
          <div className="grid grid-cols-2 gap-3">
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
              <FormField key={key} label={label}>
                <select
                  value={form[key]}
                  onChange={(e) =>
                    setForm({ ...form, [key]: Number(e.target.value) as TriState })
                  }
                  className={selectClass}
                >
                  {triOptions.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </FormField>
            ))}
          </div>
        </div>

        {/* DVR */}
        <div>
          <div className="text-xs uppercase tracking-wider text-gray-500 mb-2">nDVR (rewind)</div>
          <div className="grid grid-cols-2 gap-3">
            <FormField label="DVR">
              <select
                value={form.dvr_enabled}
                onChange={(e) =>
                  setForm({ ...form, dvr_enabled: Number(e.target.value) as TriState })
                }
                className={selectClass}
              >
                {dvrTriOptions.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </FormField>
            {form.dvr_enabled !== 0 && (
              <FormField label="Rewind window (seconds)" hint="0 = inherit">
                <input
                  type="number"
                  min={0}
                  value={form.dvr_window_s}
                  onChange={(e) =>
                    setForm({ ...form, dvr_window_s: Number(e.target.value) })
                  }
                  className={inputClass}
                />
              </FormField>
            )}
          </div>
        </div>

        {/* Enabled */}
        <label className="flex items-center gap-2 text-sm text-gray-300">
          <input
            type="checkbox"
            checked={form.enabled}
            onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
          />
          Enabled
        </label>

        <ErrorBox msg={err} />
        <ModalActions
          busy={busy}
          onCancel={onClose}
          submitLabel={edit ? 'Save' : names.length > 1 ? `Add ${names.length} Streams` : 'Add'}
        />
      </form>
    </Modal>
  )
}
