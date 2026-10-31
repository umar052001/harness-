import { createEffect, createMemo, createSignal, For, onCleanup, onMount, Show } from 'solid-js'
import { Portal } from 'solid-js/web'
import {
  archiveURL,
  downloadURL,
  fetchListing,
  fetchPublished,
  fetchRuns,
  fetchText,
  publishSite,
  rawURL,
} from './api'
import type { ActiveRun, Listing, RunSummary } from './types'
import { appStatusLabel, type AppStatus } from './app-status'

/**
 * Device widths the preview can be drawn at.
 *
 * `fill` is 0 rather than a number because "as wide as the space available" is
 * not a width - it is the absence of one. The rest are the three widths a
 * layout is actually reviewed at, and they are deliberately the small ones: a
 * phone is where a page is used, and a preview that only ever shows 1400px
 * hides every problem worth finding.
 */
const PREVIEW_WIDTHS = { fill: 0, phone: 390, tablet: 834, desktop: 1440 } as const
type PreviewWidth = keyof typeof PREVIEW_WIDTHS

/**
 * The artifacts panel: what the run actually produced.
 *
 * The first version of this dashboard streamed events and stopped there, so a
 * user who asked for a website got a transcript and no way to see the website.
 * This panel is the answer, in three parts: a file list, a live preview when
 * there is an HTML file to frame, and a source view for everything else.
 *
 * The listing refreshes on a timer while a run is live. Polling rather than
 * pushing is a deliberate trade: the file tree changes in ways the event stream
 * does not describe (an agent writes a hundred files between two events), and
 * three seconds of staleness is invisible next to the complexity of a second
 * channel.
 */
export default function Artifacts(props: {
  runId: string | null
  running: boolean
  /** Live runs server-side; used to mark options that are still being written. */
  active?: ActiveRun[]
  onFiles?: (n: number) => void
  onSelectRun?: (id: string) => void
}) {
  const [listing, setListing] = createSignal<Listing | null>(null)
  const [runs, setRuns] = createSignal<RunSummary[]>([])
  const [open, setOpen] = createSignal<string | null>(null)
  const [source, setSource] = createSignal<{ path: string; text: string } | null>(null)
  const [error, setError] = createSignal('')
  // Bumped when a run finishes, so the iframe reloads instead of showing the
  // page as it looked when the agent first wrote it.
  const [nonce, setNonce] = createSignal(0)
  // Whether the file list is slid over the stage. It is a drawer rather than a
  // column so the preview keeps the full width the device widths depend on.
  const [filesOpen, setFilesOpen] = createSignal(false)
  // The address this run's site is published on, empty while it is not.
  const [site, setSite] = createSignal('')
  const [publishing, setPublishing] = createSignal(false)
  const [appStatus, setAppStatus] = createSignal<AppStatus>()
  const [appBusy, setAppBusy] = createSignal(false)
  const hasApp = () => listing()?.files.some(f => f.path === 'ai-os.app.json') ?? false
  const [copied, setCopied] = createSignal(false)
  // How wide to draw the preview. "fill" uses the panel; the others are the
  // device widths a design is actually checked at, because a layout that works
  // in a 420px rail says very little about a phone.
  const [width, setWidth] = createSignal<PreviewWidth>('fill')
  // Full-viewport preview. The rail is a good place to glance at a page and a
  // bad place to judge one, so this hands over the whole window.
  const [expanded, setExpanded] = createSignal(false)
  // Whether the preview address actually answers. A missing asset or a build
  // that was never finished should say so rather than showing a blank frame.
  const [previewState, setPreviewState] = createSignal<'checking' | 'ok' | 'error'>('ok')

  let timer: number | undefined

  async function refreshApp() {
    const id = props.runId
    if (!id) return
    try {
      const res = await fetch(`/api/workspace/${id}/app`, { signal: AbortSignal.timeout(5000) })
      if (!res.ok) throw new Error('Runtime status unavailable')
      const app = await res.json()
      if (props.runId === id) setAppStatus(app)
    } catch { if (props.runId === id) setAppStatus(undefined) }
  }

  const refresh = async () => {
    const id = props.runId
    if (!id) return
    try {
      const next = await fetchListing(id)
      if (props.runId !== id) return
      setListing(next)
      setError('')
      // The status line wants to say how much has been produced; this panel is
      // the only place that knows, so it reports up rather than polling twice.
      props.onFiles?.(next.files.length)
      // Keep the reader's selection if it still exists.
      if (open() && !next.files.some((f) => f.path === open())) setOpen(null)
    } catch (e) {
      if (props.runId === id) setError((e as Error).message)
    }
  }

  createEffect(() => {
    // Prior runs are kept on disk, so they have to be offered on load. A run id
    // only lives in this tab's memory, which means without this a reload throws
    // away the output of the run you were watching.
    if (!props.onSelectRun) return
    void fetchRuns()
      .then((rs) => {
        setRuns(rs)
        if (!props.runId && rs.length > 0 && props.onSelectRun) props.onSelectRun(rs[0].id)
      })
      .catch(() => undefined)
  })

  createEffect(() => {
    const id = props.runId
    if (!id) return
    setListing(null)
    setError('')
    setOpen(null)
    setSource(null)
    setSite('')
    setAppStatus(undefined)
    setAppBusy(false)
    void refreshApp()
    void fetchPublished(id)
      .then((u) => { if (props.runId === id) setSite(u ?? '') })
      .catch(() => undefined)
    void refresh()
  })

  // A single persistent poll, started once. It is deliberately *not* recreated
  // when props.active changes: `active` refreshes every three seconds, and an
  // effect that depended on it would tear down and restart the interval on that
  // cadence, so the refresh it guards might never actually fire.
  onMount(() => {
    timer = window.setInterval(() => {
      if (props.runId && (props.running || selectedIsActive())) void refresh()
      if (props.runId && hasApp()) void refreshApp()
    }, 3000)
  })

  // The final refresh fires on the live→finished edge only, so the iframe reloads
  // once when a run completes rather than on every poll.
  let prevRunning = false
  createEffect(() => {
    const live = props.running
    if (prevRunning && !live) {
      // The last poll may have been mid-write when the run ended, and this is
      // the moment the user is looking for the output.
      void refresh().then(() => setNonce((n) => n + 1))
    }
    prevRunning = live
  })

  onCleanup(() => {
    if (timer !== undefined) clearInterval(timer)
    timer = undefined
  })

  const view = async (path: string) => {
    const id = props.runId
    if (!id) return
    if (open() === path) {
      setOpen(null)
      setSource(null)
      return
    }
    setOpen(path)
    setSource(null)
    // An HTML file is something to look at rather than read, so it goes to the
    // frame; anything else is opened as source in the drawer.
    if (path.endsWith('.html') || path.endsWith('.htm')) {
      setFilesOpen(false)
      return
    }
    try {
      const text = await fetchText(id, path)
      if (props.runId === id && open() === path) setSource({ path, text })
    } catch (e) {
      if (props.runId === id && open() === path) setError((e as Error).message)
    }
  }

  const previewPath = createMemo(() => {
    const chosen = open()
    return chosen && /\.html?$/i.test(chosen) ? chosen : listing()?.preview ?? ''
  })

  // Build output gets its own download because it is what most people actually
  // want: a zip of `dist` is the site, ready to drop on any host, while a zip of
  // the project is the source. The directory name is the only signal there is,
  // so it is a short list of names build tools actually use rather than a guess.
  const buildRoot = createMemo(() => {
    const path = previewPath()
    const cut = path.lastIndexOf('/')
    if (cut < 0) return ''
    const dir = path.slice(0, cut)
    const leaf = dir.slice(dir.lastIndexOf('/') + 1)
    return ['dist', 'build', 'out', 'public'].includes(leaf) ? dir : ''
  })
  const previewURL = createMemo(() => {
    // Once published, the panel frames the published address. It is the same
    // bytes a visitor gets, so what is on screen is what the link shows - and
    // root-absolute references resolve on their own, with no rewriting.
    if (site() && !open()) return `${site()}/?v=${nonce()}`
    const id = props.runId
    const path = previewPath()
    if (!id || !path) return ''
    // The nonce defeats the browser cache so a rebuilt page actually appears.
    return `${rawURL(id, path)}?v=${nonce()}`
  })

  // Probe the preview rather than trusting the listing.
  //
  // The listing says which file to frame, which is not the same as saying it
  // loads: a build that never finished, an index that was renamed mid-run, or a
  // published site whose root is gone all produce a frame that is silently
  // blank. One HEAD request turns that into an explanation.
  //
  // Only the raw mount is probed. A published site is on its own origin and
  // deliberately answers without a CORS header, so a cross-origin probe is
  // refused by the browser and would report every working site as broken - the
  // frame itself is the only thing that can see those, and it is what is on
  // screen anyway.
  createEffect(() => {
    const url = previewURL()
    if (!url) return
    if (!url.startsWith('/')) {
      setPreviewState('ok')
      return
    }
    let cancelled = false
    setPreviewState('checking')
    void fetch(url, { method: 'HEAD', signal: AbortSignal.timeout(10000) })
      .then((res) => {
        if (!cancelled) setPreviewState(res.ok ? 'ok' : 'error')
      })
      .catch(() => {
        if (!cancelled) setPreviewState('error')
      })
    onCleanup(() => {
      cancelled = true
    })
  })

  // Reload the frame: a new nonce is a different address, so the browser cannot
  // answer this one from cache.
  const reload = () => {
    setNonce((n) => n + 1)
    void refresh()
  }

  // Closing drops back to the rail, which cannot draw the two large widths. A
  // width the rail has no button for would otherwise survive as a frame
  // scrolled off to one side with nothing showing as selected.
  const closePreview = () => {
    if (width() !== 'fill' && width() !== 'phone') setWidth('fill')
    setExpanded(false)
  }

  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      // Escape is the one key a full-screen overlay has to answer. Anything
      // else would trap the user behind a panel they cannot leave by keyboard.
      if (e.key === 'Escape') closePreview()
    }
    window.addEventListener('keydown', onKey)
    onCleanup(() => window.removeEventListener('keydown', onKey))
  })

  // The page behind an overlay must not scroll under it, or the overlay's own
  // scroll position and the page's disagree and the page appears to move.
  createEffect(() => {
    if (!expanded()) return
    const previous = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    onCleanup(() => {
      document.body.style.overflow = previous
    })
  })

  const frameSandbox = () =>
    site() && !open()
      ? 'allow-scripts allow-same-origin allow-forms allow-popups allow-modals'
      : 'allow-scripts allow-forms allow-popups'

  async function changeApp() {
    const id = props.runId
    if (!id || appBusy()) return
    setAppBusy(true)
    setError('')
    try {
      const res = await fetch(`/api/workspace/${id}/app`, { method: appStatus()?.active ? 'DELETE' : 'POST', signal: AbortSignal.timeout(310000) })
      const text = await res.text()
      if (!res.ok) throw new Error(text)
      const app = JSON.parse(text)
      if (props.runId !== id) return
      setAppStatus(app)
      setSite(app.url)
      setOpen(null)
      setNonce(n => n + 1)
    } catch (e) { if (props.runId === id) setError((e as Error).message) }
    finally { if (props.runId === id) { setAppBusy(false); void refreshApp() } }
  }

  // The drawn width of the frame, in CSS pixels. `fill` is 0, meaning "as wide
  // as the panel", which is the only sensible default in a 420px rail.
  const frameWidth = createMemo(() => PREVIEW_WIDTHS[width()])

  const doPublish = async () => {
    const id = props.runId
    if (!id) return
    setPublishing(true)
    try {
      const published = await publishSite(id)
      if (props.runId !== id) return
      setSite(published.urls[0] ?? '')
      setNonce((n) => n + 1)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setPublishing(false)
    }
  }

  const copyLink = async () => {
    const url = site()
    if (!url) return
    try {
      await navigator.clipboard.writeText(url)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard access is refused in plenty of ordinary contexts. The address
      // is on screen either way, so this is a note rather than a failure.
      setError('clipboard unavailable — the address is below, select it to copy')
    }
  }

  const grouped = createMemo(() => {
    const files = listing()?.files ?? []
    const dirs = new Map<string, typeof files>()
    for (const f of files) {
      const dir = f.path.includes('/') ? f.path.slice(0, f.path.lastIndexOf('/')) : '.'
      const list = dirs.get(dir) ?? []
      list.push(f)
      dirs.set(dir, list)
    }
    return [...dirs.entries()].sort((a, b) => a[0].localeCompare(b[0]))
  })

  // Ids that are live right now, so the run picker can say which entries are
  // still being written rather than presenting them as finished history.
  const activeIds = createMemo(() => new Set((props.active ?? []).map((r) => r.runId)))
  const selectedIsActive = createMemo(() => {
    const id = props.runId
    return id ? activeIds().has(id) : false
  })

  return (
    <section class="relative flex min-h-0 flex-1 flex-col bg-surface-1">
      <div class="flex shrink-0 items-center gap-2 px-3 py-1.5 text-[11px]">
        <Show when={props.onSelectRun && runs().length > 0}>
          <select
            value={props.runId ?? ''}
            onChange={(e) => props.onSelectRun?.(e.currentTarget.value)}
            aria-label="Which run to show"
            class="max-w-[180px] shrink-0 truncate rounded border border-hairline bg-surface-2 px-2 py-1.5 text-[11px] text-zinc-400 outline-none transition hover:border-zinc-600"
          >
            <For each={runs()}>
              {(r) => (
                <option value={r.id}>
                  {new Date(r.modTime).toLocaleString(undefined, {
                    month: 'short',
                    day: 'numeric',
                    hour: '2-digit',
                    minute: '2-digit',
                  })}{' '}
                  · {r.files}f
                  {activeIds().has(r.id) ? ' · running' : ''}
                </option>
              )}
            </For>
          </select>
        </Show>
        <Show when={(listing()?.files.length ?? 0) > 0}>
          {/* The file list is a drawer over the stage rather than a column.
              A permanent third column would leave the preview too narrow for
              the device widths it exists to show. */}
          <button
            type="button"
            onClick={() => setFilesOpen((v) => !v)}
            aria-expanded={filesOpen()}
            class="shrink-0 rounded border border-hairline px-1.5 py-1 text-[10px] uppercase tracking-wider text-zinc-500 transition hover:border-cyan-500/60 hover:text-cyan-300"
          >
            {filesOpen() ? 'close files' : `files · ${listing()!.files.length}`}
          </button>
          <Show when={listing()!.truncated}>
            <span class="mono text-zinc-600">+more</span>
          </Show>
        </Show>
        <span class="flex-1" />
        <Show when={(listing()?.files.length ?? 0) > 0}>
          {/* A real download rather than a click handler: the browser owns the
              transfer, so a large archive does not have to pass through a
              fetch, and the browser can show its own progress. */}
          <Show when={props.runId}>
            <a
              href={archiveURL(props.runId!)}
              title="Download the whole project as a zip (dependency trees excluded)"
              aria-label="Download the whole project as a zip"
              class="shrink-0 rounded border border-hairline px-1.5 py-1 text-[10px] uppercase tracking-wider text-zinc-500 transition hover:border-cyan-500/60 hover:text-cyan-300"
            >
              ↓ zip
            </a>
          </Show>
        </Show>
      </div>

      <Show when={hasApp()}>
          <div class="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
            <button class="quiet-button" disabled={appBusy() || props.running} onClick={() => void changeApp()}>{appBusy() ? 'Working…' : appStatus()?.active ? 'Stop app' : 'Build & start app'}</button>
            <span class="min-w-0 flex-1 break-words" role="status">{appStatusLabel(appStatus())}</span>
          </div>
      </Show>
      <Show when={previewURL()}>
        <div class="flex shrink-0 flex-wrap items-center gap-x-2 gap-y-1 px-3 pb-1.5 text-[10px]">
          <Show
            when={site()}
            fallback={
              <button
                type="button"
                onClick={() => void doPublish()}
                disabled={publishing()}
                class="shrink-0 rounded border border-zinc-700 px-2 py-1.5 uppercase tracking-wider text-zinc-400 transition hover:border-emerald-500/60 hover:text-emerald-300 disabled:opacity-50"
                title="A localhost address on this machine, not an internet deployment"
              >
                {publishing() ? 'preparing…' : 'Create local link'}
              </button>
            }
          >
            <span class="shrink-0 uppercase tracking-wider text-emerald-500/80">local link</span>
          </Show>
          <WidthPicker
            value={width()}
            expanded={false}
            onChange={(w) => {
              setWidth(w)
              setNonce((n) => n + 1)
            }}
          />
          <span class="mono min-w-0 flex-1 truncate text-zinc-600" title={site() || previewPath()}>
            {site() || previewPath()}
          </span>
          {/* These controls carry their own vertical padding: measured at 15px
              without it, they were the smallest targets in the product. */}
          <Show when={site()}>
            <button
              type="button"
              onClick={() => void copyLink()}
              class="hidden shrink-0 rounded px-1.5 py-1.5 text-zinc-500 transition hover:text-emerald-300 sm:block"
            >
              {copied() ? 'copied ✓' : 'copy link'}
            </button>
          </Show>
          <Show when={props.runId && buildRoot()}>
            <a
              href={archiveURL(props.runId!, buildRoot())}
              title={`Download the built site (${buildRoot()}/) as a zip`}
              class="hidden shrink-0 rounded px-1.5 py-1.5 text-zinc-500 transition hover:text-cyan-300 sm:block"
            >
              ↓ site
            </a>
          </Show>
          <Show when={props.runId}>
            <a
              href={downloadURL(props.runId!, previewPath())}
              title="Download this page"
              class="hidden shrink-0 rounded px-1.5 py-1.5 text-zinc-500 transition hover:text-cyan-300 sm:block"
            >
              ↓ page
            </a>
          </Show>
          <button
            type="button"
            onClick={reload}
            title="Reload the preview"
            aria-label="Reload the preview"
            class="grid size-6 shrink-0 place-items-center rounded text-zinc-500 transition hover:text-cyan-300"
          >
            ⟳
          </button>
          <button
            type="button"
            onClick={() => setExpanded(true)}
            title="Open the preview on its own"
            aria-label="Open the preview on its own"
            class="grid size-6 shrink-0 place-items-center rounded text-zinc-500 transition hover:text-cyan-300"
          >
            ⤢
          </button>
          <a
            href={site() || rawURL(props.runId!, previewPath())}
            target="_blank"
            rel="noreferrer"
            class="shrink-0 rounded px-1.5 py-1.5 text-zinc-500 transition hover:text-emerald-300"
          >
            open ↗
          </a>
        </div>
      </Show>

      <Show when={error()}>
        <div class="border-b border-red-500/30 bg-red-500/10 px-3 py-1.5 text-[11px] text-red-300">
          {error()}
        </div>
      </Show>

      <Show
        when={props.runId}
        fallback={
          <div class="flex flex-1 flex-col items-center justify-center gap-1 text-zinc-700">
            <span class="text-xs">no run yet</span>
            <span class="text-[11px] text-zinc-600">files the agent writes appear here</span>
          </div>
        }
      >
        <Show
          when={(listing()?.files.length ?? 0) > 0}
          fallback={
            <div class="empty-result">
              <Show when={props.running} fallback={<><h2>No output files yet</h2><p>Open Activity to see what happened. Adjust your request and start a new run; previous work stays saved.</p></>}>
                <span class="size-3 animate-spin rounded-full border border-zinc-700 border-t-cyan-400" />
                <span class="text-[11px]">waiting for the first file…</span>
              </Show>
            </div>
          }
        >
          <Show when={previewURL()}>
            <div class="flex min-h-0 flex-1 flex-col">
              <Show when={previewState() === 'checking'}>
                <div class="flex items-center gap-1.5 border-b border-hairline/70 px-3 py-1 text-[10px] text-zinc-600">
                  <span class="size-1.5 animate-pulse rounded-full bg-cyan-400/70" />
                  checking the preview
                </div>
              </Show>
              <Show
                when={previewState() !== 'error'}
                fallback={
                  <div class="flex flex-1 flex-col items-center justify-center gap-2 px-6 text-center">
                    <span class="text-[11px] uppercase tracking-wider text-amber-500/80">
                      preview unavailable
                    </span>
                    <span class="text-[11px] leading-relaxed text-zinc-600">
                      {previewPath()} does not load. The run may have written it
                      without finishing, or the build output may have moved.
                    </span>
                    <div class="mt-1 flex items-center gap-3">
                      <button
                        type="button"
                        onClick={reload}
                        class="rounded border border-zinc-700 px-2 py-1.5 text-[10px] uppercase tracking-wider text-zinc-400 transition hover:border-cyan-500/60 hover:text-cyan-300"
                      >
                        retry
                      </button>
                      <Show when={props.runId && previewPath()}>
                        <a
                          href={rawURL(props.runId!, previewPath())}
                          target="_blank"
                          rel="noreferrer"
                          class="text-[10px] uppercase tracking-wider text-zinc-500 transition hover:text-cyan-300"
                        >
                          open the file ↗
                        </a>
                      </Show>
                    </div>
                  </div>
                }
              >
                <PreviewFrame
                  url={previewURL()}
                  sandbox={frameSandbox()}
                  width={frameWidth()}
                  address={site() || previewPath()}
                />
              </Show>
            </div>
          </Show>
          <Show when={!previewURL()}>
            <div class="empty-result"><h2>Your files are ready to explore</h2><p>This project has no static HTML preview yet. Browse its source or download the project to run it locally.</p><button onClick={() => setFilesOpen(true)}>Browse {listing()?.files.length} files</button></div>
          </Show>
        </Show>
      </Show>

      {/* The file drawer slides over the stage's left edge instead of taking a
          column, so the preview keeps the width the device sizes depend on. */}
      <Show when={filesOpen() && (listing()?.files.length ?? 0) > 0}>
        <Portal>
          <div class="absolute inset-y-0 left-0 z-30 flex w-[min(300px,80vw)] flex-col border-r border-hairline bg-surface-1/95 shadow-[18px_0_40px_-12px_rgba(0,0,0,0.8)] backdrop-blur-sm">
            <Show
              when={source()}
              fallback={
                <>
                  <div class="flex shrink-0 items-center gap-2 border-b border-hairline px-3 py-2 text-[10px] uppercase tracking-wider text-zinc-500">
                    <span>files</span>
                    <span class="mono normal-case tracking-normal text-zinc-700">
                      {listing()!.files.length}
                    </span>
                    <span class="flex-1" />
                    <button
                      type="button"
                      onClick={() => setFilesOpen(false)}
                      aria-label="Close the file list"
                      class="text-zinc-500 transition hover:text-zinc-200"
                    >
                      ✕
                    </button>
                  </div>
                  <div class="min-h-0 flex-1 overflow-y-auto py-1">
                    <For each={grouped()}>
                      {([dir, files]) => (
                        <div class="mb-1">
                          <div class="px-3 py-0.5 text-[10px] uppercase tracking-wider text-zinc-700">
                            {dir}
                          </div>
                          <For each={files}>
                            {(f) => (
                              // The row is a container rather than one button
                              // because a download link cannot live inside a
                              // button: nesting interactive elements breaks
                              // both the keyboard order and the click handling.
                              <div
                                class={`flex w-full items-center transition ${
                                  open() === f.path
                                    ? 'bg-emerald-500/10 text-emerald-200'
                                    : 'text-zinc-400 hover:bg-surface-2'
                                }`}
                              >
                                <button
                                  type="button"
                                  onClick={() => void view(f.path)}
                                  class="flex min-w-0 flex-1 items-center gap-2 px-3 py-1.5 text-left text-[12px]"
                                >
                                  <span class="text-zinc-700">{iconFor(f.kind)}</span>
                                  <span class="truncate">{f.path.split('/').pop()}</span>
                                  <span class="mono ml-auto shrink-0 text-[10px] text-zinc-700">
                                    {sizeOf(f.size)}
                                  </span>
                                </button>
                                <Show when={props.runId}>
                                  <a
                                    href={downloadURL(props.runId!, f.path)}
                                    title={`Download ${f.path}`}
                                    aria-label={`Download ${f.path}`}
                                    class="shrink-0 px-2 py-1.5 text-[11px] text-zinc-700 transition hover:text-cyan-300"
                                  >
                                    ↓
                                  </a>
                                </Show>
                              </div>
                            )}
                          </For>
                        </div>
                      )}
                    </For>
                  </div>
                </>
              }
            >
              <div class="flex shrink-0 items-center gap-2 border-b border-hairline px-2 py-1.5">
                <button
                  type="button"
                  onClick={() => setSource(null)}
                  aria-label="Back to the file list"
                  class="shrink-0 rounded px-1.5 py-1 text-[10px] uppercase tracking-wider text-zinc-500 transition hover:bg-surface-2 hover:text-zinc-200"
                >
                  ← files
                </button>
                <span class="mono min-w-0 flex-1 truncate text-[11px] text-zinc-500">
                  {source()!.path}
                </span>
                <Show when={props.runId}>
                  <a
                    href={downloadURL(props.runId!, source()!.path)}
                    title="Download this file"
                    class="shrink-0 rounded px-1.5 py-1 text-[10px] uppercase tracking-wider text-zinc-500 transition hover:text-cyan-300"
                  >
                    ↓
                  </a>
                </Show>
                <button
                  type="button"
                  onClick={() => setFilesOpen(false)}
                  aria-label="Close the file list"
                  class="shrink-0 rounded px-1.5 py-1 text-[10px] text-zinc-500 transition hover:text-zinc-200"
                >
                  ✕
                </button>
              </div>
              <pre class="mono min-h-0 flex-1 overflow-auto p-3 text-[11px] leading-relaxed text-zinc-300">
                {source()!.text}
              </pre>
            </Show>
          </div>
        </Portal>
      </Show>

      {/* The expanded preview is portalled to the body rather than rendered
          here. A `fixed` overlay inside the panel would be positioned against
          the panel's own containing block, which on narrow screens is a
          transformed drawer - the overlay would be clipped to a drawer that is
          itself off screen. */}
      <Show when={expanded() && previewURL()}>
        <Portal>
          <div
            class="fixed inset-0 z-50 flex flex-col bg-surface-0"
            role="dialog"
            aria-modal="true"
            aria-label="Preview"
          >
            <div class="flex shrink-0 items-center gap-2 border-b border-hairline px-3 py-2">
              <span class="text-[10px] font-semibold uppercase tracking-wider text-zinc-500">
                preview
              </span>
              <WidthPicker
                value={width()}
                expanded
                onChange={(w) => {
                  setWidth(w)
                  setNonce((n) => n + 1)
                }}
              />
              <span class="mono min-w-0 flex-1 truncate text-[11px] text-zinc-600">
                {site() || previewPath()}
              </span>
              <button
                type="button"
                onClick={reload}
                title="Reload the preview"
                aria-label="Reload the preview"
                class="shrink-0 rounded border border-hairline px-2 py-1.5 text-[10px] uppercase tracking-wider text-zinc-500 transition hover:border-cyan-500/60 hover:text-cyan-300"
              >
                reload
              </button>
              <Show when={site()}>
                <a
                  href={site()}
                  target="_blank"
                  rel="noreferrer"
                  class="shrink-0 rounded border border-hairline px-2 py-1.5 text-[10px] uppercase tracking-wider text-zinc-500 transition hover:border-emerald-500/60 hover:text-emerald-300"
                >
                  open ↗
                </a>
              </Show>
              <button
                type="button"
                onClick={closePreview}
                title="Close (Esc)"
                aria-label="Close the preview"
                class="shrink-0 rounded border border-hairline px-2 py-1.5 text-[10px] uppercase tracking-wider text-zinc-400 transition hover:border-zinc-600 hover:text-zinc-100"
              >
                close
              </button>
            </div>
            <Show
              when={previewState() !== 'error'}
              fallback={
                <div class="flex flex-1 flex-col items-center justify-center gap-2 px-6 text-center">
                  <span class="text-[11px] uppercase tracking-wider text-amber-500/80">
                    preview unavailable
                  </span>
                  <span class="text-[11px] text-zinc-600">{previewPath()} does not load.</span>
                  <button
                    type="button"
                    onClick={reload}
                    class="mt-1 rounded border border-zinc-700 px-2 py-0.5 text-[10px] uppercase tracking-wider text-zinc-400 transition hover:border-cyan-500/60 hover:text-cyan-300"
                  >
                    retry
                  </button>
                </div>
              }
            >
              <PreviewFrame
                url={previewURL()}
                sandbox={frameSandbox()}
                width={frameWidth()}
                address={site() || previewPath()}
                padded
              />
            </Show>
          </div>
        </Portal>
      </Show>
    </section>
  )
}

/**
 * PreviewFrame is the run's page inside a browser-shaped frame.
 *
 * The chrome is not decoration for its own sake: an address bar showing the
 * real path is what tells you *which* page you are looking at, which matters
 * when a project has several entry points and the panel picked one.
 */
function PreviewFrame(props: {
  url: string
  sandbox: string
  width: number
  address: string
  padded?: boolean
}) {
  return (
    <div class={`flex min-h-0 flex-1 justify-center overflow-auto bg-surface-0 ${props.padded ? 'p-4' : ''}`}>
      <div
        class="flex min-h-0 w-full flex-col overflow-hidden rounded-md border border-hairline bg-surface-2 shadow-[0_0_0_1px_rgba(0,0,0,0.4),0_18px_50px_-12px_rgba(0,0,0,0.8)]"
        // A device width is a real constraint, not a scale factor: a phone
        // layout has to lay out at 390px, so the frame is drawn that narrow and
        // the viewport around it scrolls. Scaling a wide frame down instead
        // would show a desktop layout at phone size and prove nothing.
        style={{ width: props.width ? `${props.width}px` : undefined }}
      >
        <div class="flex shrink-0 items-center gap-1.5 border-b border-hairline/80 bg-surface-2/80 px-2.5 py-1.5">
          <span class="size-1.5 rounded-full bg-zinc-700" />
          <span class="size-1.5 rounded-full bg-zinc-700" />
          <span class="size-1.5 rounded-full bg-zinc-700" />
          <span class="mono ml-1.5 min-w-0 flex-1 truncate rounded-sm bg-black/40 px-2 py-0.5 text-[10px] text-zinc-500">
            {props.address}
          </span>
          <Show when={props.width}>
            <span class="mono shrink-0 text-[10px] text-zinc-700">{props.width}</span>
          </Show>
        </div>
        <iframe
          title="preview"
          src={props.url}
          // The sandbox is deliberately different per source, and the reason
          // is the document's origin rather than the run's trustworthiness.
          //
          // The raw mount is same-origin with the dashboard, so it is framed
          // without allow-same-origin: that opaque origin is what stops
          // agent-authored HTML from reaching this page's DOM and API. Its
          // subresources still load because the raw handler answers with
          // "Access-Control-Allow-Origin: null".
          //
          // A published site is already on its own origin, one that cannot
          // see the dashboard either way, so framing it with an opaque
          // origin buys no isolation and costs correctness: its stylesheet
          // and script requests become cross-origin and fail without a CORS
          // header the published route deliberately does not send.
          sandbox={props.sandbox}
          referrerpolicy="no-referrer"
          class="min-h-0 w-full flex-1 border-0 bg-white"
        />
      </div>
    </div>
  )
}

/**
 * WidthPicker chooses the width the preview is drawn at.
 *
 * The two large widths are offered only in the expanded view. In a 420px rail
 * they cannot be shown without the frame shrinking to fit, which would show a
 * desktop layout at phone size - the exact illusion this control exists to
 * prevent.
 */
function WidthPicker(props: {
  value: PreviewWidth
  expanded: boolean
  onChange: (w: PreviewWidth) => void
}) {
  const options = (): PreviewWidth[] =>
    props.expanded ? ['fill', 'phone', 'tablet', 'desktop'] : ['fill', 'phone']
  return (
    <div class="flex shrink-0 overflow-hidden rounded border border-hairline" role="group" aria-label="Preview width">
      <For each={options()}>
        {(w) => (
          <button
            type="button"
            aria-pressed={props.value === w}
            onClick={() => props.onChange(w)}
            title={w === 'fill' ? 'Fit the panel' : `${PREVIEW_WIDTHS[w]}px`}
            class={`px-2 py-1.5 text-[10px] uppercase tracking-wider transition ${
              props.value === w
                ? 'bg-zinc-800 text-cyan-300'
                : 'text-zinc-600 hover:text-zinc-300'
            }`}
          >
            {w === 'fill' ? 'fit' : w}
          </button>
        )}
      </For>
    </div>
  )
}

function iconFor(kind: string): string {
  switch (kind) {
    case 'html':
      return '◈'
    case 'css':
      return '❋'
    case 'js':
      return '⌘'
    case 'ts':
      return '⌁'
    case 'json':
      return '{}'
    case 'text':
      return '≡'
    case 'image':
      return '▣'
    default:
      return '·'
  }
}

function sizeOf(n: number): string {
  if (n < 1024) return `${n}B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)}k`
  return `${(n / 1024 / 1024).toFixed(1)}M`
}
