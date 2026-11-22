import type { Metadata, Viewport } from "next";

import { ActivityTagPreferencesProvider } from "@/components/ActivityTags";
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
        <div className="container">
          <ActivityTagPreferencesProvider>{children}</ActivityTagPreferencesProvider>
        </div>
        <Taskbar />
      </body>
    </html>
  );
}
