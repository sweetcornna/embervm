import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { HashRouter, NavLink, Navigate, Route, Routes } from "react-router-dom";
import { clearToken, getToken } from "./api/client";
import { Login } from "./pages/Login";
import { Overview } from "./pages/Overview";
import { SandboxDetail } from "./pages/SandboxDetail";
import { Sandboxes } from "./pages/Sandboxes";
import { Storage } from "./pages/Storage";
import { Templates } from "./pages/Templates";

const qc = new QueryClient({
  defaultOptions: { queries: { retry: 1, staleTime: 2000 } },
});

const NAV = [
  { to: "/", label: "Overview", end: true },
  { to: "/sandboxes", label: "Sandboxes" },
  { to: "/templates", label: "Templates" },
  { to: "/storage", label: "Storage" },
];

function Shell(props: { onLogout: () => void; children: React.ReactNode }) {
  return (
    <div className="flex min-h-dvh">
      <aside className="flex w-52 shrink-0 flex-col border-r border-hairline bg-surface">
        <div className="flex items-center gap-2.5 px-4 py-4">
          <span className="ember-live inline-block size-2.5 rounded-full bg-ember" aria-hidden />
          <div>
            <div className="font-display text-[15px] font-bold leading-tight tracking-wide">EmberVM</div>
            <div className="font-mono text-[10px] uppercase tracking-[0.18em] text-faint">console · 余烬</div>
          </div>
        </div>
        <nav className="mt-2 flex flex-col gap-0.5 px-2">
          {NAV.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end}
              className={({ isActive }) =>
                `rounded px-2.5 py-1.5 text-sm ${
                  isActive ? "bg-raised font-medium text-ember" : "text-muted hover:bg-raised hover:text-ink"
                }`
              }
            >
              {n.label}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto p-3">
          <button
            onClick={props.onLogout}
            className="w-full rounded border border-border px-2.5 py-1.5 text-left text-xs text-muted hover:bg-raised hover:text-ink"
          >
            Sign out
          </button>
        </div>
      </aside>
      <main className="min-w-0 flex-1 px-6 py-5">{props.children}</main>
    </div>
  );
}

export default function App() {
  const [authed, setAuthed] = useState(() => Boolean(getToken()));
  useEffect(() => {
    const onUnauthorized = () => setAuthed(false);
    window.addEventListener("embervm:unauthorized", onUnauthorized);
    return () => window.removeEventListener("embervm:unauthorized", onUnauthorized);
  }, []);

  if (!authed) return <Login onDone={() => setAuthed(true)} />;

  return (
    <QueryClientProvider client={qc}>
      <HashRouter>
        <Shell
          onLogout={() => {
            clearToken();
            qc.clear();
            setAuthed(false);
          }}
        >
          <Routes>
            <Route path="/" element={<Overview />} />
            <Route path="/sandboxes" element={<Sandboxes />} />
            <Route path="/sandboxes/:id" element={<SandboxDetail />} />
            <Route path="/templates" element={<Templates />} />
            <Route path="/storage" element={<Storage />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </Shell>
      </HashRouter>
    </QueryClientProvider>
  );
}
