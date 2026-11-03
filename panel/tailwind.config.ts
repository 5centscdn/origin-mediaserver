import type { Config } from 'tailwindcss'

// Colors come from CSS variables in src/app/globals.css (space-separated RGB
// channels, so opacity modifiers like bg-primary/15 work). Pages should use
// the semantic names (bg-surface, text-fg-muted, bg-primary, text-danger…).
// The gray, blue and yellow scales are remapped onto the same palette so
// any class not yet migrated still lands on-theme.
const v = (name: string) => `rgb(var(--${name}) / <alpha-value>)`

const config: Config = {
  content: [
    './src/pages/**/*.{js,ts,jsx,tsx,mdx}',
    './src/components/**/*.{js,ts,jsx,tsx,mdx}',
    './src/app/**/*.{js,ts,jsx,tsx,mdx}',
  ],
  theme: {
    extend: {
      colors: {
        canvas: v('bg'),
        sidebar: v('sidebar'),
        surface: v('surface'),
        elevated: v('elevated'),
        border: { DEFAULT: v('border'), strong: v('border-strong') },
        fg: { DEFAULT: v('fg'), secondary: v('fg-secondary'), muted: v('fg-muted') },
        primary: { DEFAULT: v('primary'), hover: v('primary-hover'), fg: v('primary-fg') },
        success: v('success'),
        warning: v('warning'),
        danger: { DEFAULT: v('danger'), strong: v('danger-strong') },
        info: v('info'),
        gray: {
          50: v('gray-50'),
          100: v('gray-100'),
          200: v('gray-200'),
          300: v('gray-300'),
          400: v('gray-400'),
          500: v('gray-500'),
          600: v('gray-600'),
          700: v('gray-700'),
          800: v('gray-800'),
          900: v('gray-900'),
          950: v('gray-950'),
        },
        blue: {
          300: v('blue-300'),
          400: v('blue-400'),
          500: v('blue-500'),
          600: v('blue-600'),
          700: v('blue-700'),
        },
        yellow: {
          200: v('yellow-200'),
          300: v('yellow-300'),
          400: v('yellow-400'),
          500: v('yellow-500'),
          600: v('yellow-600'),
          700: v('yellow-700'),
          900: v('yellow-900'),
        },
      },
      fontFamily: {
        sans: ['var(--font-sans)', 'Inter', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['var(--font-mono)', 'JetBrains Mono', 'ui-monospace', 'SFMono-Regular', 'monospace'],
      },
      fontSize: {
        '2xs': ['0.6875rem', { lineHeight: '1rem' }],
      },
      borderRadius: {
        DEFAULT: '0.375rem',
      },
      boxShadow: {
        pop: '0 10px 30px -10px rgb(0 0 0 / 0.6), 0 0 0 1px rgb(var(--border))',
      },
      keyframes: {
        'toast-in': {
          from: { opacity: '0', transform: 'translateY(8px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
      },
      animation: {
        'toast-in': 'toast-in 150ms ease-out',
      },
    },
  },
  plugins: [],
}

export default config
