import type { NextConfig } from 'next';

// The T05 mock backend lives in files named `*.mock.ts`. A production build
// drops `mock.ts` from pageExtensions, so those route handlers are never
// compiled into the output at all - absent, not gated at runtime. See
// docs/adr/0008-web-ships-no-backend.md.
const isProduction = process.env.NODE_ENV === 'production';

const nextConfig: NextConfig = {
  reactStrictMode: true,
  pageExtensions: isProduction ? ['ts', 'tsx'] : ['ts', 'tsx', 'mock.ts', 'mock.tsx'],
  // The dashboard renders evidence captured from the application under test.
  // It must never be framed, and it must not leak its URL to a target site.
  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'X-Frame-Options', value: 'DENY' },
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'Referrer-Policy', value: 'no-referrer' },
          { key: 'Permissions-Policy', value: 'camera=(), microphone=(), geolocation=()' },
        ],
      },
    ];
  },
};

export default nextConfig;
