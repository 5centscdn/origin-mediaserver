'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import { Plus, Trash2, Pencil, Zap, ShieldAlert } from 'lucide-react'
import { redactUrl } from '@/lib/utils'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge, BadgeVariant } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  CheckboxField,
  FormSection,
  useStateReset,
} from '@/components/FormHelpers'
import {
  PageHeader,
  Button,
  IconButton,
  TableCard,
  Table,
  THead,
  TH,
  TBody,
  TR,
  TD,
  RowActions,
  Truncate,
  DropdownMenu,
  EmptyState,
  TableSkeleton,
  StatusBadge,
  StatusKind,
  useToast,
} from '@/components/ui'
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

// stateStatus maps the failover manager's rule state onto a status pill
// (always with text, so it reads without color).
function stateStatus(state: string): { status: StatusKind; label: string } {
  switch (state) {
    case 'active_primary':
      return { status: 'success', label: 'On primary' }
    case 'active_backup':
      return { status: 'warning', label: 'On backup' }
    case 'idle':
      return { status: 'idle', label: 'Idle' }
    default:
      return { status: 'idle', label: state || '—' }
  }
}

// Backup protocols the failover manager implements get a colored badge;
// the "not supported yet" ones (rules saved earlier) stay neutral.
function protocolVariant(p: string): BadgeVariant {
  if (p === 'rtmp') return 'blue'
  if (p === 'mp4') return 'purple'
  return 'gray'
}

type SortKey = 'app' | 'primary' | 'protocol' | 'state'
type SortState = { key: SortKey; dir: 'asc' | 'desc' }

function sortValue(r: FailoverRule, key: SortKey): string {
  switch (key) {
    case 'app':
      return r.app_name.toLowerCase()
    case 'primary':
      return r.primary_stream.toLowerCase()
    case 'protocol':
      return (r.backup_protocol || '').toLowerCase()
    case 'state':
      return (r.state || '').toLowerCase()
  }
}

function FailoverContent() {
  const { data, mutate, isLoading } = useSWR('failover-rules', listFailoverRules, {
    refreshInterval: 5000,
  })
  const toast = useToast()
  const [newOpen, setNewOpen] = useState(false)
  const [editing, setEditing] = useState<FailoverRule | null>(null)
  const [deleting, setDeleting] = useState<FailoverRule | null>(null)
  const [triggering, setTriggering] = useState<FailoverRule | null>(null)
  const [sort, setSort] = useState<SortState>({ key: 'app', dir: 'asc' })

  const rules = useMemo(() => {
    const mul = sort.dir === 'asc' ? 1 : -1
    return [...(data ?? [])].sort((a, b) => {
      const va = sortValue(a, sort.key)
      const vb = sortValue(b, sort.key)
      if (va < vb) return -1 * mul
      if (va > vb) return 1 * mul
      return a.app_name.localeCompare(b.app_name) || a.primary_stream.localeCompare(b.primary_stream)
    })
  }, [data, sort])

  const sortProps = (key: SortKey) => ({
    sort: sort.key === key ? sort.dir : (false as const),
    onSort: () =>
      setSort((s) => (s.key === key ? { key, dir: s.dir === 'asc' ? 'desc' : 'asc' } : { key, dir: 'asc' })),
  })

  const createButton = (
    <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={() => setNewOpen(true)}>
      Create rule
    </Button>
  )

  return (
    <>
      <PageHeader
        title="Failover Rules"
        description="When a primary stream stalls, a rule switches its outputs to a backup (an RTMP pull or a looped MP4 file) so viewers keep playing, and can switch back once the primary recovers."
        actions={createButton}
      />

      {isLoading ? (
        <TableSkeleton rows={3} cols={7} />
      ) : rules.length === 0 ? (
        <TableCard>
          <EmptyState
            icon={<ShieldAlert size={20} />}
            title="No failover rules defined"
            description="Create a rule to automatically pull a backup feed when the primary stalls."
            action={createButton}
          />
        </TableCard>
      ) : (
        <TableCard>
          <Table>
            <THead>
              <tr>
                <TH {...sortProps('app')}>Application</TH>
                <TH {...sortProps('primary')}>Primary</TH>
                <TH>Backup URL</TH>
                <TH {...sortProps('protocol')}>Protocol</TH>
                <TH>Trigger</TH>
                <TH {...sortProps('state')}>State</TH>
                <TH>Enabled</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {rules.map((r) => {
                const st = stateStatus(r.state)
                return (
                  <TR key={r.id}>
                    <TD>{r.app_name}</TD>
                    <TD>
                      <span className="font-medium text-fg">{r.primary_stream}</span>
                    </TD>
                    <TD>
                      <Truncate mono max="max-w-[260px]">
                        {redactUrl(r.backup_url)}
                      </Truncate>
                    </TD>
                    <TD>
                      <Badge variant={protocolVariant(r.backup_protocol)} mono>
                        {r.backup_protocol}
                      </Badge>
                    </TD>
                    <TD className="whitespace-nowrap">{r.trigger_type === 'manual' ? 'Manual only' : r.trigger_type === 'automatic' ? 'Automatic' : r.trigger_type}</TD>
                    <TD>
                      <StatusBadge status={st.status} label={st.label} />
                    </TD>
                    <TD>
                      {r.enabled ? (
                        <StatusBadge status="success" label="Enabled" />
                      ) : (
                        <StatusBadge status="disabled" label="Disabled" />
                      )}
                    </TD>
                    <TD align="right">
                      <RowActions>
                        <Button
                          size="xs"
                          variant="secondary"
                          icon={<Zap size={13} />}
                          title="Trigger failover manually"
                          onClick={() => setTriggering(r)}
                        >
                          Trigger
                        </Button>
                        <IconButton label="Edit rule" size="xs" onClick={() => setEditing(r)}>
                          <Pencil size={14} />
                        </IconButton>
                        <DropdownMenu
                          label={`More actions for ${r.app_name}/${r.primary_stream}`}
                          items={[
                            {
                              label: 'Delete rule',
                              icon: <Trash2 size={14} />,
                              danger: true,
                              onClick: () => setDeleting(r),
                            },
                          ]}
                        />
                      </RowActions>
                    </TD>
                  </TR>
                )
              })}
            </TBody>
          </Table>
        </TableCard>
      )}

      <RuleModal
        open={newOpen}
        edit={null}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          toast.success('Failover rule created')
          mutate()
        }}
      />
      <RuleModal
        open={!!editing}
        edit={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          toast.success('Failover rule saved')
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
          toast.success(`Deleted failover rule for ${deleting.app_name}/${deleting.primary_stream}`)
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
          toast.success(`Failover triggered for ${triggering.app_name}/${triggering.primary_stream}`)
          setTriggering(null)
          mutate()
        }}
      />
    </>
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
      description="The backup is played at the primary's URLs while the primary is stalled."
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <FormSection title="Primary">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="App name" required>
              <input
                required
                value={form.app_name}
                onChange={(e) => setForm({ ...form, app_name: e.target.value })}
                className={inputClass}
                placeholder="live"
              />
            </FormField>
            <FormField label="Primary stream" required>
              <input
                required
                value={form.primary_stream}
                onChange={(e) => setForm({ ...form, primary_stream: e.target.value })}
                className={inputClass}
                placeholder="main"
              />
            </FormField>
          </div>
        </FormSection>

        <FormSection title="Backup">
          <FormField
            label="Backup URL"
            required
            hint={
              form.backup_protocol === 'mp4' ? (
                <>
                  Plays the file in a loop at the primary&apos;s URLs while the primary is down. H.264 + AAC MP4
                  (not fragmented); an http(s) file is downloaded once and cached on the server, a path must be
                  readable by the server.
                </>
              ) : undefined
            }
          >
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
              className={inputClass + ' font-mono text-[13px]'}
              placeholder={
                form.backup_protocol === 'mp4'
                  ? 'https://cdn.example.com/slate.mp4 or /var/lib/origin-mediaserver/slate.mp4'
                  : 'rtmp://backup.example.com:1935/live/main'
              }
            />
          </FormField>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="Backup protocol">
              <select
                value={form.backup_protocol}
                onChange={(e) => setForm({ ...form, backup_protocol: e.target.value })}
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
                onChange={(e) => setForm({ ...form, trigger_type: e.target.value })}
                className={inputClass}
              >
                <option value="automatic">Automatic</option>
                <option value="manual">Manual only</option>
              </select>
            </FormField>
          </div>
        </FormSection>

        <FormSection title="Timing">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="Stall timeout (ms)">
              <input
                type="number"
                value={form.stall_timeout_ms}
                onChange={(e) => setForm({ ...form, stall_timeout_ms: Number(e.target.value) })}
                className={inputClass}
              />
            </FormField>
            <FormField label="Warmup (ms)">
              <input
                type="number"
                value={form.warmup_ms}
                onChange={(e) => setForm({ ...form, warmup_ms: Number(e.target.value) })}
                className={inputClass}
              />
            </FormField>
          </div>
          <div className="space-y-2.5">
            <CheckboxField
              checked={form.enabled}
              onChange={(v) => setForm({ ...form, enabled: v })}
              label="Enabled"
            />
            <CheckboxField
              checked={form.auto_restore}
              onChange={(v) => setForm({ ...form, auto_restore: v })}
              label="Auto restore to primary once it recovers"
            />
          </div>
        </FormSection>

        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel={edit ? 'Save' : 'Create'} />
      </form>
    </Modal>
  )
}
