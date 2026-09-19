import { useCallback, useRef, useState } from "react"
import { X } from "lucide-react"
import { cn } from "@/lib/utils"

/**
 * A tag-input component that stores a comma-separated string.
 *
 * Typing a comma or pressing Enter adds the current text as a tag. Each tag
 * has an X button for removal. The underlying value is always a plain
 * comma-separated string so it can be bound directly to form state.
 */
export function TagInput({
  value,
  onChange,
  placeholder,
  className,
}: {
  value: string
  onChange: (value: string) => void
  placeholder?: string
  className?: string
}) {
  const [input, setInput] = useState("")
  const inputRef = useRef<HTMLInputElement>(null)

  const tags = value
    ? value
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean)
    : []

  const addTag = useCallback(
    (raw: string) => {
      const tag = raw.trim()
      if (!tag) return
      if (tags.includes(tag)) return
      onChange([...tags, tag].join(","))
    },
    [tags, onChange],
  )

  const removeTag = useCallback(
    (index: number) => {
      onChange(tags.filter((_, i) => i !== index).join(","))
    },
    [tags, onChange],
  )

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault()
      addTag(input)
      setInput("")
    }
    if (e.key === "Backspace" && !input && tags.length > 0) {
      removeTag(tags.length - 1)
    }
  }

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value
    // If user pastes something with commas, split it into tags immediately.
    if (v.includes(",")) {
      const parts = v.split(",")
      const last = parts.pop() ?? ""
      for (const p of parts) addTag(p)
      setInput(last)
    } else {
      setInput(v)
    }
  }

  return (
    <div
      className={cn(
        "flex min-h-9 w-full flex-wrap items-center gap-1 rounded-md border border-input bg-transparent px-2 py-1 text-sm shadow-sm transition-colors focus-within:ring-1 focus-within:ring-ring",
        className,
      )}
      onClick={() => inputRef.current?.focus()}
    >
      {tags.map((tag, i) => (
        <span
          key={`${tag}-${i}`}
          className="inline-flex items-center gap-0.5 rounded-md bg-muted px-1.5 py-0.5 text-xs font-medium"
        >
          {tag}
          <button
            type="button"
            className="ml-0.5 rounded-sm text-muted-foreground hover:text-foreground"
            onClick={(e) => {
              e.stopPropagation()
              removeTag(i)
            }}
            aria-label={`移除 ${tag}`}
          >
            <X className="h-3 w-3" />
          </button>
        </span>
      ))}
      <input
        ref={inputRef}
        type="text"
        value={input}
        onChange={handleChange}
        onKeyDown={handleKeyDown}
        onBlur={() => {
          if (input.trim()) {
            addTag(input)
            setInput("")
          }
        }}
        placeholder={tags.length === 0 ? placeholder : undefined}
        className="min-w-[80px] flex-1 bg-transparent py-0.5 text-sm outline-none placeholder:text-muted-foreground"
      />
    </div>
  )
}
