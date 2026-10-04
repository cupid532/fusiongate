/** UI-only label: never use this to change a request name or upstream model. */
export function modelDisplayName(model: string, displayName?: string): string {
  if (displayName?.trim()) return displayName
  const basename = model.slice(model.lastIndexOf("/") + 1)
  return basename || model
}
