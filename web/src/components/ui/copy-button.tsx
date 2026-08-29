import * as React from "react"
import { Check, Copy } from "lucide-react"

import { Button, type ButtonProps } from "@/components/ui/button"

export interface CopyButtonProps extends Omit<ButtonProps, "onClick"> {
  value: string
  label?: string
  copiedLabel?: string
  iconOnly?: boolean
  onCopyError?: (error: unknown) => void
}

/**
 * CopyButton centralizes clipboard behavior and gives every copy action a clear,
 * accessible success state. The fallback covers non-secure/local deployments
 * where navigator.clipboard may not be available.
 */
export function CopyButton({
  value,
  label = "复制",
  copiedLabel = "已复制",
  iconOnly = false,
  onCopyError,
  disabled,
  title,
  "aria-label": ariaLabel,
  ...props
}: CopyButtonProps) {
  const [copied, setCopied] = React.useState(false)
  const resetTimer = React.useRef<number | null>(null)

  React.useEffect(
    () => () => {
      if (resetTimer.current != null) window.clearTimeout(resetTimer.current)
    },
    []
  )

  async function copy() {
    if (!value) return
    try {
      await writeClipboard(value)
      setCopied(true)
      if (resetTimer.current != null) window.clearTimeout(resetTimer.current)
      resetTimer.current = window.setTimeout(() => setCopied(false), 1600)
    } catch (error) {
      setCopied(false)
      onCopyError?.(error)
    }
  }

  const accessibleLabel = copied ? copiedLabel : ariaLabel ?? label

  return (
    <Button
      type="button"
      onClick={() => void copy()}
      disabled={disabled || !value}
      aria-label={iconOnly ? accessibleLabel : ariaLabel}
      title={title ?? accessibleLabel}
      {...props}
    >
      {copied ? <Check className="text-primary" /> : <Copy />}
      {!iconOnly && (copied ? copiedLabel : label)}
      {iconOnly && <span className="sr-only">{accessibleLabel}</span>}
    </Button>
  )
}

async function writeClipboard(value: string) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value)
    return
  }

  const textarea = document.createElement("textarea")
  textarea.value = value
  textarea.setAttribute("readonly", "")
  textarea.style.position = "fixed"
  textarea.style.opacity = "0"
  document.body.appendChild(textarea)
  textarea.select()
  try {
    if (!document.execCommand("copy")) throw new Error("clipboard copy was rejected")
  } finally {
    textarea.remove()
  }
}
