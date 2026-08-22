import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Traces the modules actually imported and emits a self-contained server, so
  // the runtime image carries neither the full node_modules tree nor the build
  // toolchain. See web/Dockerfile.
  output: "standalone",
  // The compiler API avoids detached TypeScript CLI processes, which are
  // blocked by some hardened CI and container build environments.
  experimental: {
    useTypeScriptCli: false,
  },
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
          {
            key: "Permissions-Policy",
            value: "camera=(), microphone=(), geolocation=(), payment=()",
          },
        ],
      },
    ];
  },
};

export default nextConfig;
