import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { HashRouter, NavLink, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { clearToken, getToken } from "./api/client";
import { ErrorBoundary } from "./components/ui";
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

function Sidebar(props: { onLogout: () => void }) {
  return (
    <aside className="flex w-56 shrink-0 flex-col border-r border-hairline bg-surface">
      <div className="flex items-center gap-2.5 border-b border-hairline px-4 py-3.5">
        <span
          aria-hidden
          className="inline-grid size-6 place-items-center rounded-md"
          style={{ background: "radial-gradient(circle at 50% 45%, var(--color-accent), #7a3d0e)" }}
        >
          <span className="size-2 rounded-full bg-[#fff3e0]" />
        </span>
        <div className="leading-tight">
          <div className="text-[15px] font-semibold tracking-tight">EmberVM</div>
          <div className="font-mono text-[10px] uppercase tracking-[0.16em] text-faint">console</div>
        </div>
      </div>
      <nav className="flex flex-col gap-0.5 p-2">
        {NAV.map((n) => (
          <NavLink
            key={n.to}
            to={n.to}
            end={n.end}
            className={({ isActive }) =>
              `rounded-md px-2.5 py-1.5 text-[13px] font-medium transition-colors ${
                isActive
                  ? "bg-raised text-ink shadow-[inset_2px_0_0_var(--color-accent)]"
                  : "text-muted hover:bg-raised/60 hover:text-ink"
              }`
            }
          >
            {n.label}
          </NavLink>
        ))}
      </nav>
      <div className="mt-auto border-t border-hairline p-3">
        <button
          onClick={props.onLogout}
          className="w-full rounded-md px-2.5 py-1.5 text-left text-xs text-muted hover:bg-raised hover:text-ink"
        >
          Sign out
        </button>
      </div>
    </aside>
  );
}

function Routed() {
  const loc = useLocation();
  return (
    <main className="min-w-0 flex-1 overflow-y-auto">
      <div className="mx-auto max-w-6xl px-8 py-7">
        {/* Keyed by path: a caught render error clears when you navigate. */}
        <ErrorBoundary key={loc.pathname}>
          <Routes>
            <Route path="/" element={<Overview />} />
            <Route path="/sandboxes" element={<Sandboxes />} />
            <Route path="/sandboxes/:id" element={<SandboxDetail />} />
            <Route path="/templates" element={<Templates />} />
            <Route path="/storage" element={<Storage />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </ErrorBoundary>
      </div>
    </main>
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
        <div className="flex h-dvh overflow-hidden">
          <Sidebar
            onLogout={() => {
              clearToken();
              qc.clear();
              setAuthed(false);
            }}
          />
          <Routed />
        </div>
      </HashRouter>
    </QueryClientProvider>
  );
}
