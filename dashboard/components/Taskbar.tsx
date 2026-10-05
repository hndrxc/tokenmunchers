"use client";

import Link from "next/link";
import { useEffect, useState } from "react";

function time(d: Date): string {
  return d.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" });
}

/** Decorative Win95 taskbar: Start goes home, the tray clock ticks. */
export function Taskbar() {
  const [now, setNow] = useState<string>("");
  useEffect(() => {
    const tick = () => setNow(time(new Date()));
    tick();
    const t = setInterval(tick, 10_000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="taskbar">
      <Link href="/" className="start">
        <span className="flag" aria-hidden>
          <i />
          <i />
          <i />
          <i />
        </span>
        Start
      </Link>
      <span className="task">🍪 tokenmunchers</span>
      <span className="tray" aria-label="Clock">
        <span aria-hidden>🔊</span>
        <span suppressHydrationWarning>{now}</span>
      </span>
    </div>
  );
}
