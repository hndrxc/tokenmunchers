import type { NextConfig } from "next";

const RAW = "https://raw.githubusercontent.com/hndrxc/tokenmunchers/main";

const nextConfig: NextConfig = {
  images: { remotePatterns: [{ hostname: "avatars.githubusercontent.com" }, { hostname: "cdn.discordapp.com" }] },
  async rewrites() {
    return [
      { source: "/install.sh", destination: `${RAW}/install-munch.sh` },
      { source: "/install.ps1", destination: `${RAW}/install-munch.ps1` },
    ];
  },
};

export default nextConfig;
