'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Modal } from '@/components/Modal'
import {
  FormField,
  FormSection,
  CheckboxField,
  inputClass,
  textareaClass,
  ModalActions,
  ErrorBox,
  useStateReset,
} from '@/components/FormHelpers'
import { Toggle } from '@/components/ui'
import {
  listApplications,
  createStreamConfig,
  updateStreamConfig,
  Application,
  StreamConfig,
} from '@/lib/api'

type TriState = -1 | 0 | 1

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
  max_streams: 0,
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
        max_streams: edit.max_streams ?? 0,
      })
    } else {
      setForm(defaultStreamForm)
    }
    setErr(null)
  })

  // A publish limit only means something for a pattern (cam_%i, cam_*).
  const isPattern = (n: string) => n.includes('*') || n.includes('%i')
  const showLimit = names.length <= 1 ? form.is_wildcard : names.some(isPattern)

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
      description={
        edit
          ? 'Per-stream overrides on top of the application settings.'
          : 'Define a stream under an application. Exact names can be used in ABR groups before they go live.'
      }
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-5">
        <FormSection title="Stream">
          {/* Parent application (Streams page) */}
          {!app && (
            <FormField
              label="Parent Application"
              required
              hint={edit ? "A stream can't be moved to another application." : undefined}
            >
              <select
                required
                value={appId}
                onChange={(e) => setAppId(e.target.value)}
                disabled={!!edit}
                className={inputClass}
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
          <FormField
            label={edit ? 'Stream Name / Pattern' : 'Stream Name(s)'}
            required
            hint={!edit ? 'Enter several names (one per line or comma separated) to create one stream each.' : undefined}
          >
            {edit ? (
              <input
                required
                value={form.name_pattern}
                onChange={(e) => onPatternChange(e.target.value)}
                className={inputClass + ' font-mono'}
                placeholder="* or cam_* or cam_%i or mystream"
              />
            ) : (
              <textarea
                required
                rows={3}
                value={form.name_pattern}
                onChange={(e) => onPatternChange(e.target.value)}
                className={textareaClass}
                placeholder={'mystream\nor several, one per line: test1, test2, test3'}
              />
            )}
          </FormField>

          {!edit && names.length > 1 && (
            <div className="rounded-md border border-info/30 bg-info/10 px-3 py-2 text-xs text-info">
              Creates {names.length} streams: <span className="font-mono">{names.join(', ')}</span>
              {parent && (
                <span className="text-fg-muted">
                  {' '}
                  (publish to <span className="font-mono">rtmp://&lt;host&gt;/{parent.name}/{names[0]}</span>)
                </span>
              )}
            </div>
          )}

          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 rounded-md border border-border bg-elevated/40 px-3 py-2 text-xs text-fg-muted">
            <dt>
              <code className="font-mono text-fg-secondary">*</code>
            </dt>
            <dd>any stream name</dd>
            <dt>
              <code className="font-mono text-fg-secondary">cam_*</code>
            </dt>
            <dd>
              any stream starting with <code className="font-mono text-fg-secondary">cam_</code>
            </dd>
            <dt>
              <code className="font-mono text-fg-secondary">cam_%i</code>
            </dt>
            <dd>FMLE multi-bitrate (cam_1, cam_2, …)</dd>
            <dt>
              <code className="font-mono text-fg-secondary">mystream</code>
            </dt>
            <dd>only that exact stream</dd>
          </dl>

          {/* Wildcard (single name only) */}
          {names.length <= 1 && (
            <CheckboxField
              checked={form.is_wildcard}
              onChange={(v) => setForm({ ...form, is_wildcard: v })}
              label="Is Wildcard"
              description="Auto-detected from a pattern containing * or %i."
            />
          )}

          {showLimit && (
            <FormField
              label="Max publishing streams"
              hint={
                <>
                  How many streams matching this pattern can publish at the same time. Further publishes are refused
                  (e.g. <code className="font-mono">cam_%i</code> with 3 takes cam_1, cam_2, cam_3 and refuses a 4th
                  rendition). 0 = no limit.
                </>
              }
            >
              <input
                type="number"
                min={0}
                step={1}
                value={form.max_streams}
                onChange={(e) => setForm({ ...form, max_streams: Math.max(0, Math.floor(Number(e.target.value) || 0)) })}
                className={inputClass + ' sm:max-w-[12rem]'}
              />
            </FormField>
          )}

          <Toggle
            checked={form.enabled}
            onChange={(v) => setForm({ ...form, enabled: v })}
            label="Enabled"
          />
        </FormSection>

        {/* Protocol overrides */}
        <FormSection title="Output Protocol Overrides" description="Inherit uses the parent application's setting.">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
                  onChange={(e) => setForm({ ...form, [key]: Number(e.target.value) as TriState })}
                  className={inputClass}
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
        </FormSection>

        {/* DVR */}
        <FormSection title="nDVR (rewind)">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="DVR">
              <select
                value={form.dvr_enabled}
                onChange={(e) => setForm({ ...form, dvr_enabled: Number(e.target.value) as TriState })}
                className={inputClass}
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
                  onChange={(e) => setForm({ ...form, dvr_window_s: Number(e.target.value) })}
                  className={inputClass}
                />
              </FormField>
            )}
          </div>
        </FormSection>

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
