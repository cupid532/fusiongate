export function reorderProviderIDs(providerIDs: number[], sourceId: number, targetId: number): number[] {
  const ids = [...providerIDs]
  const sourceIndex = ids.indexOf(sourceId)
  const targetIndex = ids.indexOf(targetId)
  if (sourceIndex < 0 || targetIndex < 0 || sourceIndex === targetIndex) return ids
  const [moved] = ids.splice(sourceIndex, 1)
  const insertAt = sourceIndex < targetIndex ? targetIndex - 1 : targetIndex
  ids.splice(insertAt, 0, moved)
  return ids
}
