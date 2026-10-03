/**
 * Extract every .json file from a ZIP archive using only the browser's built-in
 * APIs (no library). Parses the ZIP central directory to locate each entry, then
 * decompresses deflated data via DecompressionStream (available in all modern
 * browsers since Chrome 80 / Safari 16.4 / Firefox 113).
 *
 * Returns the text content of each .json entry. Non-.json files are skipped.
 *
 * Every limit below is a constant on purpose. The central directory's own
 * uncompressed-size field is attacker-controlled data: a ~2 KB archive can
 * declare 0xffffffff, so a ceiling derived from that value bounds nothing. The
 * credential import flow only ever carries a handful of small files, so the caps
 * are fixed and deliberately generous rather than proportional.
 */

/** Refuse an archive larger than this before touching its directory. */
const MAX_ARCHIVE_BYTES = 32 * 1024 * 1024
/** Most .json entries a credential archive may contribute. */
const MAX_ENTRIES = 200
/** Largest decompressed single entry. */
const MAX_ENTRY_BYTES = 8 * 1024 * 1024
/** Largest decompressed payload across every entry of one archive. */
const MAX_TOTAL_BYTES = 32 * 1024 * 1024

const mb = (bytes: number) => Math.round(bytes / 1024 / 1024)

export async function extractJsonFromZip(buffer: ArrayBuffer): Promise<string[]> {
  if (buffer.byteLength > MAX_ARCHIVE_BYTES) {
    throw new Error(`ZIP 文件超过 ${mb(MAX_ARCHIVE_BYTES)} MB 上限`)
  }
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
  if (cdCount > MAX_ENTRIES) throw new Error(`ZIP 条目数超过 ${MAX_ENTRIES} 上限`)
  if (cdOffset > bytes.length) throw new Error("not a valid ZIP archive")

  let pos = cdOffset
  let totalBytes = 0
  for (let i = 0; i < cdCount; i++) {
    // A truncated or hand-edited directory must fail loudly rather than being
    // read past the end of the buffer.
    if (pos + 46 > bytes.length) throw new Error("not a valid ZIP archive")
    if (view.getUint32(pos, true) !== 0x02014b50) break
    const method = view.getUint16(pos + 10, true)
    const compressedSize = view.getUint32(pos + 20, true)
    const nameLen = view.getUint16(pos + 28, true)
    const extraLen = view.getUint16(pos + 30, true)
    const commentLen = view.getUint16(pos + 32, true)
    const localHeaderOffset = view.getUint32(pos + 42, true)
    if (pos + 46 + nameLen + extraLen + commentLen > bytes.length) throw new Error("not a valid ZIP archive")
    const name = new TextDecoder().decode(bytes.subarray(pos + 46, pos + 46 + nameLen))
    pos += 46 + nameLen + extraLen + commentLen

    if (!name.toLowerCase().endsWith(".json")) continue
    if (method !== 0 && method !== 8) continue // only stored or deflate
    if (entries.length >= MAX_ENTRIES) throw new Error(`ZIP 条目数超过 ${MAX_ENTRIES} 上限`)

    // Parse the local file header to find the actual data start.
    if (localHeaderOffset + 30 > bytes.length) throw new Error("not a valid ZIP archive")
    const localNameLen = view.getUint16(localHeaderOffset + 26, true)
    const localExtraLen = view.getUint16(localHeaderOffset + 28, true)
    const dataStart = localHeaderOffset + 30 + localNameLen + localExtraLen
    if (dataStart + compressedSize > bytes.length) throw new Error("not a valid ZIP archive")
    if (compressedSize > MAX_ENTRY_BYTES) {
      throw new Error(`ZIP 中单个文件超过 ${mb(MAX_ENTRY_BYTES)} MB 上限`)
    }
    const raw = bytes.subarray(dataStart, dataStart + compressedSize)

    let text: string
    let entryBytes: number
    if (method === 0) {
      text = new TextDecoder().decode(raw)
      entryBytes = raw.length
    } else {
      const ds = new DecompressionStream("deflate-raw")
      const writer = ds.writable.getWriter()
      const reader = ds.readable.getReader()
      // Feed the payload without awaiting the write promise: a rejecting write
      // (malformed deflate, or a stream cancelled after a limit tripped) would
      // otherwise surface as an unhandled rejection.
      void writer.write(raw).catch(() => {})
      void writer.close().catch(() => {})
      const chunks: Uint8Array[] = []
      let total = 0
      // Release the pipe before unwinding so a rejected archive cannot keep
      // decompressing in the background.
      const abort = async () => {
        try {
          await reader.cancel()
        } catch {
          /* already closed */
        }
      }
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        total += value.length
        // The cap is enforced on bytes actually produced, never on the size the
        // archive claims for itself.
        if (total > MAX_ENTRY_BYTES) {
          await abort()
          throw new Error(`ZIP 中单个文件解压后超过 ${mb(MAX_ENTRY_BYTES)} MB 上限`)
        }
        if (totalBytes + total > MAX_TOTAL_BYTES) {
          await abort()
          throw new Error(`ZIP 解压总量超过 ${mb(MAX_TOTAL_BYTES)} MB 上限`)
        }
        chunks.push(value)
      }
      const merged = new Uint8Array(total)
      let offset = 0
      for (const chunk of chunks) {
        merged.set(chunk, offset)
        offset += chunk.length
      }
      text = new TextDecoder().decode(merged)
      entryBytes = total
    }

    totalBytes += entryBytes
    if (totalBytes > MAX_TOTAL_BYTES) throw new Error(`ZIP 解压总量超过 ${mb(MAX_TOTAL_BYTES)} MB 上限`)
    entries.push(text)
  }
  return entries
}
