'use client'

import React, { useState } from 'react'
import useSWR from 'swr'
import {
  Plus,
  Trash2,
  Pencil,
  ChevronDown,
  ChevronRight,
  AppWindow,
  Eye,
  EyeOff,
  Copy,
  ListPlus,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { StreamConfigModal } from '@/components/StreamConfigModal'
import { Modal } from '@/components/Modal'
import { Badge, BadgeVariant } from '@/components/Badge'
import {
  FormField,
  FormSection,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import {
  PageHeader,
  TableCard,
  Table,
  THead,
  TH,
  TBody,
  TR,
  TD,
  RowActions,
  Button,
  IconButton,
  CopyButton,
  CopyField,
  DropdownMenu,
  EmptyState,
  TableSkeleton,
  StatusBadge,
  Toggle,
  useToast,
} from '@/components/ui'
import { listenPort, copyToClipboard } from '@/lib/utils'
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

// One badge color per meaning: delivery protocols are info, DVB-DASH
// extras (profile signalling, EBU-TT-D subtitles) are the primary accent,
// recording (nDVR) is warning.
const OUTPUT_PROTOCOL: BadgeVariant = 'blue'
const OUTPUT_DVB: BadgeVariant = 'purple'
const OUTPUT_RECORD: BadgeVariant = 'yellow'

function protocolChips(app: Application) {
  const chips: { label: string; on: boolean; color: BadgeVariant }[] = [
    { label: 'HLS', on: app.out_hls, color: OUTPUT_PROTOCOL },
    { label: 'DASH', on: app.out_dash, color: OUTPUT_PROTOCOL },
    { label: 'MPEG-TS', on: app.out_mpegts, color: OUTPUT_PROTOCOL },
    { label: 'LL-HLS', on: app.out_ll_hls, color: OUTPUT_PROTOCOL },
    { label: 'DVB-DASH', on: app.dvb_dash_enabled, color: OUTPUT_DVB },
    { label: 'EBU-TT-D', on: app.dvb_subtitles_enabled, color: OUTPUT_DVB },
    { label: 'nDVR', on: app.dvr_enabled, color: OUTPUT_RECORD },
  ]
  return chips.filter((c) => c.on)
}

function OutputBadges({ app }: { app: Application }) {
  const chips = protocolChips(app)
  if (chips.length === 0) return <span className="text-xs text-fg-muted">none</span>
  return (
    <div className="flex flex-wrap gap-1">
      {chips.map((c) => (
        <Badge key={c.label} variant={c.color} mono>
          {c.label}
        </Badge>
      ))}
    </div>
  )
}

// RTMP/SRT publish addresses for an app. The host is the one this panel is
// served from (the encoder may need a different, public name); the port
// comes from the node's listen config.
function publishURLs(appName: string, node: Awaited<ReturnType<typeof getNodeInfo>> | undefined) {
  const host = typeof window !== 'undefined' ? window.location.hostname : '<host>'
  return {
    rtmp: `rtmp://${host}${listenPort(node?.rtmp_listen, ':1935')}/${appName}`,
    srt: node?.srt?.enabled
      ? `srt://${host}${listenPort(node?.srt?.listen, ':4000')}?streamid=${appName}/<stream>`
      : null,
  }
}

// ---------- main content ----------

function ApplicationsContent() {
  const { data: apps, mutate: refreshApps, isLoading } = useSWR(
    'applications',
    listApplications,
    { refreshInterval: 15000 },
  )
  // Shares the 'node' SWR cache key with AppModal/Dashboard/Settings.
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 30000 })
  const toast = useToast()

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

  const openCreate = () => {
    setEditApp(null)
    setAppModalOpen(true)
  }

  return (
    <>
      <PageHeader
        title="Applications"
        description="Manage publishing applications, output protocols, and per-stream configs."
        actions={
          <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={openCreate}>
            New Application
          </Button>
        }
      />

      {isLoading ? (
        <TableSkeleton rows={3} cols={6} />
      ) : !apps || apps.length === 0 ? (
        <TableCard>
          <EmptyState
            icon={<AppWindow size={20} />}
            title="No applications yet"
            description="Create an application to start accepting RTMP, SRT and WHIP publish streams."
            action={
              <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={openCreate}>
                New Application
              </Button>
            }
          />
        </TableCard>
      ) : (
        <TableCard>
          <Table>
            <THead>
              <tr>
                <TH className="w-10">
                  <span className="sr-only">Expand</span>
                </TH>
                <TH>Name</TH>
                <TH>Status</TH>
                <TH>Publish key</TH>
                <TH>Allow any stream</TH>
                <TH>Outputs</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {apps.map((app) => (
                <AppRows
                  key={app.id}
                  app={app}
                  node={node}
                  expanded={expandedAppId === app.id}
                  onToggleExpand={() => toggleExpand(app.id)}
                  onEdit={() => {
                    setEditApp(app)
                    setAppModalOpen(true)
                  }}
                  onDelete={() => setDeleteAppTarget(app)}
                  onAddStream={() => {
                    setStreamModalApp(app)
                    setEditStream(null)
                  }}
                  onEditStream={(s) => {
                    setStreamModalApp(app)
                    setEditStream(s)
                  }}
                  onDeleteStream={(s) => setDeleteStreamTarget({ app, stream: s })}
                />
              ))}
            </TBody>
          </Table>
        </TableCard>
      )}

      {/* Application create/edit modal */}
      <AppModal
        open={appModalOpen}
        edit={editApp}
        onClose={() => setAppModalOpen(false)}
        onSaved={(name) => {
          toast.success(editApp ? `Saved application ${name}` : `Created application ${name}`)
          setAppModalOpen(false)
          refreshApps()
        }}
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
          toast.success(`Deleted application ${deleteAppTarget.name}`)
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
          onClose={() => {
            setStreamModalApp(null)
            setEditStream(null)
          }}
          onSaved={() => {
            toast.success(editStream ? 'Stream config saved' : 'Stream config created')
            setStreamModalApp(null)
            setEditStream(null)
            refreshApps()
          }}
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
          toast.success(`Deleted stream config ${deleteStreamTarget.stream.name_pattern}`)
          setDeleteStreamTarget(null)
          refreshApps()
        }}
      />
    </>
  )
}

// ---------- App row + expandable details ----------

function AppRows({
  app,
  node,
  expanded,
  onToggleExpand,
  onEdit,
  onDelete,
  onAddStream,
  onEditStream,
  onDeleteStream,
}: {
  app: Application
  node: Awaited<ReturnType<typeof getNodeInfo>> | undefined
  expanded: boolean
  onToggleExpand: () => void
  onEdit: () => void
  onDelete: () => void
  onAddStream: () => void
  onEditStream: (s: StreamConfig) => void
  onDeleteStream: (s: StreamConfig) => void
}) {
  const toast = useToast()
  const [showKey, setShowKey] = useState(false)

  const { data: streams } = useSWR(
    expanded ? ['stream-configs', app.id] : null,
    () => listStreamConfigs(app.id),
    { refreshInterval: 10000 },
  )

  const urls = publishURLs(app.name, node)

  const copyFromMenu = async (text: string, what: string) => {
    if (await copyToClipboard(text)) toast.success(`${what} copied`)
    else toast.error('Copy failed')
  }

  return (
    <>
      <TR selected={expanded}>
        <TD className="w-10 pr-0">
          <IconButton
            size="xs"
            label={expanded ? `Collapse ${app.name} details` : `Expand ${app.name} details`}
            aria-expanded={expanded}
            onClick={onToggleExpand}
          >
            {expanded ? <ChevronDown size={15} /> : <ChevronRight size={15} />}
          </IconButton>
        </TD>
        <TD>
          <button
            type="button"
            onClick={onToggleExpand}
            className="rounded font-medium text-fg hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-fg"
          >
            {app.name}
          </button>
        </TD>
        <TD>
          <StatusBadge
            status={app.enabled ? 'success' : 'disabled'}
            label={app.enabled ? 'Enabled' : 'Disabled'}
          />
        </TD>
        <TD>
          {app.publish_key ? (
            <div className="flex items-center gap-1">
              <code className="font-mono text-xs text-fg-secondary">
                {showKey ? app.publish_key : maskKey(app.publish_key)}
              </code>
              <IconButton
                size="xs"
                label={showKey ? 'Hide publish key' : 'Show publish key'}
                onClick={() => setShowKey((v) => !v)}
              >
                {showKey ? <EyeOff size={13} /> : <Eye size={13} />}
              </IconButton>
              <CopyButton text={app.publish_key} title="Copy publish key" toastMessage="Publish key copied" />
            </div>
          ) : (
            <span className="text-xs text-fg-muted">No key</span>
          )}
        </TD>
        <TD>
          <Badge variant={app.allow_any_stream ? 'blue' : 'yellow'}>
            {app.allow_any_stream ? 'Yes' : 'No'}
          </Badge>
        </TD>
        <TD>
          <OutputBadges app={app} />
        </TD>
        <TD align="right">
          <RowActions>
            <CopyButton
              text={urls.rtmp}
              label="RTMP"
              variant="secondary"
              title="Copy RTMP server URL (add the stream name in the encoder)"
              toastMessage="RTMP server URL copied"
            />
            <IconButton size="xs" label={`Edit ${app.name}`} onClick={onEdit}>
              <Pencil size={14} />
            </IconButton>
            <DropdownMenu
              label={`More actions for ${app.name}`}
              items={[
                {
                  label: expanded ? 'Hide stream configs' : 'Show stream configs',
                  icon: expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />,
                  onClick: onToggleExpand,
                },
                { label: 'Add stream config', icon: <ListPlus size={14} />, onClick: onAddStream },
                ...(urls.srt
                  ? [{ label: 'Copy SRT publish URL', icon: <Copy size={14} />, onClick: () => copyFromMenu(urls.srt!, 'SRT URL') }]
                  : []),
                { label: 'Delete application', icon: <Trash2 size={14} />, danger: true, separator: true, onClick: onDelete },
              ]}
            />
          </RowActions>
        </TD>
      </TR>

      {expanded && (
        <tr>
          <td colSpan={7} className="bg-canvas/50 p-0">
            <div className="space-y-4 border-b border-border px-4 py-4 sm:px-6">
              <AppDetails app={app} urls={urls} />
              <StreamConfigsPanel
                app={app}
                streams={streams}
                onAddStream={onAddStream}
                onEditStream={onEditStream}
                onDeleteStream={onDeleteStream}
              />
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

// Read-only summary of the app's settings, grouped as in the edit form.
function AppDetails({ app, urls }: { app: Application; urls: { rtmp: string; srt: string | null } }) {
  const scteLabel: Record<string, string> = {
    disabled: 'Disabled',
    passthrough: 'Passthrough',
    insert: 'Insert',
    hybrid: 'Hybrid',
  }
  const markerLabel: Record<string, string> = {
    none: 'None',
    cue: 'CUE-OUT/-IN/-CONT',
    splicepoint: 'SPLICEPOINT-SCTE35',
    daterange: 'DATERANGE',
  }
  const scte = app.scte35_mode || 'disabled'
  const marker = app.ad_marker_format || 'none'

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <DetailSection title="Ingest & auth">
        <DetailItem label="RTMP server URL">
          <CopyField value={urls.rtmp} />
          <div className="mt-1 text-xs text-fg-muted">
            Put the stream name in the encoder&apos;s stream name / key field.
          </div>
        </DetailItem>
        {urls.srt && (
          <DetailItem label="SRT publish URL">
            <CopyField value={urls.srt} />
          </DetailItem>
        )}
        <DetailItem label="Push login">
          {app.push_login ? (
            <span className="inline-flex flex-wrap items-center gap-2">
              <code className="font-mono text-xs text-fg-secondary">{app.push_login}</code>
              <span className="text-xs text-fg-muted">
                password {app.has_push_password ? 'set' : 'not set'}
              </span>
            </span>
          ) : (
            <span className="text-fg-muted">None</span>
          )}
        </DetailItem>
        <DetailItem label="Allow any stream">
          {app.allow_any_stream ? 'Yes' : 'No: only streams with a stream config can publish'}
        </DetailItem>
      </DetailSection>

      <DetailSection title="Delivery">
        <DetailItem label="Outputs">
          <OutputBadges app={app} />
        </DetailItem>
        <DetailItem label="HLS overrides">
          <span className="tabular">
            Segment {app.hls_seg_duration_s ? `${app.hls_seg_duration_s}s` : 'global'} · window{' '}
            {app.hls_playlist_window ? `${app.hls_playlist_window} segments` : 'global'}
          </span>
        </DetailItem>
        <DetailItem label="SCTE-35">
          {scteLabel[scte] ?? scte}
          {scte !== 'disabled' && (
            <span className="text-fg-muted"> · markers: {markerLabel[marker] ?? marker}</span>
          )}
        </DetailItem>
        <DetailItem label="nDVR">
          {app.dvr_enabled ? (
            <span className="tabular">
              Recording · window {app.dvr_window_s ? `${app.dvr_window_s}s` : 'global'}
            </span>
          ) : (
            <span className="text-fg-muted">Off</span>
          )}
        </DetailItem>
      </DetailSection>
    </div>
  )
}

function DetailSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-lg border border-border bg-surface">
      <h3 className="border-b border-border px-4 py-2.5 text-[13px] font-semibold text-fg">{title}</h3>
      <dl className="divide-y divide-border">{children}</dl>
    </section>
  )
}

function DetailItem({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-1 px-4 py-2.5 sm:grid-cols-[140px_1fr] sm:gap-4">
      <dt className="pt-0.5 text-[13px] text-fg-muted">{label}</dt>
      <dd className="min-w-0 text-[13px] text-fg-secondary">{children}</dd>
    </div>
  )
}

function StreamConfigsPanel({
  app,
  streams,
  onAddStream,
  onEditStream,
  onDeleteStream,
}: {
  app: Application
  streams: StreamConfig[] | undefined
  onAddStream: () => void
  onEditStream: (s: StreamConfig) => void
  onDeleteStream: (s: StreamConfig) => void
}) {
  return (
    <section className="overflow-hidden rounded-lg border border-border bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-4 py-2.5">
        <h3 className="text-[13px] font-semibold text-fg">
          Stream configs <span className="font-normal text-fg-muted">· {app.name}</span>
        </h3>
        <Button variant="secondary" size="xs" icon={<Plus size={13} />} onClick={onAddStream}>
          Add stream config
        </Button>
      </div>

      {!streams || streams.length === 0 ? (
        <div className="px-4 py-6 text-center text-[13px] text-fg-muted">
          No stream configs.{' '}
          {app.allow_any_stream
            ? 'Any stream can publish (allow_any_stream is on).'
            : 'Add a config to allow specific streams to publish.'}
        </div>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <TH>Pattern</TH>
                <TH>Match</TH>
                <TH>Outputs</TH>
                <TH>DVR</TH>
                <TH>Status</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {streams.map((s) => (
                <StreamConfigRow
                  key={s.id}
                  stream={s}
                  onEdit={() => onEditStream(s)}
                  onDelete={() => onDeleteStream(s)}
                />
              ))}
            </TBody>
          </Table>
        </div>
      )}
    </section>
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
  const dvrLabel =
    stream.dvr_enabled === -1
      ? 'inherit'
      : stream.dvr_enabled === 1
        ? `on${stream.dvr_window_s ? ` (${stream.dvr_window_s}s)` : ''}`
        : 'off'

  // Tri-state outputs (-1 inherit, 0 off, 1 on): overrides get a badge,
  // inherited ones are summarised as text.
  const protoChips: { label: string; v: number }[] = [
    { label: 'HLS', v: stream.out_hls },
    { label: 'DASH', v: stream.out_dash },
    { label: 'MPEG-TS', v: stream.out_mpegts },
    { label: 'LL-HLS', v: stream.out_ll_hls },
    { label: 'DVB-DASH', v: stream.dvb_dash_enabled },
    { label: 'EBU-TT-D', v: stream.dvb_subtitles_enabled },
  ]
  const inherited = protoChips.filter((c) => c.v === -1)

  return (
    <TR>
      <TD mono className="text-fg">
        {stream.name_pattern}
      </TD>
      <TD>
        {stream.is_wildcard ? <Badge variant="purple">wildcard</Badge> : <Badge variant="gray">exact</Badge>}
      </TD>
      <TD>
        <div className="flex flex-wrap items-center gap-1">
          {protoChips
            .filter((c) => c.v !== -1)
            .map(({ label, v }) => (
              <Badge
                key={label}
                mono
                variant={v === 1 ? 'blue' : 'gray'}
                className={v === 1 ? undefined : 'line-through'}
                title={`${label}: ${v === 1 ? 'on' : 'off'}`}
              >
                {label}
                <span className="sr-only">{v === 1 ? ' on' : ' off'}</span>
              </Badge>
            ))}
          {inherited.length > 0 && (
            <span className="text-xs text-fg-muted" title={`Inherited from app: ${inherited.map((c) => c.label).join(', ')}`}>
              {inherited.length === protoChips.length ? 'all inherit' : `${inherited.length} inherit`}
            </span>
          )}
        </div>
      </TD>
      <TD>{dvrLabel}</TD>
      <TD>
        <StatusBadge status={stream.enabled ? 'success' : 'disabled'} label={stream.enabled ? 'On' : 'Off'} />
      </TD>
      <TD align="right">
        <RowActions>
          <IconButton size="xs" label={`Edit ${stream.name_pattern}`} onClick={onEdit}>
            <Pencil size={14} />
          </IconButton>
          <DropdownMenu
            label={`More actions for ${stream.name_pattern}`}
            items={[{ label: 'Delete stream config', icon: <Trash2 size={14} />, danger: true, onClick: onDelete }]}
          />
        </RowActions>
      </TD>
    </TR>
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

// Text input for a secret, masked by default with a show/hide button.
function SecretInput({
  value,
  onChange,
  placeholder,
  required,
  label,
}: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  required?: boolean
  label: string
}) {
  const [show, setShow] = useState(false)
  return (
    <div className="relative">
      <input
        type={show ? 'text' : 'password'}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className={inputClass + ' pr-10 font-mono'}
        placeholder={placeholder}
        autoComplete="new-password"
        spellCheck={false}
        required={required}
      />
      <IconButton
        size="xs"
        label={show ? `Hide ${label}` : `Show ${label}`}
        onClick={() => setShow((v) => !v)}
        className="absolute right-1 top-1/2 -translate-y-1/2"
      >
        {show ? <EyeOff size={14} /> : <Eye size={14} />}
      </IconButton>
    </div>
  )
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
  onSaved: (name: string) => void
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
      onSaved(form.name)
    } catch (e: any) {
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }

  const outputs = [
    { key: 'out_hls', label: 'HLS', description: 'Segmented HTTP Live Streaming' },
    { key: 'out_dash', label: 'DASH', description: 'MPEG-DASH manifest and segments' },
    { key: 'out_mpegts', label: 'MPEG-TS', description: 'Continuous TS over HTTP (mpeg.2ts)' },
    { key: 'out_ll_hls', label: 'LL-HLS', description: 'Low-latency HLS partial segments' },
    { key: 'dvb_dash_enabled', label: 'DVB-DASH Profile', description: 'DVB profile signalling in the MPD' },
    { key: 'dvb_subtitles_enabled', label: 'EBU-TT-D Subtitles', description: 'CEA-608/708 captions as DASH subtitles' },
  ] as const

  const appPath = form.name || '<app>'

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title={edit ? 'Edit Application' : 'New Application'}
      description={edit ? edit.name : 'An application groups streams under one publish path, key and output settings.'}
      size="lg"
    >
      <form onSubmit={onSubmit} className="space-y-5">
        <FormSection title="General">
          <FormField label="App Name" required hint="Used in publish and playback URLs">
            <input
              required
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              className={inputClass}
              placeholder="live"
            />
          </FormField>
          <Toggle
            checked={form.enabled}
            onChange={(v) => setForm({ ...form, enabled: v })}
            label="Enabled"
            description="Disabled applications reject new publishers"
          />
        </FormSection>

        <FormSection title="Ingest & auth" description="Who may publish, and how encoders authenticate.">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="Publish Key" hint="Leave blank for no key">
              <SecretInput
                label="publish key"
                value={form.publish_key}
                onChange={(v) => setForm({ ...form, publish_key: v })}
                placeholder="leave blank for no key"
              />
            </FormField>
            <div className="hidden sm:block" />
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
              required={!!form.push_login && !edit?.has_push_password}
              hint={
                edit?.has_push_password
                  ? 'Set. Leave blank to keep the current password'
                  : 'Encoder password (FMLE prompts for it)'
              }
            >
              <SecretInput
                label="push password"
                value={form.push_password}
                onChange={(v) => setForm({ ...form, push_password: v })}
                placeholder={edit?.has_push_password ? '••••••••' : ''}
                required={!!form.push_login && !edit?.has_push_password}
              />
            </FormField>
          </div>

          <Toggle
            checked={form.allow_any_stream}
            onChange={(v) => setForm({ ...form, allow_any_stream: v })}
            label="Allow Any Stream"
            description="If off, only streams defined in stream configs can publish"
          />

          {/* Publish URLs — informational, derived from the app name above */}
          <div className="space-y-1 rounded-md border border-border bg-elevated/40 px-3 py-2">
            <div className="text-xs font-medium text-fg-muted">Publish via</div>
            <div className="break-all font-mono text-xs text-fg-secondary">
              RTMP: rtmp://&lt;host&gt;{listenPort(node?.rtmp_listen, ':1935')}/{appPath}/&lt;stream&gt;
            </div>
            {node?.srt?.enabled && (
              <div className="break-all font-mono text-xs text-fg-secondary">
                SRT: srt://&lt;host&gt;{listenPort(node?.srt?.listen, ':4000')}?streamid={appPath}/&lt;stream&gt;
              </div>
            )}
          </div>
        </FormSection>

        <FormSection title="Output protocols" description="Formats viewers can play this application's streams in.">
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {outputs.map(({ key, label, description }) => (
              <div key={key} className="rounded-md border border-border bg-elevated/30 px-3 py-2.5">
                <Toggle
                  checked={form[key]}
                  onChange={(v) => setForm({ ...form, [key]: v })}
                  label={label}
                  description={description}
                />
              </div>
            ))}
          </div>
        </FormSection>

        <FormSection title="HLS overrides" description="0 = use global defaults.">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <FormField label="Segment Duration (s)">
              <input
                type="number"
                min={0}
                value={form.hls_seg_duration_s}
                onChange={(e) => setForm({ ...form, hls_seg_duration_s: Number(e.target.value) })}
                className={inputClass}
              />
            </FormField>
            <FormField label="Playlist Window (segments)">
              <input
                type="number"
                min={0}
                value={form.hls_playlist_window}
                onChange={(e) => setForm({ ...form, hls_playlist_window: Number(e.target.value) })}
                className={inputClass}
              />
            </FormField>
          </div>
        </FormSection>

        <FormSection title="SCTE-35" description="Ad-break markers.">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
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
            <FormField
              label="Ad marker format"
              hint={form.scte35_mode === 'disabled' ? 'Choose a mode first' : 'Tags written into HLS playlists'}
            >
              <select
                value={form.ad_marker_format}
                onChange={(e) => setForm({ ...form, ad_marker_format: e.target.value })}
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
        </FormSection>

        <FormSection title="nDVR (rewind)">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Toggle
              checked={form.dvr_enabled}
              onChange={(v) => setForm({ ...form, dvr_enabled: v })}
              label="Record & allow rewind"
              description="One fMP4 recording serves both HLS and DASH rewind"
            />
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
            <div className="space-y-1 break-all rounded-md border border-border bg-elevated/40 px-3 py-2 font-mono text-xs text-fg-secondary">
              <div>/{appPath}/&lt;stream&gt;/playlist_dvr.m3u8</div>
              <div>/{appPath}/&lt;stream&gt;/manifest_dvr.mpd</div>
            </div>
          )}
        </FormSection>

        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel={edit ? 'Save' : 'Create'} />
      </form>
    </Modal>
  )
}
