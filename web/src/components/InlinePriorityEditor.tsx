import { useEffect, useRef, useState } from "react"
import { Check, Pencil, X } from "lucide-react"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"

interface InlinePriorityEditorProps {
  value: number
  onSave: (value: number) => Promise<void>
  disabled?: boolean
}

export function InlinePriorityEditor({ value, onSave, disabled }: InlinePriorityEditorProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(String(value))
  const [saving, setSaving] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (!editing) setDraft(String(value))
  }, [editing, value])

  useEffect(() => {
    if (editing) {
      inputRef.current?.focus()
      inputRef.current?.select()
    }
  }, [editing])

  function cancel() {
    setDraft(String(value))
    setEditing(false)
  }

  async function save() {
    const next = Number(draft)
    if (!Number.isInteger(next) || next < 0) {
      setDraft(String(value))
      return
    }
    if (next === value) {
      setEditing(false)
      return
    }
    setSaving(true)
    try {
      await onSave(next)
      setEditing(false)
    } finally {
      setSaving(false)
    }
  }

  if (!editing) {
    return (
      <button
        type="button"
        disabled={disabled}
        onClick={() => setEditing(true)}
        title="点击修改优先级，数字越大越优先"
        className="group inline-flex items-center gap-1 rounded-md border border-amber-500/25 bg-amber-500/10 px-2 py-1 text-xs font-semibold tabular-nums text-amber-700 transition-colors hover:border-amber-500/50 hover:bg-amber-500/15 disabled:cursor-not-allowed disabled:opacity-50 dark:text-amber-300"
      >
        {value}
        <Pencil className="h-3 w-3 opacity-50 transition-opacity group-hover:opacity-100" />
      </button>
    )
  }

  return (
    <div className="flex items-center gap-1">
      <Input
        ref={inputRef}
        type="number"
        min={0}
        value={draft}
        disabled={saving}
        onChange={(event) => setDraft(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter") void save()
          if (event.key === "Escape") cancel()
        }}
        onBlur={() => void save()}
        aria-label="优先级"
        className="h-7 w-16 px-2 text-center text-xs tabular-nums"
      />
      <Button type="button" variant="ghost" size="icon" className="h-7 w-7" onMouseDown={(event) => event.preventDefault()} onClick={() => void save()} disabled={saving} aria-label="保存优先级">
        <Check className="h-3.5 w-3.5 text-emerald-600" />
      </Button>
      <Button type="button" variant="ghost" size="icon" className="h-7 w-7" onMouseDown={(event) => event.preventDefault()} onClick={cancel} disabled={saving} aria-label="取消修改">
        <X className="h-3.5 w-3.5 text-muted-foreground" />
      </Button>
    </div>
  )
}
