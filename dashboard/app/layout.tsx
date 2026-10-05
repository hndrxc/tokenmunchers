import type { Metadata, Viewport } from "next";

import { Taskbar } from "@/components/Taskbar";

import "./globals.css";

export const metadata: Metadata = {
  title: "tokenmunchers",
  description: "Live AI token usage leaderboard for the crew",
};

export const viewport: Viewport = {
  themeColor: "#008080",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <div className="container">{children}</div>
        <Taskbar />
      </body>
    </html>
  );
}
