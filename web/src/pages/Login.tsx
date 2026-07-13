import { useState } from "react";
import { ApiError, api, setToken } from "../api/client";
import { Button, ErrorNote, Field, inputCls } from "../components/ui";

export function Login(props: { onDone: () => void }) {
  const [token, setTok] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function connect(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setToken(token.trim());
    try {
      await api("GET", "/templates"); // cheapest authenticated probe
      props.onDone();
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 401
          ? new Error("Token rejected. Tokens are defined in the apiserver's --tokens-file.")
          : err,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-dvh place-items-center px-4">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-3">
          <span
            aria-hidden
            className="inline-grid size-8 place-items-center rounded-lg"
            style={{ background: "radial-gradient(circle at 50% 45%, var(--color-accent), #7a3d0e)" }}
          >
            <span className="size-2.5 rounded-full bg-[#fff3e0]" />
          </span>
          <div className="leading-tight">
            <h1 className="text-lg font-semibold tracking-tight">EmberVM console</h1>
            <p className="font-mono text-[11px] uppercase tracking-[0.16em] text-faint">
              sandbox cloud · operator
            </p>
          </div>
        </div>
        <form onSubmit={connect} className="rounded-lg border border-border bg-surface p-6">
          <div className="space-y-4">
            <Field label="API token" hint="Sent as a Bearer token to this host's /v0 API.">
              <input
                className={inputCls}
                type="password"
                autoFocus
                autoComplete="off"
                value={token}
                onChange={(e) => setTok(e.target.value)}
                placeholder="dev-token"
              />
            </Field>
            <ErrorNote error={error} />
            <Button kind="primary" type="submit" busy={busy} disabled={!token.trim()}>
              Connect
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
