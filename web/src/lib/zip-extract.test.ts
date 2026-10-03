import { deflateRawSync } from "node:zlib"
import { describe, expect, it } from "vitest"
import { extractJsonFromZip } from "./zip-extract"

type Entry = {
  name: string
  data: Buffer
  /** Compress with deflate instead of storing verbatim. */
  deflate?: boolean
  /** Override the size written into both size fields, to forge a claim. */
  declaredSize?: number
}

function concat(parts: Uint8Array[]): Buffer {
  return Buffer.concat(parts.map((part) => Buffer.from(part)))
}

function localHeader(entry: Entry, payload: Buffer): Buffer {
  const name = Buffer.from(entry.name, "utf8")
  const header = Buffer.alloc(30)
  header.writeUInt32LE(0x04034b50, 0)
  header.writeUInt16LE(20, 4)
  header.writeUInt16LE(entry.deflate ? 8 : 0, 8)
  header.writeUInt32LE(payload.length, 18)
  header.writeUInt32LE(entry.declaredSize ?? entry.data.length, 22)
  header.writeUInt16LE(name.length, 26)
  return concat([header, name, payload])
}

function centralHeader(entry: Entry, payload: Buffer, offset: number): Buffer {
  const name = Buffer.from(entry.name, "utf8")
  const header = Buffer.alloc(46)
  header.writeUInt32LE(0x02014b50, 0)
  header.writeUInt16LE(20, 4)
  header.writeUInt16LE(20, 6)
  header.writeUInt16LE(entry.deflate ? 8 : 0, 10)
  header.writeUInt32LE(payload.length, 20)
  header.writeUInt32LE(entry.declaredSize ?? entry.data.length, 24)
  header.writeUInt16LE(name.length, 28)
  header.writeUInt32LE(offset, 42)
  return concat([header, name])
}

function zip(entries: Entry[]): ArrayBuffer {
  const locals: Buffer[] = []
  const centrals: Buffer[] = []
  let offset = 0
  for (const entry of entries) {
    const payload = entry.deflate ? deflateRawSync(entry.data) : entry.data
    const local = localHeader(entry, payload)
    centrals.push(centralHeader(entry, payload, offset))
    locals.push(local)
    offset += local.length
  }
  const directory = concat(centrals)
  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50, 0)
  end.writeUInt16LE(entries.length, 8)
  end.writeUInt16LE(entries.length, 10)
  end.writeUInt32LE(directory.length, 12)
  end.writeUInt32LE(offset, 16)
  const archive = concat([...locals, directory, end])
  return archive.buffer.slice(archive.byteOffset, archive.byteOffset + archive.byteLength) as ArrayBuffer
}

describe("extractJsonFromZip", () => {
  it("extracts stored and deflated .json entries and skips everything else", async () => {
    const entries = await extractJsonFromZip(
      zip([
        { name: "codex.json", data: Buffer.from('{"kind":"oauth"}') },
        { name: "notes.txt", data: Buffer.from("ignore me") },
        { name: "claude.json", data: Buffer.from('{"kind":"oauth","platform":"claude"}'), deflate: true },
      ]),
    )
    expect(entries).toEqual(['{"kind":"oauth"}', '{"kind":"oauth","platform":"claude"}'])
  })

  it("caps one entry's output at the fixed limit even when the archive claims 4 GiB", async () => {
    // 9 MiB of one repeated byte: ~9 KB compressed, far past the 8 MiB ceiling.
    const oversized = Buffer.alloc(9 * 1024 * 1024, 0x61)
    await expect(
      extractJsonFromZip(zip([{ name: "bomb.json", data: oversized, deflate: true, declaredSize: 0xffffffff }]))
    ).rejects.toThrow(/解压后超过|解压总量超过/)
  })

  it("does not let a small declared size reject a legitimate larger payload", async () => {
    // The old bound was `declared * 2 + 1024`, so a wrong (or hostile) claim in
    // either direction decided what the browser was allowed to decompress.
    const payload = Buffer.from(JSON.stringify({ token: "x".repeat(4096) }))
    const entries = await extractJsonFromZip(zip([{ name: "big.json", data: payload, deflate: true, declaredSize: 16 }]))
    expect(entries).toHaveLength(1)
    expect(entries[0]).toBe(payload.toString("utf8"))
  })

  it("refuses an archive with more than 200 entries", async () => {
    const many = Array.from({ length: 201 }, (_, i) => ({ name: `f${i}.json`, data: Buffer.from("{}") }))
    await expect(extractJsonFromZip(zip(many))).rejects.toThrow(/条目数超过/)
  })

  it("refuses an archive whose central directory points past the file", async () => {
    const archive = Buffer.from(zip([{ name: "a.json", data: Buffer.from("{}") }]))
    // Corrupt the EOCD's central-directory offset.
    const eocd = archive.length - 22
    archive.writeUInt32LE(archive.length + 4096, eocd + 16)
    await expect(
      extractJsonFromZip(archive.buffer.slice(archive.byteOffset, archive.byteOffset + archive.byteLength) as ArrayBuffer)
    ).rejects.toThrow(/not a valid ZIP archive/)
  })

  it("refuses input that is not a ZIP at all", async () => {
    await expect(extractJsonFromZip(new Uint8Array(64).buffer)).rejects.toThrow(/not a valid ZIP archive/)
  })
})
