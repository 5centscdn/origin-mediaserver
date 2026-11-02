// Complete typed API client for the Origin Media Server.
//
// The default API base is `/api/v1` (same-origin, when the panel is served
// from the Go binary). When the user switches the "active server" in the
// panel UI, all calls go to `${activeServerUrl}/api/v1` instead. The active
// server URL persists in localStorage as `activeServerUrl`.

let accessToken: string | null = null
let refreshToken: string | null = null
let activeServerUrl: string | null = null

const LS_ACCESS = 'access_token'
const LS_REFRESH = 'refresh_token'
const LS_ROLE = 'role'
const LS_ACTIVE = 'activeServerUrl'

// ---------- Token / auth state ----------

export function getToken(): string | null {
  return accessToken
}

export function setToken(t: string): void {
  accessToken = t
  if (typeof window !== 'undefined') {
    localStorage.setItem(LS_ACCESS, t)
  }
}

export function clearAuth(): void {
  accessToken = null
  refreshToken = null
  if (typeof window !== 'undefined') {
    localStorage.removeItem(LS_ACCESS)
    localStorage.removeItem(LS_REFRESH)
    localStorage.removeItem(LS_ROLE)
  }
}

export function restoreAuth(): void {
  if (typeof window === 'undefined') return
  const t = localStorage.getItem(LS_ACCESS)
  const r = localStorage.getItem(LS_REFRESH)
  if (t) accessToken = t
  if (r) refreshToken = r
  const a = localStorage.getItem(LS_ACTIVE)
  if (a) activeServerUrl = a
}

export function getRole(): string | null {
  if (typeof window === 'undefined') return null
  return localStorage.getItem(LS_ROLE)
}

// ---------- Active server (multi-server) ----------

export interface ServerEntry {
  id: string
  name: string
  url: string
  note: string
  created_at: string
  is_local?: boolean
}

export function getActiveServer(): ServerEntry | null {
  if (typeof window === 'undefined') return null
  const raw = localStorage.getItem('activeServer')
  if (!raw) return null
  try {
    return JSON.parse(raw) as ServerEntry
  } catch {
    return null
  }
}

export function setActiveServer(s: ServerEntry | null): void {
  if (typeof window === 'undefined') return
  if (!s || s.is_local || !s.url) {
    activeServerUrl = null
    localStorage.removeItem(LS_ACTIVE)
    localStorage.removeItem('activeServer')
    return
  }
  // Strip trailing slash.
  activeServerUrl = s.url.replace(/\/+$/, '')
  localStorage.setItem(LS_ACTIVE, activeServerUrl)
  localStorage.setItem('activeServer', JSON.stringify(s))
}

function baseUrl(): string {
  // Re-read each call so a fresh switch is picked up immediately.
  if (typeof window !== 'undefined') {
    const a = localStorage.getItem(LS_ACTIVE)
    if (a) {
      activeServerUrl = a.replace(/\/+$/, '')
      return `${activeServerUrl}/api/v1`
    }
    activeServerUrl = null
  }
  return '/api/v1'
}

// ---------- Core fetch wrapper ----------

async function apiFetch<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...((opts.headers as Record<string, string>) ?? {}),
  }
  if (accessToken) {
    headers['Authorization'] = `Bearer ${accessToken}`
  }
  let res: Response
  try {
    res = await fetch(`${baseUrl()}${path}`, { ...opts, headers })
  } catch (err: any) {
    throw new Error(err?.message ?? 'network error')
  }

  if (res.status === 401) {
    // Token expired / invalid — bounce to login.
    clearAuth()
    if (typeof window !== 'undefined' && !window.location.pathname.endsWith('/login') && !window.location.pathname.endsWith('/login/')) {
      window.location.href = '/login/'
    }
    throw new Error('unauthorized')
  }
  if (!res.ok) {
    let msg = res.statusText
    try {
      const j = await res.json()
      msg = j.error ?? j.message ?? msg
    } catch {}
    throw new Error(msg)
  }
  if (res.status === 204) return undefined as T
  const text = await res.text()
  if (!text) return undefined as T
  try {
    return JSON.parse(text) as T
  } catch {
    return text as unknown as T
  }
}

// ---------- Auth endpoints ----------

export async function login(
  email: string,
  password: string,
): Promise<{ access_token: string; refresh_token: string; role: string }> {
  const data = await apiFetch<{ access_token: string; refresh_token: string; role: string }>(
    '/auth/login',
    { method: 'POST', body: JSON.stringify({ email, password }) },
  )
  accessToken = data.access_token
  refreshToken = data.refresh_token
  if (typeof window !== 'undefined') {
    localStorage.setItem(LS_ACCESS, data.access_token)
    localStorage.setItem(LS_REFRESH, data.refresh_token)
    if (data.role) localStorage.setItem(LS_ROLE, data.role)
  }
  return data
}

export async function logout(): Promise<void> {
  try {
    await apiFetch<void>('/auth/logout', { method: 'POST' })
  } catch {
    // Even if the server call fails, drop local state.
  }
  clearAuth()
}

// ---------- Servers ----------

export async function listServers(): Promise<ServerEntry[]> {
  try {
    return await apiFetch<ServerEntry[]>('/servers')
  } catch (err: any) {
    if (err?.message === 'unauthorized') throw err
    // The endpoint may not exist on older servers — return local-only.
    return []
  }
}

export async function addServer(data: {
  name: string
  url: string
  note: string
}): Promise<ServerEntry> {
  return apiFetch<ServerEntry>('/servers', {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export async function deleteServer(id: string): Promise<void> {
  await apiFetch<void>(`/servers/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

// Ping a server by absolute URL to see if it's reachable.
export async function pingServer(
  url: string,
): Promise<{ ok: boolean; info?: NodeInfo }> {
  const u = url.replace(/\/+$/, '')
  try {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    if (accessToken) headers['Authorization'] = `Bearer ${accessToken}`
    const res = await fetch(`${u}/api/v1/node`, { headers })
    if (!res.ok) return { ok: false }
    const info = (await res.json()) as NodeInfo
    return { ok: true, info }
  } catch {
    return { ok: false }
  }
}

// ---------- Streams ----------

export interface Stream {
  id: string
  app: string
  name: string
  source_type: string
  source_addr: string
  abr_group?: string
  started_at: string
  uptime_s: number
  bytes_in: number
  subscribers: number
  codec?: string
}

export async function listStreams(): Promise<Stream[]> {
  const v = await apiFetch<Stream[] | null>('/streams')
  return v ?? []
}

// These all address a stream by its two separate app/name path segments,
// not a single "app/name" id — chi v5 matches on RawPath and never
// unescapes a "%2F" produced by encodeURIComponent("app/name"), so a
// combined-id URL silently fails to route to the right stream.
export async function getStream(app: string, name: string): Promise<Stream> {
  return apiFetch<Stream>(`/streams/${encodeURIComponent(app)}/${encodeURIComponent(name)}`)
}

export async function kickStream(app: string, name: string): Promise<void> {
  await apiFetch<void>(`/streams/${encodeURIComponent(app)}/${encodeURIComponent(name)}/kick`, { method: 'POST' })
}

// Re-resolves this stream's application/stream-config (SCTE-35 mode, DASH
// toggle, HLS overrides, DVR) and restarts just its output pipeline —
// unlike kickStream, this never touches the ingest connection or any other
// stream. Use after editing an Application while a stream using it is live.
export async function reloadStream(app: string, name: string): Promise<void> {
  await apiFetch<void>(`/streams/${encodeURIComponent(app)}/${encodeURIComponent(name)}/reload`, { method: 'POST' })
}

export async function getStreamDVR(app: string, name: string): Promise<DVRInfo> {
  return apiFetch<DVRInfo>(`/streams/${encodeURIComponent(app)}/${encodeURIComponent(name)}/dvr`)
}

// ---------- ABR Groups ----------

export interface ABRGroup {
  id: string
  name: string
  app_name: string
  stream_name: string
  auto_detect: boolean
  created_at: string
}

export interface ABRVariant {
  id: string
  group_id: string
  stream_name: string
  src_app_name: string
  label: string
  bandwidth: number
  avg_bandwidth: number
  width: number
  height: number
  frame_rate: number
  codecs: string
  audio_only: boolean
  sort_order: number
}

// One row of the ABR group editor. 0 / '' means auto-detect from the live
// stream (avg_bandwidth 0 = omit).
export interface ABRTrack {
  stream_name: string
  bandwidth: number
  avg_bandwidth: number
  resolution: string // "1280x720" or ''
  codecs: string
  audio_only: boolean
}

// Replaces the group's whole track list, in order, in one transaction.
export async function replaceABRTracks(groupId: string, tracks: ABRTrack[]): Promise<void> {
  await apiFetch<void>(`/abr-groups/${encodeURIComponent(groupId)}/tracks`, {
    method: 'PUT',
    body: JSON.stringify({ tracks }),
  })
}

export async function listABRGroups(): Promise<ABRGroup[]> {
  const v = await apiFetch<ABRGroup[] | null>('/abr-groups')
  return v ?? []
}

export async function createABRGroup(data: Partial<ABRGroup>): Promise<ABRGroup> {
  return apiFetch<ABRGroup>('/abr-groups', {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export async function updateABRGroup(id: string, data: Partial<ABRGroup>): Promise<void> {
  await apiFetch<void>(`/abr-groups/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

export async function deleteABRGroup(id: string): Promise<void> {
  await apiFetch<void>(`/abr-groups/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function listABRVariants(groupId: string): Promise<ABRVariant[]> {
  const v = await apiFetch<ABRVariant[] | null>(
    `/abr-groups/${encodeURIComponent(groupId)}/variants`,
  )
  return v ?? []
}

export async function addABRVariant(
  groupId: string,
  data: Partial<ABRVariant>,
): Promise<ABRVariant> {
  return apiFetch<ABRVariant>(`/abr-groups/${encodeURIComponent(groupId)}/variants`, {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export async function updateABRVariant(
  groupId: string,
  vid: string,
  data: Partial<ABRVariant>,
): Promise<void> {
  await apiFetch<void>(
    `/abr-groups/${encodeURIComponent(groupId)}/variants/${encodeURIComponent(vid)}`,
    { method: 'PUT', body: JSON.stringify(data) },
  )
}

export async function deleteABRVariant(groupId: string, vid: string): Promise<void> {
  await apiFetch<void>(
    `/abr-groups/${encodeURIComponent(groupId)}/variants/${encodeURIComponent(vid)}`,
    { method: 'DELETE' },
  )
}

// ---------- Failover ----------

export interface FailoverRule {
  id: string
  app_name: string
  primary_stream: string
  backup_url: string
  backup_protocol: string
  trigger_type: string
  stall_timeout_ms: number
  warmup_ms: number
  enabled: boolean
  auto_restore: boolean
  state: string
}

export async function listFailoverRules(): Promise<FailoverRule[]> {
  const v = await apiFetch<FailoverRule[] | null>('/failover-rules')
  return v ?? []
}

export async function createFailoverRule(
  data: Partial<FailoverRule>,
): Promise<FailoverRule> {
  return apiFetch<FailoverRule>('/failover-rules', {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export async function updateFailoverRule(
  id: string,
  data: Partial<FailoverRule>,
): Promise<void> {
  await apiFetch<void>(`/failover-rules/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

export async function deleteFailoverRule(id: string): Promise<void> {
  await apiFetch<void>(`/failover-rules/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function triggerFailoverRule(id: string): Promise<void> {
  await apiFetch<void>(`/failover-rules/${encodeURIComponent(id)}/trigger`, {
    method: 'POST',
  })
}

// ---------- Pull sources ----------

export interface PullSource {
  id: string
  app_name: string
  stream_name: string
  url: string
  protocol: string
  on_demand: boolean
  reconnect: boolean
  reconnect_delay_s: number
  status: string
  // SRT decryption (protocol === 'srt' only). The real secret is never
  // returned by the API — has_passphrase just says whether one is set.
  // Send `passphrase` to set/replace it, or `clear_passphrase: true` to
  // remove it; omitting both on an update leaves it unchanged.
  has_passphrase?: boolean
  passphrase?: string
  clear_passphrase?: boolean
  pbkeylen?: number
}

export async function listPullSources(): Promise<PullSource[]> {
  const v = await apiFetch<PullSource[] | null>('/pull-sources')
  return v ?? []
}

export async function createPullSource(
  data: Partial<PullSource>,
): Promise<PullSource> {
  return apiFetch<PullSource>('/pull-sources', {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export async function updatePullSource(
  id: string,
  data: Partial<PullSource>,
): Promise<void> {
  await apiFetch<void>(`/pull-sources/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

export async function deletePullSource(id: string): Promise<void> {
  await apiFetch<void>(`/pull-sources/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function startPull(id: string): Promise<void> {
  await apiFetch<void>(`/pull-sources/${encodeURIComponent(id)}/start`, {
    method: 'POST',
  })
}

export async function stopPull(id: string): Promise<void> {
  await apiFetch<void>(`/pull-sources/${encodeURIComponent(id)}/stop`, {
    method: 'POST',
  })
}

// ---------- Users / tokens ----------

export interface User {
  id: string
  email: string
  role: string
  created_at: string
}

export interface APIToken {
  id: string
  name: string
  scopes: string[]
  expires_at?: string
  created_at: string
}

export async function listUsers(): Promise<User[]> {
  const v = await apiFetch<User[] | null>('/users')
  return v ?? []
}

export async function createUser(data: {
  email: string
  password: string
  role: string
}): Promise<User> {
  return apiFetch<User>('/users', { method: 'POST', body: JSON.stringify(data) })
}

export async function updateUser(
  id: string,
  data: Partial<{ email: string; password: string; role: string }>,
): Promise<void> {
  await apiFetch<void>(`/users/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

export async function deleteUser(id: string): Promise<void> {
  await apiFetch<void>(`/users/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function listTokens(userId: string): Promise<APIToken[]> {
  const v = await apiFetch<APIToken[] | null>(
    `/users/${encodeURIComponent(userId)}/tokens`,
  )
  return v ?? []
}

export async function createToken(
  userId: string,
  data: { name: string; scopes: string[] },
): Promise<{ token: string }> {
  return apiFetch<{ token: string }>(
    `/users/${encodeURIComponent(userId)}/tokens`,
    { method: 'POST', body: JSON.stringify(data) },
  )
}

export async function deleteToken(userId: string, tid: string): Promise<void> {
  await apiFetch<void>(
    `/users/${encodeURIComponent(userId)}/tokens/${encodeURIComponent(tid)}`,
    { method: 'DELETE' },
  )
}

// ---------- Node info ----------

export interface NodeInfo {
  node_name: string
  stream_count: number
  version: string
  data_dir?: string
  rtmp_listen?: string
  http_listen?: string
  srt?: {
    enabled?: boolean
    listen?: string
    latency_ms?: number
  }
  hls?: {
    segment_duration?: number
    playlist_size?: number
  }
  dvr?: {
    enabled?: boolean
    storage_path?: string
    window_seconds?: number
  }
}

export async function getNodeInfo(): Promise<NodeInfo> {
  return apiFetch<NodeInfo>('/node')
}

// DVR config update
export async function updateDVRConfig(data: { window_seconds: number; enabled?: boolean }): Promise<void> {
  await apiFetch<void>('/dvr-config', {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

// ---------- DVR ----------

export interface DVRInfo {
  app: string
  stream: string
  oldest_ms: number
  newest_ms: number
  segment_count: number
  total_bytes: number
  window_s: number
  // Playback URL paths (relative to the server origin): hls/dash are the
  // whole rewind window, live; the *_range/*_timeshift entries are
  // templates with {placeholders} to fill in.
  urls?: Record<string, string>
}

// ---------- Applications ----------

export interface Application {
  id: string
  name: string
  publish_key: string
  // RTMP push login (Adobe RTMP authentication); '' = none. The password is
  // write-only: the API reports only whether one is set, and an update
  // without push_password keeps it.
  push_login: string
  has_push_password: boolean
  push_password?: string
  enabled: boolean
  allow_any_stream: boolean
  out_hls: boolean
  out_dash: boolean
  out_mpegts: boolean
  out_ll_hls: boolean
  // DVB-DASH profile signaling + EBU-TT-D subtitles (real-time CEA-608/708
  // decode, not AI-generated) — see internal/dash. Both require
  // config.toml's [dash.dvb] global flags to also be on; these only narrow
  // further, same as every other out_* toggle.
  dvb_dash_enabled: boolean
  dvb_subtitles_enabled: boolean
  hls_seg_duration_s: number   // 0 = use global
  hls_playlist_window: number  // 0 = use global
  // disabled | passthrough | insert | hybrid — see internal/hls.Packager.
  // SetSCTE35Config. "disabled" is the server-side default for any app
  // that hasn't set this explicitly.
  scte35_mode: string
  ad_marker_format: string // none | cue-out-cont | scte35-2022 | daterange
  // nDVR: record this app's streams (fMP4, see internal/dvr) and serve
  // rewind playback. config.toml's [dvr] enabled turns it on everywhere.
  dvr_enabled: boolean
  dvr_window_s: number // rewind window; 0 = global [dvr] window_seconds
  created_at: string
  updated_at: string
}

export interface StreamConfig {
  id: string
  app_id: string
  name_pattern: string
  is_wildcard: boolean
  out_hls: number      // -1=inherit, 0=off, 1=on
  out_dash: number
  out_mpegts: number
  out_ll_hls: number
  dvb_dash_enabled: number      // -1=inherit, 0=off, 1=on
  dvb_subtitles_enabled: number // -1=inherit, 0=off, 1=on
  dvr_enabled: number  // -1=inherit from app, 0=off, 1=on
  dvr_window_s: number // 0 = inherit from app
  abr_group_id: string
  enabled: boolean
  created_at: string
  scte35_mode: string      // '' = inherit from app; else disabled|passthrough|insert|hybrid
  ad_marker_format: string // '' = inherit from app
}

export async function listApplications(): Promise<Application[]> {
  const v = await apiFetch<Application[] | null>('/applications')
  return v ?? []
}

export async function createApplication(data: Partial<Application>): Promise<Application> {
  return apiFetch<Application>('/applications', { method: 'POST', body: JSON.stringify(data) })
}

export async function updateApplication(id: string, data: Partial<Application>): Promise<void> {
  await apiFetch<void>(`/applications/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(data) })
}

export async function deleteApplication(id: string): Promise<void> {
  await apiFetch<void>(`/applications/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function listStreamConfigs(appId: string): Promise<StreamConfig[]> {
  const v = await apiFetch<StreamConfig[] | null>(`/applications/${encodeURIComponent(appId)}/streams`)
  return v ?? []
}

export async function createStreamConfig(appId: string, data: Partial<StreamConfig>): Promise<StreamConfig> {
  return apiFetch<StreamConfig>(`/applications/${encodeURIComponent(appId)}/streams`, {
    method: 'POST', body: JSON.stringify(data),
  })
}

export async function updateStreamConfig(appId: string, sid: string, data: Partial<StreamConfig>): Promise<void> {
  await apiFetch<void>(`/applications/${encodeURIComponent(appId)}/streams/${encodeURIComponent(sid)}`, {
    method: 'PUT', body: JSON.stringify(data),
  })
}

export async function deleteStreamConfig(appId: string, sid: string): Promise<void> {
  await apiFetch<void>(`/applications/${encodeURIComponent(appId)}/streams/${encodeURIComponent(sid)}`, { method: 'DELETE' })
}
