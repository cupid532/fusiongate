import * as React from "react"

import { cn } from "@/lib/utils"

type InputProps = React.ComponentProps<"input"> & {
  /** Override password-manager detection for auth fields or configuration secrets. */
  passwordManager?: boolean
}

const authAutoCompleteTokens = new Set([
  "username", "email", "current-password", "new-password", "one-time-code", "webauthn",
])

const Input = React.forwardRef<HTMLInputElement, InputProps>(
  ({ className, type, autoComplete, passwordManager, ...props }, ref) => {
    // Use explicit auth semantics, not masking: revealed passwords remain auth fields,
    // while masked API keys and other configuration values are not website passwords.
    const managed = passwordManager ?? autoComplete?.toLowerCase().split(/\s+/).some((token) => authAutoCompleteTokens.has(token)) ?? false
    return (
      <input
        type={type}
        autoComplete={autoComplete ?? (managed ? undefined : "off")}
        data-1p-ignore={managed ? undefined : "true"}
        data-lpignore={managed ? undefined : "true"}
        className={cn(
          "flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
          className
        )}
        ref={ref}
        {...props}
      />
    )
  }
)
Input.displayName = "Input"

export { Input }
