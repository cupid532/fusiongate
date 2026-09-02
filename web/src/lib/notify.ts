// A tiny module-level notification bus.
//
// It exists so that three things which all live *outside* the React tree can
// still raise user-visible feedback:
//
//   1. `lib/api.ts`, when a request comes back 401 and the session is gone.
//   2. The QueryClient's MutationCache/QueryCache error handlers in `main.tsx`,
//      which are constructed before any provider mounts.
//   3. Ordinary components, via the `useToast()` hook.
//
// Routing everything through one bus is what lets a single global handler cover
// every mutation in the app instead of each call site remembering an onError.

export type ToastTone = "success" | "error" | "info"

export type ToastInput = {
  tone?: ToastTone
  title: string
  description?: string
  /** Milliseconds before auto-dismiss. Errors stay until dismissed by default. */
  duration?: number
}

export type Toast = ToastInput & { id: number; tone: ToastTone; duration: number }

type Listener = (toast: Toast) => void

const listeners = new Set<Listener>()
let nextId = 1

const DEFAULT_DURATION: Record<ToastTone, number> = {
  success: 3200,
  info: 4200,
  // Errors are the ones worth reading. Leave them up long enough to act on.
  error: 8000,
}

export function subscribeToasts(listener: Listener): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function notify(input: ToastInput): void {
  const tone = input.tone ?? "info"
  const toast: Toast = {
    ...input,
    tone,
    id: nextId++,
    duration: input.duration ?? DEFAULT_DURATION[tone],
  }
  for (const listener of listeners) listener(toast)
}

export const notifySuccess = (title: string, description?: string) =>
  notify({ tone: "success", title, description })

export const notifyError = (title: string, description?: string) =>
  notify({ tone: "error", title, description })

// ---------------------------------------------------------------------------
// Session loss
// ---------------------------------------------------------------------------

// FusionGate keeps admin sessions in memory, so every gateway restart (i.e.
// every deploy) invalidates them. Without this hook the console kept rendering
// a full, interactive UI whose every request silently 401'd — tables showed
// their "no results" empty state and buttons did nothing at all. AuthProvider
// registers here so a 401 anywhere drops straight back to the login screen.
type UnauthorizedHandler = () => void
let unauthorizedHandler: UnauthorizedHandler | null = null

export function setUnauthorizedHandler(handler: UnauthorizedHandler | null) {
  unauthorizedHandler = handler
}

export function reportUnauthorized() {
  unauthorizedHandler?.()
}
