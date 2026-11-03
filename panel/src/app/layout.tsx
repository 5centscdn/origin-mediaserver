import type { Metadata } from 'next'
import { Inter, JetBrains_Mono } from 'next/font/google'
import './globals.css'

// next/font downloads the fonts at build time and serves them from the
// static export, so the panel embedded in the binary needs no external
// requests at runtime.
const inter = Inter({ subsets: ['latin'], variable: '--font-sans', display: 'swap' })
const mono = JetBrains_Mono({ subsets: ['latin'], variable: '--font-mono', display: 'swap' })

export const metadata: Metadata = {
  title: 'Origin Media Server',
  description: '5centsCDN Origin Media Server control panel',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${inter.variable} ${mono.variable}`}>
      <body>{children}</body>
    </html>
  )
}
