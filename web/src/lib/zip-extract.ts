/**
 * Extract every .json file from a ZIP archive using only the browser's built-in
 * APIs (no library). Parses the ZIP central directory to locate each entry, then
 * decompresses deflated data via DecompressionStream (available in all modern
 * browsers since Chrome 80 / Safari 16.4 / Firefox 113).
 *
 * Returns the text content of each .json entry. Non-.json files are skipped.
 */

export async function extractJsonFromZip(buffer: ArrayBuffer): Promise<string[]> {
  const view = new DataView(buffer)
  const bytes = new Uint8Array(buffer)
  const entries: string[] = []

  // Find the End of Central Directory record (last 22+ bytes of the file).
  let eocdOffset = -1
  for (let i = bytes.length - 22; i >= 0 && i >= bytes.length - 65557; i--) {
    if (view.getUint32(i, true) === 0x06054b50) {
      eocdOffset = i
      break
    }
  }
  if (eocdOffset < 0) throw new Error("not a valid ZIP archive")

  const cdOffset = view.getUint32(eocdOffset + 16, true)
  const cdCount = view.getUint16(eocdOffset + 10, true)

  let pos = cdOffset
  for (let i = 0; i < cdCount; i++) {
    if (view.getUint32(pos, true) !== 0x02014b50) break
    const method = view.getUint16(pos + 10, true)
    const compressedSize = view.getUint32(pos + 20, true)
    const uncompressedSize = view.getUint32(pos + 24, true)
    const nameLen = view.getUint16(pos + 28, true)
    const extraLen = view.getUint16(pos + 30, true)
    const commentLen = view.getUint16(pos + 32, true)
    const localHeaderOffset = view.getUint32(pos + 42, true)
    const name = new TextDecoder().decode(bytes.subarray(pos + 46, pos + 46 + nameLen))
    pos += 46 + nameLen + extraLen + commentLen

    if (!name.toLowerCase().endsWith(".json")) continue
    if (method !== 0 && method !== 8) continue // only stored or deflate

    // Parse the local file header to find the actual data start.
    const localNameLen = view.getUint16(localHeaderOffset + 26, true)
    const localExtraLen = view.getUint16(localHeaderOffset + 28, true)
    const dataStart = localHeaderOffset + 30 + localNameLen + localExtraLen
    const raw = bytes.subarray(dataStart, dataStart + compressedSize)

    let text: string
    if (method === 0) {
      text = new TextDecoder().decode(raw)
    } else {
      const ds = new DecompressionStream("deflate-raw")
      const writer = ds.writable.getWriter()
      const reader = ds.readable.getReader()
      writer.write(raw)
      writer.close()
      const chunks: Uint8Array[] = []
      let total = 0
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        chunks.push(value)
        total += value.length
        if (total > uncompressedSize * 2 + 1024) throw new Error("decompressed data exceeds expected size")
      }
      const merged = new Uint8Array(total)
      let offset = 0
      for (const chunk of chunks) {
        merged.set(chunk, offset)
        offset += chunk.length
      }
      text = new TextDecoder().decode(merged)
    }
    entries.push(text)
  }
  return entries
}
