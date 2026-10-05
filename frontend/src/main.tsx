/**
 * SPA bootstrap composition root (Blueprint §6).
 *
 * Only provider assembly: a single module-scope QueryClient (StrictMode
 * double-invoke safe — created exactly once) wrapped around the App shell.
 * Styles are imported first so design tokens and base rules resolve before the
 * first paint, preventing FOUC/CLS. All session, error-boundary, toast, and
 * route concerns live in App.tsx — nothing is duplicated here.
 */
import "./styles/tokens.css";
import "./styles/global.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import {
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { ReactQueryDevtools } from "@tanstack/react-query-devtools";
import { App } from "./App";

const RETRY_MAX = 3;

/**
 * Do not auto-retry auth/validation/rate-limit failures; retrying a 401 would
 * hammer the auth endpoint (brute-force/lockout risk) and retrying a 429
 * defeats the gateway's backoff. Retry only network errors (no status) and
 * transient 5xx, capped at RETRY_MAX attempts.
 */
type RetriableError = Error & { response?: { status?: number } | undefined; status?: number };
const shouldRetry = (failureCount: number, error: Error): boolean => {
  const err = error as RetriableError;
  const status = err.response?.status ?? err.status;
  if (typeof status === "number") {
    if (status >= 500) return failureCount < RETRY_MAX;
    return false; // 4xx incl. 401/403/429 — never retried
  }
  return failureCount < RETRY_MAX; // network error, no HTTP status
};

/**
 * Exponential backoff, 100 ms base, factor 2, jittered (API §7.7). Capped so a
 * failing query surfaces its final error state promptly instead of hanging.
 */
const retryDelay = (attempt: number): number =>
  Math.min(3200, Math.round(100 * 2 ** attempt) * (0.8 + Math.random() * 0.4));

/**
 * Shared client so tests/devtools can assert the same default options.
 * staleTime 30 s keeps recently viewed ledger lists from re-flashing on
 * re-fetch (recognition over recall); mutations are never auto-retried client
 * side to avoid duplicate financial writes (idempotency is server-enforced).
 */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: shouldRetry,
      retryDelay,
      refetchOnWindowFocus: true,
    },
    mutations: {
      retry: 0,
    },
  },
});

const container = document.getElementById("root");

if (container) {
  createRoot(container).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <App />
        {import.meta.env.DEV && <ReactQueryDevtools initialIsOpen={false} />}
      </QueryClientProvider>
    </StrictMode>,
  );
} else {
  // Missing #root (server-HTML mismatch): fail through a generic message, not a
  // raw createRoot stack trace. No error internals are surfaced to the user.
  console.error("FMS bootstrap failed: #root element not found");
  document.body.textContent =
    "The application could not start. Please reload to continue.";
}