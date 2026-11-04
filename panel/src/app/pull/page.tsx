'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import { Plus, Trash2, Pencil, Play, Square, Download, Lock, X } from 'lucide-react'
import { redactUrl } from '@/lib/utils'
import { usePreview } from '@/components/preview/StreamPreview'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
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
  StatusDot,
  useToast,
} from '@/components/ui'
import {
  listPullSources,
  createPullSource,
  updatePullSource,
  deletePullSource,
  startPull,
  stopPull,
  PullSource,
  listStreams,
} from '@/lib/api'

export default function PullPage() {
  return (
    <Layout>
      <PullContent />
    </Layout>
  )
}

// Display names for the protocol badge; every protocol uses the same
// badge tone so color never stands in for meaning here.
const PROTOCOL_LABEL: Record<string, string> = {
  rtmp: 'RTMP',
  rtsp: 'RTSP',
  srt: 'SRT',
  mpegts_http: 'MPEG-TS HTTP',
  mpegts_udp: 'MPEG-TS UDP',
  icecast: 'Icecast audio',
  hls: 'HLS',
}

const protocolLabel = (p: string) => PROTOCOL_LABEL[p] ?? (p || '—')

// onFallback: the source is running from one of its fallback URLs rather
// than the primary.
const onFallback = (s: PullSource) =>
  s.status === 'active' && (s.fallback_urls?.length ?? 0) > 0 && !!s.active_url && s.active_url !== s.url

type SortKey = 'stream' | 'app' | 'protocol' | 'status'
type SortState = { key: SortKey; dir: 'asc' | 'desc' }

function sortValue(s: PullSource, key: SortKey): string {
  switch (key) {
    case 'stream':
      return s.stream_name.toLowerCase()
    case 'app':
      return s.app_name.toLowerCase()
    case 'protocol':
      return (s.protocol || '').toLowerCase()
    case 'status':
      return (s.status || '').toLowerCase()
  }
}

function PullContent() {
  const preview = usePreview()
  // Live streams, to know which destinations are actually receiving
  // (preview only those). Same SWR key as Live Streams.
  const { data: liveList } = useSWR('streams', listStreams, { refreshInterval: 5000 })
  const liveByKey = useMemo(() => new Map((liveList ?? []).map((x) => [`${x.app}/${x.name}`, x])), [liveList])
  const liveKeys = liveByKey
  const { data, mutate, isLoading } = useSWR('pull-sources', listPullSources, {
    refreshInterval: 5000,
  })
  const toast = useToast()
  const [newOpen, setNewOpen] = useState(false)
  const [editing, setEditing] = useState<PullSource | null>(null)
  const [deleting, setDeleting] = useState<PullSource | null>(null)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [sort, setSort] = useState<SortState>({ key: 'app', dir: 'asc' })

  const sources = useMemo(() => {
    const mul = sort.dir === 'asc' ? 1 : -1
    return [...(data ?? [])].sort((a, b) => {
      const va = sortValue(a, sort.key)
      const vb = sortValue(b, sort.key)
      if (va < vb) return -1 * mul
      if (va > vb) return 1 * mul
      return a.app_name.localeCompare(b.app_name) || a.stream_name.localeCompare(b.stream_name)
    })
  }, [data, sort])

  const sortProps = (key: SortKey) => ({
    sort: sort.key === key ? sort.dir : (false as const),
    onSort: () =>
      setSort((s) => (s.key === key ? { key, dir: s.dir === 'asc' ? 'desc' : 'asc' } : { key, dir: 'asc' })),
  })

  const op = async (id: string, fn: () => Promise<void>, done: string) => {
    setBusyId(id)
    try {
      await fn()
      toast.success(done)
      mutate()
    } catch (e: any) {
      toast.error(e?.message ?? 'Operation failed')
    } finally {
      setBusyId(null)
    }
  }

  const createButton = (
    <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={() => setNewOpen(true)}>
      New source
    </Button>
  )

  return (
    <>
      <PageHeader
        title="Pull Sources"
        description="Ingest remote streams into this server. Each source can list fallback URLs that are tried in order when the primary fails."
        actions={createButton}
      />

      {isLoading ? (
        <TableSkeleton rows={3} cols={7} />
      ) : sources.length === 0 ? (
        <TableCard>
          <EmptyState
            icon={<Download size={20} />}
            title="No pull sources configured"
            description="Pull a remote RTMP, RTSP, SRT or MPEG-TS feed into a local app."
            action={createButton}
          />
        </TableCard>
      ) : (
        <TableCard>
          <Table dense>
            <THead>
              <tr>
                <TH {...sortProps('app')}>Application</TH>
                <TH {...sortProps('stream')}>Stream</TH>
                <TH>Source URL</TH>
                <TH {...sortProps('protocol')}>Protocol</TH>
                <TH align="right">Fallbacks</TH>
                <TH {...sortProps('status')}>Status</TH>
                <TH>Mode</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {sources.map((s) => {
                const isActive = s.status === 'active'
                const fallback = onFallback(s)
                const nFallback = s.fallback_urls?.length ?? 0
                return (
                  <TR key={s.id}>
                    <TD>{s.app_name}</TD>
                    <TD>
                      <div className="flex items-center gap-2.5">
                        <StatusDot status={isActive ? (fallback ? 'warning' : 'live') : 'idle'} />
                        <span className="font-medium text-fg">{s.stream_name}</span>
                      </div>
                    </TD>
                    <TD>
                      <div className="flex flex-col gap-0.5">
                        <Truncate mono max="max-w-[260px]" className="text-fg-secondary">
                          {redactUrl(s.url)}
                        </Truncate>
                        {fallback && (
                          <span className="flex min-w-0 items-center gap-1 text-xs text-warning">
                            <span className="shrink-0">via</span>
                            <Truncate mono max="max-w-[230px]">
                              {redactUrl(s.active_url!)}
                            </Truncate>
                          </span>
                        )}
                      </div>
                    </TD>
                    <TD>
                      <div className="inline-flex items-center gap-1.5">
                        <Badge variant="blue" mono>
                          {protocolLabel(s.protocol)}
                        </Badge>
                        {s.protocol === 'srt' && s.has_passphrase && (
                          <span title="Encrypted (passphrase set)" className="inline-flex text-fg-muted">
                            <Lock size={13} aria-hidden />
                            <span className="sr-only">Encrypted</span>
                          </span>
                        )}
                      </div>
                    </TD>
                    <TD numeric muted={nFallback === 0}>
                      {nFallback}
                    </TD>
                    <TD>
                      <div className="flex flex-col items-start gap-1">
                        {isActive ? (
                          <StatusBadge status="success" label="Active" />
                        ) : (
                          <StatusBadge
                            status={s.status === 'error' ? 'error' : 'idle'}
                            label={capitalize(s.status || 'inactive')}
                          />
                        )}
                        {fallback && (
                          <span title={`Running from fallback ${redactUrl(s.active_url ?? '')}`}>
                            <StatusBadge status="warning" label="On fallback" />
                          </span>
                        )}
                      </div>
                    </TD>
                    <TD className="whitespace-nowrap">{s.on_demand ? 'On demand' : 'Always on'}</TD>
                    <TD align="right">
                      <RowActions>
                        <IconButton
                          label={
                            liveKeys.has(`${s.app_name}/${s.stream_name}`)
                              ? `Preview ${s.app_name}/${s.stream_name}`
                              : `Preview ${s.app_name}/${s.stream_name} (not receiving)`
                          }
                          size="xs"
                          variant="secondary"
                          onClick={() => {
                            const st = liveByKey.get(`${s.app_name}/${s.stream_name}`)
                            // The destination stream's own HLS/DASH output, never the source URL.
                            preview.open({
                              app: s.app_name,
                              stream: s.stream_name,
                              live: !!st,
                              codec: st?.codec,
                              bitrateBps: st?.bitrate_bps,
                              context: `Pull source · ${protocolLabel(s.protocol)}`,
                            })
                          }}
                        >
                          <Play size={13} />
                        </IconButton>
                        {isActive ? (
                          <Button
                            size="xs"
                            variant="danger-outline"
                            icon={<Square size={12} />}
                            loading={busyId === s.id}
                            onClick={() => op(s.id, () => stopPull(s.id), `Stopped ${s.app_name}/${s.stream_name}`)}
                          >
                            Stop
                          </Button>
                        ) : (
                          <Button
                            size="xs"
                            variant="secondary"
                            icon={<Play size={12} />}
                            loading={busyId === s.id}
                            onClick={() => op(s.id, () => startPull(s.id), `Started ${s.app_name}/${s.stream_name}`)}
                          >
                            Start
                          </Button>
                        )}
                        <IconButton label="Edit source" size="xs" onClick={() => setEditing(s)}>
                          <Pencil size={14} />
                        </IconButton>
                        <DropdownMenu
                          label={`More actions for ${s.app_name}/${s.stream_name}`}
                          items={[
                            {
                              label: 'Delete source',
                              icon: <Trash2 size={14} />,
                              danger: true,
                              onClick: () => setDeleting(s),
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

      <PullModal
        open={newOpen}
        edit={null}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          toast.success('Pull source created')
          mutate()
        }}
      />
      <PullModal
        open={!!editing}
        edit={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          toast.success('Pull source saved')
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
          toast.success(`Deleted pull source ${deleting.app_name}/${deleting.stream_name}`)
          setDeleting(null)
          mutate()
        }}
      />
    </>
  )
}

const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

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
    fallback_urls: [] as string[],
    primary_check_s: 30,
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
        fallback_urls: edit.fallback_urls ?? [],
        primary_check_s: edit.primary_check_s ?? 30,
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
        fallback_urls: [],
        primary_check_s: 30,
      })
    }
    setClearPassphrase(false)
    setErr(null)
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    const data = { ...form, fallback_urls: form.fallback_urls.map((u) => u.trim()).filter(Boolean) }
    try {
      if (edit) {
        await updatePullSource(edit.id, { ...data, clear_passphrase: clearPassphrase })
      } else {
        await createPullSource(data)
      }
      onSaved()
    } catch (e: any) {
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }

  const urlInputClass = inputClass + ' font-mono text-[13px]'

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title={edit ? 'Edit pull source' : 'New pull source'}
      description="Pulls a remote feed and publishes it locally as app/stream."
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <FormSection title="Destination">
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
            <FormField label="Stream name" required>
              <input
                required
                value={form.stream_name}
                onChange={(e) => setForm({ ...form, stream_name: e.target.value })}
                className={inputClass}
                placeholder="cam1"
              />
            </FormField>
          </div>
        </FormSection>

        <FormSection title="Source">
          <FormField label="Source URL" required hint="The protocol is picked from the URL's scheme when it decides it.">
            <input
              required
              value={form.url}
              onChange={(e) => {
                const url = e.target.value
                const proto = protocolForURL(url, form.protocol)
                setForm({ ...form, url, protocol: proto ?? form.protocol })
              }}
              className={urlInputClass}
              placeholder="rtsp://camera.example.com/stream or http://radio.example.com:8000/live.mp3"
            />
          </FormField>
          <FormField
            label="Fallback URLs (optional)"
            hint={
              <>
                Tried in order when the primary fails or sends nothing for 10 s. Each URL&apos;s protocol comes from its
                scheme (rtmp://, srt://, rtsp://, udp://, http://).
              </>
            }
          >
            <div className="space-y-2">
              {form.fallback_urls.map((u, i) => (
                <div key={i} className="flex gap-2">
                  <input
                    value={u}
                    onChange={(e) => {
                      const next = [...form.fallback_urls]
                      next[i] = e.target.value
                      setForm({ ...form, fallback_urls: next })
                    }}
                    className={urlInputClass}
                    placeholder="rtmp://backup.example.com/app/stream"
                    aria-label={`Fallback URL ${i + 1}`}
                  />
                  <IconButton
                    label="Remove fallback URL"
                    variant="danger-outline"
                    size="md"
                    className="shrink-0"
                    onClick={() =>
                      setForm({ ...form, fallback_urls: form.fallback_urls.filter((_, j) => j !== i) })
                    }
                  >
                    <X size={15} />
                  </IconButton>
                </div>
              ))}
              <Button
                size="xs"
                variant="ghost"
                icon={<Plus size={13} />}
                onClick={() => setForm({ ...form, fallback_urls: [...form.fallback_urls, ''] })}
              >
                Add {form.fallback_urls.length ? 'another ' : ''}fallback URL
              </Button>
            </div>
          </FormField>
          {form.fallback_urls.length > 0 && (
            <FormField
              label="Primary check interval (s)"
              hint="While on a fallback, the primary is tried this often and the stream switches back once it has been up for 3 s. 0 = stay on the fallback until it fails."
            >
              <input
                type="number"
                min={0}
                value={form.primary_check_s}
                onChange={(e) => setForm({ ...form, primary_check_s: Number(e.target.value) })}
                className={inputClass + ' max-w-[160px]'}
              />
            </FormField>
          )}
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="Protocol">
              <select
                value={form.protocol}
                onChange={(e) => setForm({ ...form, protocol: e.target.value })}
                className={inputClass}
              >
                <option value="rtmp">RTMP</option>
                <option value="rtsp">RTSP</option>
                <option value="srt">SRT</option>
                <option value="mpegts_http">MPEG-TS over HTTP</option>
                <option value="mpegts_udp">MPEG-TS over UDP</option>
                <option value="icecast">Icecast / SHOUTcast audio (MP3, AAC)</option>
                <option value="hls" disabled>
                  HLS (not supported yet)
                </option>
              </select>
            </FormField>
            <FormField label="Reconnect delay (s)">
              <input
                type="number"
                min={1}
                value={form.reconnect_delay_s}
                onChange={(e) => setForm({ ...form, reconnect_delay_s: Number(e.target.value) })}
                className={inputClass}
              />
            </FormField>
          </div>
        </FormSection>

        {form.protocol === 'srt' && (
          <FormSection title="SRT encryption">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
                  placeholder={edit?.has_passphrase ? '••••••••••' : '10-79 characters'}
                  minLength={form.passphrase ? 10 : undefined}
                  maxLength={79}
                />
                {edit?.has_passphrase && (
                  <div className="mt-2">
                    <CheckboxField
                      checked={clearPassphrase}
                      onChange={(v) => {
                        setClearPassphrase(v)
                        if (v) setForm({ ...form, passphrase: '' })
                      }}
                      label="Remove passphrase (pull unencrypted)"
                    />
                  </div>
                )}
              </FormField>
              <FormField label="Key length (bytes)">
                <select
                  value={form.pbkeylen}
                  onChange={(e) => setForm({ ...form, pbkeylen: Number(e.target.value) })}
                  className={inputClass}
                >
                  <option value={0}>Default (16)</option>
                  <option value={16}>16</option>
                  <option value={24}>24</option>
                  <option value={32}>32</option>
                </select>
              </FormField>
            </div>
          </FormSection>
        )}

        <FormSection title="Behavior">
          <div className="space-y-2.5">
            <CheckboxField
              checked={form.on_demand}
              onChange={(v) => setForm({ ...form, on_demand: v })}
              label="On demand"
              description="Start pull only when a subscriber connects"
            />
            <CheckboxField
              checked={form.reconnect}
              onChange={(v) => setForm({ ...form, reconnect: v })}
              label="Reconnect on failure"
            />
          </div>
        </FormSection>

        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel={edit ? 'Save' : 'Create'} />
      </form>
    </Modal>
  )
}

// protocolForURL picks the protocol from a URL's scheme, matching the
// server's pull.ProtocolForURL; undefined when the scheme doesn't decide it.
// http(s) carries both MPEG-TS and Icecast audio: a chosen one of those is
// kept, else .mp3/.aac/.aacp means Icecast.
function protocolForURL(url: string, current?: string): string | undefined {
  const lower = url.trim().toLowerCase()
  const scheme = lower.split('://')[0]
  if (scheme === 'http' || scheme === 'https') {
    if (/\.(mp3|aacp?)(\?|#|$)/.test(lower)) return 'icecast'
    if (current === 'icecast' || current === 'mpegts_http') return current
    return 'mpegts_http'
  }
  return (
    {
      rtmp: 'rtmp',
      rtmps: 'rtmp',
      rtsp: 'rtsp',
      rtsps: 'rtsp',
      srt: 'srt',
      udp: 'mpegts_udp',
      http: 'mpegts_http',
      https: 'mpegts_http',
    } as Record<string, string>
  )[scheme]
}
