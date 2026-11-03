'use client'

import { useState, useEffect } from 'react'
import { useRouter } from 'next/navigation'
import { login, restoreAuth, getToken } from '@/lib/api'
import { FormField, inputClass, ErrorBox } from '@/components/FormHelpers'
import { Button } from '@/components/ui'

export default function LoginPage() {
  const router = useRouter()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    restoreAuth()
    if (getToken()) {
      router.replace('/')
    }
  }, [router])

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setLoading(true)
    try {
      await login(email, password)
      router.replace('/')
    } catch (err: any) {
      setError(err?.message ?? 'Login failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-canvas px-4 py-10">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center text-center">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.svg" alt="5centscdn" className="mb-4 h-10 w-auto" />
          <h1 className="text-xl font-semibold tracking-tight text-fg">Origin Media Server</h1>
          <p className="mt-1 text-sm text-fg-muted">Sign in to the control panel</p>
        </div>

        <form
          onSubmit={onSubmit}
          className="space-y-4 rounded-xl border border-border bg-surface p-6 shadow-pop"
          aria-label="Sign in"
        >
          <FormField label="Email">
            <input
              type="email"
              required
              autoFocus
              autoComplete="username"
              aria-label="Email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className={inputClass}
              placeholder="admin@example.com"
            />
          </FormField>
          <FormField label="Password">
            <input
              type="password"
              required
              autoComplete="current-password"
              aria-label="Password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className={inputClass}
              placeholder="••••••••"
            />
          </FormField>
          <ErrorBox msg={error} />
          <Button type="submit" variant="primary" size="md" loading={loading} className="w-full">
            {loading ? 'Signing in…' : 'Sign in'}
          </Button>
        </form>

        <p className="mt-4 text-center text-xs text-fg-muted">
          Default admin: <span className="font-mono">admin@example.com</span> /{' '}
          <span className="font-mono">admin</span>
        </p>
      </div>
    </div>
  )
}
