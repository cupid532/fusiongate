// Preserve configured spelling: upstream identifiers can be case-sensitive.
export function parseManualModels(text: string): string[] {
  return [...new Set(text.split(/\r?\n/).map((item) => item.trim()).filter(Boolean))]
}
