import * as React from "react"
import { AnimatePresence, motion } from "motion/react"
import { Check, Copy } from "lucide-react"

import { Button, type ButtonProps } from "@/components/ui/button"

export interface CopyButtonProps extends Omit<ButtonProps, "onClick"> {
  value: string
  label?: string
  copiedLabel?: string
  iconOnly?: boolean
  onCopyError?: (error: unknown) => void
}

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
      <AnimatePresence mode="wait" initial={false}>
        {copied ? (
          <motion.span
            key="check"
            initial={{ scale: 0.5, opacity: 0 }}
            animate={{ scale: 1, opacity: 1 }}
            exit={{ scale: 0.5, opacity: 0 }}
            transition={{ duration: 0.15 }}
            className="inline-flex"
          >
            <Check className="text-primary" />
          </motion.span>
        ) : (
          <motion.span
            key="copy"
            initial={{ scale: 0.5, opacity: 0 }}
            animate={{ scale: 1, opacity: 1 }}
            exit={{ scale: 0.5, opacity: 0 }}
            transition={{ duration: 0.15 }}
            className="inline-flex"
          >
            <Copy />
          </motion.span>
        )}
      </AnimatePresence>
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
