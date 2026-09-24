import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  output: 'export',
  basePath: '/admin',
  assetPrefix: '/admin/',
  images: {
    unoptimized: true,
  },
  // A fixed build ID keeps static/admin stable when rebuilding unchanged
  // sources (the default is random per build). Chunk names stay content-hashed,
  // and the Go server serves /admin/_next with no-store, so no cache busting relies on it.
  generateBuildId: async () => 'notion2api-admin',
};

export default nextConfig;
