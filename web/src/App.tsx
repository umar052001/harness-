import { createEffect, createMemo, createSignal, For, onCleanup, Show } from 'solid-js'
import Activity from './Activity'
import Artifacts from './Artifacts'
import ChatBar from './ChatBar'
import SettingsDrawer from './Settings'
import { build, localSentence } from './events'
import { PORTFOLIO_PROMPT } from './demo'
import { fetchActive, fetchHarnesses, fetchHealth, fetchRunEvents, narrate, streamRun } from './api'
import type { ActiveRun, Harness, Health, RunEvent } from './types'

/**
 * preferOrder is the default ranking when more than one agent is installed.
 *
 * The question "which agent should this run under" has a sensible answer that
 * does not depend on the user having read the catalogue: start with the agent
 * that needs no account, then the one that is free to run locally, and only then
 * the metered ones. A metered agent is never selected silently, but it is
 * selectable, because sometimes that is exactly what is wanted.
 */
const preferOrder = ['opencode', 'gemini', 'kilocode', 'goose', 'claude']

/**
 * bestHarness picks the default agent.
 *
 * "Best" is defined as: installed first, because an agent that is not on the
 * path fails at spawn time; then cheapest; then the declared preference order.
 * The previous version hard-coded 'opencode', which meant the picker looked
 * broken on any machine without it.
 */
function bestHarness(hs: Harness[]): string {
  const rank = (h: Harness): number => {
    const i = preferOrder.indexOf(h.id)
    const preference = i === -1 ? preferOrder.length : i
    const cost = h.cost === 'free' ? 0 : h.cost === 'free-tier' ? 1 : 2
    return (h.installed ? 0 : 100) + cost * 10 + preference
  }
  const sorted = [...hs].sort((a, b) => rank(a) - rank(b))
  return sorted.length > 0 ? sorted[0].id : ''
}

type Pane = 'preview' | 'logs'

/**
 * The app is one stage with two views, not three columns.
 *
 * The earlier layout put the agent rail, the activity feed and the artifacts in
 * three fixed columns. It was defensible on a wide monitor and wrong everywhere
 * else: the centre column measured 0px at 700px wide, and even at 1440px the
 * artifacts were squeezed into 420px while the feed next to them had a screen's
 * worth of unused width. Worse, it split one job across the screen - asking for
 * something, watching it happen, and seeing the result - so the eye had to jump
 * between columns to follow a single run.
 *
 * One stage with a Preview/Logs toggle keeps the whole surface for whichever of
 * those two things matters right now, and the conversation stays in one fixed
 * place at the bottom where every assistant puts it.
 */
export default function App() {
  const [health, setHealth] = createSignal<Health | null>(null)
  const [harnesses, setHarnesses] = createSignal<Harness[]>([])
  const [selected, setSelected] = createSignal('')
  const [autoPicked, setAutoPicked] = createSignal(true)
  const [model, setModel] = createSignal('')
  const [autoApprove, setAutoApprove] = createSignal(false)
  const [prompt, setPrompt] = createSignal('')
  const [events, setEvents] = createSignal<RunEvent[]>([])
  const [running, setRunning] = createSignal(false)
  const [runId, setRunId] = createSignal<string | null>(null)
  const [fatal, setFatal] = createSignal('')
  const [narration, setNarration] = createSignal('')
  const [narrated, setNarrated] = createSignal<'local' | 'model' | ''>('')
  const [lastMessage, setLastMessage] = createSignal('')
  const [files, setFiles] = createSignal(0)
  const [pane, setPane] = createSignal<Pane>('preview')
  const [settingsOpen, setSettingsOpen] = createSignal(false)
  // Live runs on the server, regardless of which tab started them. Polled, not
  // derived from this tab's `running`: a run started elsewhere, or before a
  // reload, has to be visible or the UI lies about what the machine is doing.
  const [activeRuns, setActiveRuns] = createSignal<ActiveRun[]>([])
  // The per-run wall-clock ceiling, in seconds. Ten minutes is the default: a
  // page built from a fetched source finishes well inside it, and the run that
  // previously died did so at twenty minutes with nothing to show for it.
  const [timeLimitSec, setTimeLimitSec] = createSignal(600)

  let abort: AbortController | null = null
  let narrateTimer: number | undefined
  // Transcripts already loaded this session, so switching back to a run is
  // instant and does not re-fetch what the server will return unchanged.
  const history = new Map<string, RunEvent[]>()

  const view = createMemo(() => {
    const v = build(events())
    // The file count lives in the artifacts panel's listing, not in the event
    // stream: nothing in the stream describes a file that was written.
    return { items: v.items, summary: { ...v.summary, files: files() } }
  })
  const summary = createMemo(() => view().summary)
  const sentence = createMemo(() => narration() || localSentence(summary()))

  const selectedHarness = createMemo(() => harnesses().find((h) => h.id === selected()))
  // A run needs both a confinement boundary and an agent that exists. Saying so
  // here is what lets the chat bar disable its own button instead of letting a
  // run start and fail a second later.
  const ready = createMemo(() => Boolean(health()?.sandboxdOk && selectedHarness()?.installed))

  createEffect(() => {
    void (async () => {
      try {
        setHealth(await fetchHealth())
      } catch (e) {
        setFatal(`control plane unreachable: ${(e as Error).message}`)
      }
      try {
        const hs = await fetchHarnesses()
        setHarnesses(hs)
        const pick = bestHarness(hs)
        setSelected(pick)
        setAutoPicked(true)
        if (!hs.some((h) => h.installed)) {
          setFatal('no coding agent is installed on this machine')
        }
      } catch (e) {
        setFatal(`loading agents: ${(e as Error).message}`)
      }
    })()
  })

  // Poll what is actually running. Three seconds is frequent enough to notice a
  // run appear and cheap enough not to matter: the endpoint lists a map, not
  // the filesystem.
  createEffect(() => {
    let stop = false
    const tick = async () => {
      try {
        const { active } = await fetchActive()
        if (!stop) setActiveRuns(active)
      } catch {
        // A failed poll is not worth a banner; the next one usually succeeds and
        // the health check already reports an unreachable control plane.
      }
    }
    void tick()
    const timer = window.setInterval(() => void tick(), 3000)
    onCleanup(() => {
      stop = true
      clearInterval(timer)
    })
  })

  // Replay a selected run's recorded transcript.
  //
  // Runs are chosen from a dropdown that lists what is on disk, so without this
  // the Logs view would be blank for every run except the one this tab happened
  // to witness - which made "previous runs" a file browser with no history. The
  // effect is skipped while a run is live: there the stream is the source of
  // truth, and fetching mid-run would race the events still arriving.
  createEffect(() => {
    const id = runId()
    if (!id || running()) return
    const cached = history.get(id)
    if (cached) {
      setEvents(cached)
      return
    }
    void fetchRunEvents(id)
      .then((evs) => {
        history.set(id, evs)
        if (runId() === id && !running()) setEvents(evs)
      })
      .catch(() => undefined)
  })

  // The status line: the browser's own sentence immediately, then a better one
  // from the model a moment later. Updating locally first means the line is
  // never blank or stale, whether or not a key is configured.
  createEffect(() => {
    const s = summary()
    const local = localSentence(s)
    setNarration(local)
    setNarrated('local')
    if (!running()) return
    if (narrateTimer !== undefined) clearTimeout(narrateTimer)
    narrateTimer = window.setTimeout(() => {
      void (async () => {
        const n = await narrate({
          phase: s.phase,
          harness: selectedHarness()?.name,
          step: s.step,
          tools: s.tools,
          files: files(),
          elapsedSec: s.elapsedSec,
          lastMessage: lastMessage(),
          error: s.error,
        })
        if (n && n.narration) {
          setNarration(n.narration)
          setNarrated(n.model ? 'model' : 'local')
        }
      })()
    }, 700)
  })

  onCleanup(() => {
    abort?.abort()
    if (narrateTimer !== undefined) clearTimeout(narrateTimer)
  })

  function pick(id: string) {
    setSelected(id)
    setAutoPicked(false)
  }

  async function run() {
    if (running()) return
    if (!selected() || !prompt().trim()) return
    setFatal('')
    setEvents([])
    setRunId(null)
    setFiles(0)
    setLastMessage('')
    setRunning(true)
    setNarration('Starting…')
    abort = new AbortController()

    const result = await streamRun(
      {
        harnessId: selected(),
        prompt: prompt(),
        models: model().trim() ? [model().trim()] : undefined,
        autoApprove: autoApprove(),
        timeoutSec: timeLimitSec(),
      },
      (ev) => {
        setEvents((prev) => [...prev, ev])
        // run.assigned carries the workspace at the top level of the payload,
        // not under `data` - it is the one event the server emits itself.
        if (ev.kind === 'run.assigned' && typeof ev.runId === 'string') setRunId(ev.runId)
        if (ev.kind === 'message' && ev.text) setLastMessage(ev.text)
      },
      abort.signal,
    )
    setRunning(false)
    // Record what this tab witnessed so a later reselect is instant and a
    // reload of this run does not depend on a fetch racing the stream's end.
    const id = runId()
    if (id && result.ok) history.set(id, events())
    if (!result.ok && result.error) {
      setFatal(result.error)
      setNarration(result.error)
    }
  }

  function stop() {
    abort?.abort()
  }

  return (
    <div class="flex h-screen flex-col bg-surface-0 text-zinc-200">
      <header class="flex shrink-0 items-center gap-3 border-b border-hairline px-4 py-2.5">
        <span class="grid size-6 shrink-0 place-items-center rounded bg-emerald-500/20 font-mono text-[13px] text-emerald-300">
          σ
        </span>
        <h1 class="shrink-0 text-[13px] font-semibold tracking-tight">ai-os</h1>
        <span class="flex-1" />
        <Show
          when={health()}
          fallback={<span class="flex items-center gap-1.5 text-[11px] text-zinc-500">connecting…</span>}
        >
          {/* Below sm the labels go and the dots stay: three labelled badges
              measured 332px together and pushed the header past a 390px window.
              Each dot keeps its wording in a title, so the state is still
              available to a mouse, a screen reader, and a hover. */}
          <div class="flex shrink-0 items-center gap-2.5 text-[11px]">
            <span
              class="flex items-center gap-1.5"
              title={`Landlock ${health()!.sandboxdOk ? 'armed' : 'unavailable'}`}
            >
              <span class={`size-1.5 rounded-full ${health()!.sandboxdOk ? 'bg-emerald-400' : 'bg-amber-400'}`} />
              <span class={`hidden sm:inline ${health()!.sandboxdOk ? 'text-emerald-300' : 'text-amber-300'}`}>
                Landlock
              </span>
            </span>
            <span
              class={`flex items-center gap-1.5 ${health()?.plannerOk ? 'text-emerald-300' : 'text-zinc-600'}`}
              title={`narrator ${health()?.plannerOk ? 'model' : 'local'}`}
            >
              <span class={`size-1.5 rounded-full ${health()?.plannerOk ? 'bg-emerald-400' : 'bg-zinc-700'}`} />
              <span class="hidden sm:inline">narrator</span>
            </span>
            <span
              class={`flex items-center gap-1.5 ${health()?.artifactsOk ? 'text-emerald-300' : 'text-amber-300'}`}
              title={`output ${health()?.artifactsOk ? 'kept' : 'unavailable'}`}
            >
              <span class={`size-1.5 rounded-full ${health()?.artifactsOk ? 'bg-emerald-400' : 'bg-amber-400'}`} />
              <span class="hidden sm:inline">output</span>
            </span>
          </div>
        </Show>

        {/* Live-run badge. This is the answer to "is something running right
            now", independent of this tab's own state: a run started in another
            tab, or one whose stream this tab lost, is still shown here. */}
        <Show when={activeRuns().length > 0}>
          <span
            class="flex shrink-0 items-center gap-1.5 rounded-full border border-cyan-500/30 bg-cyan-500/10 px-2 py-0.5 text-[10px] font-medium text-cyan-200"
            title={activeRuns()
              .map((r) => `${r.harnessId}: ${r.prompt}`)
              .join('\n')}
          >
            <span class="size-1.5 animate-pulse rounded-full bg-cyan-400" />
            {activeRuns().length} running
          </span>
        </Show>
      </header>

      <main class="relative flex min-h-0 flex-1 flex-col pt-11">
        {/* The toggle floats over the stage instead of sitting in a header bar.
            It is the only global navigation in the product, so it is placed at
            the top centre where it reads as a mode switch for whatever is below
            it rather than as one more control in a row.

            The stage reserves this band with `pt-11` so the pill never lands on
            top of the preview toolbar underneath it. Measured at 360px, without
            the reservation it covered the run picker and the files button. */}
        <div class="pointer-events-none absolute inset-x-0 top-2 z-20 flex justify-center px-3">
          <div class="pointer-events-auto flex items-center gap-1 rounded-full border border-hairline bg-surface-1/85 p-0.5 shadow-lg backdrop-blur-xl">
            <For each={['preview', 'logs'] as Pane[]}>
              {(p) => (
                <button
                  type="button"
                  onClick={() => setPane(p)}
                  aria-pressed={pane() === p}
                  class={`rounded-full px-3 py-1 text-[11px] capitalize transition ${
                    pane() === p
                      ? 'bg-zinc-700/70 text-white shadow-sm'
                      : 'text-zinc-500 hover:text-zinc-300'
                  }`}
                >
                  <span class="flex items-center gap-1.5">
                    {p}
                    <Show when={p === 'logs' && summary().tools > 0}>
                      <span class="mono text-[10px] text-zinc-500">{summary().tools}</span>
                    </Show>
                  </span>
                </button>
              )}
            </For>
          </div>
        </div>

        {/* Fatal is a flow element, not an overlay. Under the floating toggle an
            absolute banner would cover the first row of whichever pane is
            showing, which is exactly the content you need next to an error. */}
        <Show when={fatal()}>
          <div class="mono mx-3 mb-2 rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-[11px] leading-relaxed text-red-300">
            {fatal()}
          </div>
        </Show>

        {/* Both views stay mounted. The artifacts panel holds a live iframe and a
            polled file listing, and tearing them down on every toggle would blank
            the preview and restart its requests - so the view is hidden with
            `hidden` rather than swapped with a conditional.

            Each wrapper is itself a flex column. Without that the panel's
            `flex-1` has no parent height to divide, and the preview iframe
            collapses to its default 150px in a full-height window. */}
        <div class={`flex min-h-0 flex-1 flex-col ${pane() === 'preview' ? '' : 'hidden'}`}>
          <Artifacts
            runId={runId()}
            running={running()}
            active={activeRuns()}
            onFiles={setFiles}
            onSelectRun={(id) => {
              setRunId(id)
              setEvents([])
              setFatal('')
            }}
          />
        </div>
        <div class={`flex min-h-0 flex-1 flex-col ${pane() === 'logs' ? '' : 'hidden'}`}>
          <Activity items={view().items} summary={summary()} live={running()} running={running()} />
        </div>
      </main>

      {/* The narration line sits above the chat bar, where a status belongs:
          close to the input that started the run, and out of the stage. The
          sentence is the only text here while idle, so it cannot compete with
          the artifact the user is looking at. */}
      <Show when={running()}>
        <div class="pointer-events-none shrink-0 px-4 pb-2">
          <div class="mx-auto flex max-w-[860px] items-center justify-center gap-2 text-[11px]">
            <span class="size-1.5 animate-pulse rounded-full bg-cyan-400" />
            <span class="min-w-0 truncate text-zinc-500">{sentence()}</span>
            <Show when={narrated() === 'model'}>
              <span class="rounded bg-violet-500/15 px-1.5 py-0.5 text-[10px] uppercase text-violet-300">
                model
              </span>
            </Show>
          </div>
        </div>
      </Show>

      <ChatBar
        prompt={prompt()}
        onPrompt={setPrompt}
        onRun={() => void run()}
        onStop={stop}
        onSettings={() => setSettingsOpen(true)}
        running={running()}
        ready={ready()}
        harness={selectedHarness()}
        autoApprove={autoApprove()}
        elapsedSec={summary().elapsedSec}
      />

      <SettingsDrawer
        open={settingsOpen()}
        onClose={() => setSettingsOpen(false)}
        harnesses={harnesses()}
        selected={selected()}
        onSelect={pick}
        autoPicked={autoPicked()}
        model={model()}
        onModel={setModel}
        autoApprove={autoApprove()}
        onAutoApprove={setAutoApprove}
        timeLimitSec={timeLimitSec()}
        onTimeLimit={setTimeLimitSec}
        onDemoPrompt={() => {
          setPrompt(PORTFOLIO_PROMPT)
          setSettingsOpen(false)
          setPane('logs')
        }}
        fatal={fatal()}
      />

    </div>
  )
}