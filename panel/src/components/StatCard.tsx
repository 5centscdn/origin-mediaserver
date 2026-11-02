'use client'

import React from 'react'
import clsx from 'clsx'

interface StatCardProps {
  label: string
  value: string | number
  sub?: string
  icon?: React.ReactNode
  color?: 'green' | 'blue' | 'purple' | 'yellow' | 'red'
}

const colorMap = {
  green: { text: 'text-green-400', icon: 'bg-green-500/10 text-green-400' },
  blue: { text: 'text-blue-400', icon: 'bg-blue-500/10 text-blue-400' },
  purple: { text: 'text-purple-400', icon: 'bg-purple-500/10 text-purple-400' },
  yellow: { text: 'text-yellow-400', icon: 'bg-yellow-500/10 text-yellow-400' },
  red: { text: 'text-red-400', icon: 'bg-red-500/10 text-red-400' },
}

export function StatCard({ label, value, sub, icon, color = 'blue' }: StatCardProps) {
  const c = colorMap[color]
  return (
    <div className="rounded-lg border border-gray-800 bg-gray-900 p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="text-xs text-gray-500 font-medium uppercase tracking-wide">
            {label}
          </div>
          <div className={clsx('text-2xl font-bold mt-1', c.text)}>{value}</div>
          {sub && <div className="text-xs text-gray-500 mt-1">{sub}</div>}
        </div>
        {icon && (
          <div
            className={clsx(
              'flex-shrink-0 h-9 w-9 rounded-md flex items-center justify-center',
              c.icon,
            )}
          >
            {icon}
          </div>
        )}
      </div>
    </div>
  )
}
