'use client'

import React from 'react'

export type BadgeVariant =
  | 'green'
  | 'red'
  | 'yellow'
  | 'blue'
  | 'gray'
  | 'orange'
  | 'purple'

interface BadgeProps {
  variant: BadgeVariant
  children: React.ReactNode
}

const styles: Record<BadgeVariant, string> = {
  green: 'bg-green-500/20 text-green-400 border-green-500/40',
  red: 'bg-red-500/20 text-red-400 border-red-500/40',
  yellow: 'bg-yellow-500/20 text-yellow-400 border-yellow-500/40',
  blue: 'bg-blue-500/20 text-blue-400 border-blue-500/40',
  gray: 'bg-gray-700/40 text-gray-300 border-gray-600/40',
  orange: 'bg-orange-500/20 text-orange-400 border-orange-500/40',
  purple: 'bg-purple-500/20 text-purple-400 border-purple-500/40',
}

export function Badge({ variant, children }: BadgeProps) {
  return (
    <span
      className={
        'inline-flex items-center gap-1 px-2 py-0.5 rounded text-xs font-medium border ' +
        styles[variant]
      }
    >
      {children}
    </span>
  )
}
