import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query"
import "./index.css"
import App from "./App.tsx"
import { AuthProvider } from "./providers/auth.tsx"
import { ThemeProvider } from "./providers/theme.tsx"
import { ConfirmProvider } from "./components/ui/confirm.tsx"
import { Toaster } from "./components/ui/toaster.tsx"
import { isAuthError } from "./lib/api.ts"
import { notify } from "./lib/notify.ts"

function describe(error: unknown): string {
  return error instanceof Error && error.message ? error.message : "请求失败，请重试"
}

const queryClient = new QueryClient({
  // Every write in this console used to fail silently: of roughly seventy
  // mutations only a handful defined onError, so a rejected delete, a failed
  // toggle or a dropped reorder simply did nothing and said nothing. Handling
  // it once on the cache covers all of them, including any added later.
  mutationCache: new MutationCache({
    onError: (error) => {
      // Session loss is already handled globally by lib/api.ts, which bounces
      // the user to the login screen. A toast on top of that is just noise.
      if (isAuthError(error)) return
      notify({ tone: "error", title: "操作失败", description: describe(error) })
    },
  }),
  // Read failures were equally invisible: pages default their data to `[]`, so
  // a failed fetch rendered as "you have no providers" / "no requests yet",
  // which is indistinguishable from the gateway actually being empty.
  queryCache: new QueryCache({
    onError: (error) => {
      if (isAuthError(error)) return
      notify({ tone: "error", title: "数据加载失败", description: describe(error) })
    },
  }),
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
})

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <AuthProvider>
          <ConfirmProvider>
            <App />
            <Toaster />
          </ConfirmProvider>
        </AuthProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>
)
