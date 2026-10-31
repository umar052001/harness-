import type {
  ActiveRun,
  Harness,
  Health,
  Listing,
  Narration,
  NarrateStatus,
  RunEvent,
  RunRequest,
  RunSummary,
} from './types'

function request(url: string, init?: RequestInit): Promise<Response> {
  return fetch(url, { ...init, signal: AbortSignal.timeout(10000) })
}

export async function fetchHealth(): Promise<Health> {
  const res = await request('/api/health')
  if (!res.ok) throw new Error(`health ${res.status}`)
  return res.json()
}

export async function fetchHarnesses(): Promise<Harness[]> {
  const res = await request('/api/harnesses')
  if (!res.ok) throw new Error(`harnesses ${res.status}`)
  const body = (await res.json()) as { harnesses: Harness[] }
  return body.harnesses
}

export async function fetchWorkflow(): Promise<{models:{id:string;provider:string}[]}> {
  const res=await request('/api/workflow')
  if(!res.ok)throw new Error(`Routed build configuration ${res.status}`)
  return res.json()
}

/** fetchListing describes what a run left behind. */
export async function fetchListing(runId: string): Promise<Listing> {
  const res = await request(`/api/workspace/${encodeURIComponent(runId)}`)
  if (!res.ok) throw new Error(`listing ${res.status}`)
  return res.json()
}

/** fetchRuns lists previous runs, so output stays reachable. */
export async function fetchRuns(): Promise<RunSummary[]> {
  const res = await request('/api/runs')
  if (!res.ok) throw new Error(`runs ${res.status}`)
  const body = (await res.json()) as { runs: RunSummary[] }
  return body.runs
}

/**
 * fetchActive lists runs that are live right now.
 *
 * This is what makes "is something running, and how many?" answerable across
 * tabs and reloads. Workspaces on disk only describe finished work, so without
 * it a second tab - or the same tab after a refresh - had no way to know a run
 * was still going.
 */
export async function fetchActive(): Promise<{ active: ActiveRun[]; limit: number }> {
  const res = await request('/api/runs/active')
  if (!res.ok) throw new Error(`active ${res.status}`)
  return (await res.json()) as { active: ActiveRun[]; limit: number }
}

/**
 * fetchRunEvents replays a run's recorded transcript.
 *
 * Everything the live stream carried is kept on disk, so the Logs view still
 * has something to show after a reload or for a run from days ago. The shape is
 * the same as the stream's, which is what lets history and live output share
 * one renderer.
 */
export async function fetchRunEvents(runId: string): Promise<RunEvent[]> {
  const res = await request(`/api/workspace/${encodeURIComponent(runId)}/events`)
  if (!res.ok) throw new Error(`events ${res.status}`)
  const body = (await res.json()) as { events: RunEvent[] }
  return body.events
}

/** rawURL is the address of one file inside a run's workspace. */
export function rawURL(runId: string, path: string): string {
  const encoded = path
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/')
  return `/api/workspace/${encodeURIComponent(runId)}/raw/${encoded}`
}

/**
 * downloadURL is the address that hands one file over instead of showing it.
 *
 * The same route serves both: a preview is rewritten for display and framed,
 * while a download is the exact bytes the agent wrote. They differ by one query
 * parameter, so there is no second code path to keep in step.
 */
export function downloadURL(runId: string, path: string): string {
  return `${rawURL(runId, path)}?download=1`
}

/**
 * archiveURL is the address of a whole project as a zip.
 *
 * `root` scopes it to part of the project - `dist` for build output - which is
 * what makes "take the site" and "take the source" both one click. Dependency
 * trees and the run's synthetic home are excluded server-side.
 */
export function archiveURL(runId: string, root?: string): string {
  const base = `/api/workspace/${encodeURIComponent(runId)}/archive`
  return root ? `${base}?root=${encodeURIComponent(root)}` : base
}

export interface PublishedSite {
  runId: string
  root: string
  index: string
  urls: string[]
}

/**
 * publishSite gives a run's build output an address of its own.
 *
 * It fails when the run produced no built site, which is worth surfacing: the
 * usual cause is a run that wrote source but never got as far as building it.
 */
export async function publishSite(runId: string): Promise<PublishedSite> {
  const res = await request(`/api/workspace/${encodeURIComponent(runId)}/publish`, { method: 'POST' })
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    throw new Error(text.trim() || `publish ${res.status}`)
  }
  const body = (await res.json()) as { site: PublishedSite }
  return body.site
}

/** fetchPublished is the address a run is already live on, if it has one. */
export async function fetchPublished(runId: string): Promise<string | null> {
  const res = await request(`/api/workspace/${encodeURIComponent(runId)}/publish`)
  if (!res.ok) return null
  const body = (await res.json()) as { published: boolean; site?: PublishedSite }
  return body.published && body.site ? (body.site.urls[0] ?? null) : null
}

/** fetchText reads a workspace file for the code viewer. */
export async function fetchText(runId: string, path: string, limit = 400_000): Promise<string> {
  const res = await request(downloadURL(runId, path))
  if (!res.ok) throw new Error(`read ${res.status}`)
  const text = await res.text()
  return text.length > limit ? `${text.slice(0, limit)}\n… truncated …` : text
}

/**
 * narrate asks the control plane for a one-line status.
 *
 * It is best-effort by construction: the endpoint answers locally when no model
 * key is configured, and any failure here falls back to the browser's own
 * sentence rather than leaving the status line blank.
 */
export async function narrate(st: NarrateStatus): Promise<Narration | null> {
  try {
    const res = await fetch('/api/narrate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(st),
    })
    if (!res.ok) return null
    return (await res.json()) as Narration
  } catch {
    return null
  }
}

export interface StreamResult {
  ok: boolean
  error?: string
}

/**
 * Start a run and deliver every server-sent event to `onEvent`.
 *
 * The run's whole life is one POST /api/run response, framed as SSE. The
 * response ends with `event: stream.end`, whose `ok` field says whether the run
 * succeeded; anything the runner surfaced as an error is in `end.error`.
 */
export async function streamRun(
  req: RunRequest,
  onEvent: (ev: RunEvent) => void,
  signal?: AbortSignal,
): Promise<StreamResult> {
  try {
    return await readRun(req, onEvent, signal)
  } catch (error) {
    return { ok: false, error: signal?.aborted
      ? 'Disconnected from the run. Use Stop run to cancel server-side work.'
      : `Connection lost. Check run history before retrying. ${(error as Error).message}` }
  }
}

async function readRun(req: RunRequest, onEvent: (ev: RunEvent) => void, signal?: AbortSignal): Promise<StreamResult> {
  const res = await fetch('/api/run', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
    signal,
  })
  if (!res.ok) {
    // A 409 from the concurrency guard is JSON with a human "error"; surface
    // that sentence rather than the raw status and brace-noise.
    const text = await res.text().catch(() => '')
    let msg = ''
    try {
      const body = JSON.parse(text) as { error?: string }
      msg = body.error ?? ''
    } catch {
      msg = text
    }
    return { ok: false, error: msg.trim() || `http ${res.status}` }
  }
  if (!res.body) return { ok: false, error: 'no response body' }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let ended: StreamResult = { ok: false, error: 'Connection ended before completion. Check run history before retrying.' }

  try {
  for (;;) {
    const { done, value } = await reader.read()
    buf += decoder.decode(value, { stream: !done })
    let frameEnd: number
    // Frames are blank-line separated.
    while ((frameEnd = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, frameEnd)
      buf = buf.slice(frameEnd + 2)
      const parsed = parseFrame(frame)
      if (!parsed) continue
      const [event, data] = parsed
      if (event === 'stream.end') {
        ended = data as StreamResult
        // Keep reading until the body actually closes so the connection is
        // drained and reusable, but the caller has the verdict already.
      } else {
        onEvent(data as RunEvent)
      }
    }
    if (done) break
  }
  // Tail frame without trailing blank line.
  const parsed = parseFrame(buf)
  if (parsed && parsed[0] === 'stream.end') ended = parsed[1] as StreamResult

    return ended
  } finally {
    reader.releaseLock()
  }
}

export async function stopRun(id: string): Promise<void> {
  const res = await fetch(`/api/runs/${encodeURIComponent(id)}/stop`, { method: 'POST', signal: AbortSignal.timeout(10000) })
  if (!res.ok && res.status !== 404) throw new Error('Could not stop the run. Try again.')
}

function parseFrame(frame: string): [string, unknown] | null {
  if (!frame.trim()) return null
  let event = 'message'
  let data = ''
  for (const line of frame.split('\n')) {
    if (line.startsWith('event: ')) event = line.slice(7).trim()
    else if (line.startsWith('data: ')) data += line.slice(6)
    else if (line === ': ping' || line.startsWith(': ')) continue
  }
  if (!data) return null
  try {
    return [event, JSON.parse(data)]
  } catch {
    return null
  }
}
