// Formatting helpers used throughout the panel.

export function formatBytes(n: number): string {
  if (!n || !isFinite(n) || n < 0) return '0 B'
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB', 'PB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)} ${units[i]}`
}

export function formatBitrate(bps: number): string {
  if (!bps || !isFinite(bps) || bps < 0) return '0 bps'
  if (bps < 1000) return `${bps} bps`
  const units = ['kbps', 'Mbps', 'Gbps']
  let v = bps / 1000
  let i = 0
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  return `${v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)} ${units[i]}`
}

export function formatUptime(s: number): string {
  if (!s || !isFinite(s) || s < 0) return '0s'
  s = Math.floor(s)
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const parts: string[] = []
  if (d) parts.push(`${d}d`)
  if (h) parts.push(`${h}h`)
  if (m) parts.push(`${m}m`)
  if (sec || parts.length === 0) parts.push(`${sec}s`)
  return parts.join(' ')
}

export function formatDate(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

export function formatRelativeMs(ms: number): string {
  if (!ms || !isFinite(ms)) return '—'
  const d = new Date(ms)
  if (isNaN(d.getTime())) return '—'
  return d.toLocaleString()
}

export function formatDuration(ms: number): string {
  if (!ms || !isFinite(ms) || ms < 0) return '0s'
  return formatUptime(Math.floor(ms / 1000))
}

// Copies text to the clipboard, working even when this page isn't served
// over a "secure context" (HTTPS or localhost) — this panel is commonly
// reached over plain HTTP by IP (e.g. http://<host>:8081), where
// `navigator.clipboard` is undefined entirely in Chrome/most browsers, so
// calling `.writeText()` on it throws synchronously rather than rejecting
// a promise — a bare `.catch()` around it never even runs, silently
// breaking every "Copy" button. Falls back to the older
// document.execCommand('copy') path (via a temporary offscreen textarea),
// which has no such secure-context restriction. Returns whether the copy
// actually succeeded, so callers can show real success/failure instead of
// optimistically assuming it worked.
export async function copyToClipboard(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // fall through to the execCommand fallback below
    }
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.top = '-1000px'
    ta.style.left = '-1000px'
    document.body.appendChild(ta)
    ta.focus()
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}

// isStreamPattern / matchesStreamPattern mirror the server's stream config
// patterns (internal/db.MatchStreamPattern): "*" globs and FMLE "%i" (one or
// more digits); anything else is an exact name.
export function isStreamPattern(name: string): boolean {
  return name.includes('%i') || name.includes('*')
}

export function matchesStreamPattern(pattern: string, name: string): boolean {
  if (!isStreamPattern(pattern)) return pattern === name
  const re = pattern
    .split(/(\*|%i)/)
    .map((p) => (p === '*' ? '.*' : p === '%i' ? '[0-9]+' : p.replace(/[.+?^${}()|[\]\\]/g, '\\$&')))
    .join('')
  return new RegExp('^' + re + '$').test(name)
}

// listenPort turns a listen address ("0.0.0.0:1935", ":4000") into the
// ":port" part shown after <host> in publish URLs; the bind address isn't
// what an encoder connects to.
export function listenPort(listen: string | undefined, fallback: string): string {
  if (!listen) return fallback
  const i = listen.lastIndexOf(':')
  return i >= 0 ? listen.slice(i) : fallback
}

// redactUrl hides the password in a URL's userinfo for display
// ("rtsp://admin:secret@cam/1" → "rtsp://admin:***@cam/1"). Pull and
// failover source URLs can carry camera credentials; tables and tooltips
// show the redacted form (edit forms still hold the real value).
export function redactUrl(raw: string): string {
  if (!raw) return raw
  return raw.replace(/^([a-z][a-z0-9+.-]*:\/\/)([^/@:]*):[^/@]*@/i, '$1$2:***@')
}
