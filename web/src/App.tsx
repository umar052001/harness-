import { batch, createEffect, createMemo, createSignal, For, onCleanup, onMount, Show } from 'solid-js'
import Activity from './Activity'
import Artifacts from './Artifacts'
import ChatBar from './ChatBar'
import SettingsDrawer from './Settings'
import Usage from './Usage'
import Database from './Database'
import { latestUsage } from './usage'
import { build, localSentence } from './events'
import { PORTFOLIO_PROMPT } from './demo'
import { fetchActive, fetchHarnesses, fetchHealth, fetchRunEvents, fetchRuns, stopRun, streamRun } from './api'
import type { ActiveRun, Harness, Health, RunEvent, RunSummary } from './types'

type Pane = 'preview' | 'logs'

export default function App() {
  const [health, setHealth] = createSignal<Health | null>(null)
  const [harnesses, setHarnesses] = createSignal<Harness[]>([])
  const [selected, setSelected] = createSignal('opencode')
  const [model, setModel] = createSignal('')
  const [autoApprove, setAutoApprove] = createSignal(false)
  const [prompt, setPrompt] = createSignal('')
  const [events, setEvents] = createSignal<RunEvent[]>([])
  const [running, setRunning] = createSignal(false)
  const [stopping, setStopping] = createSignal(false)
  const [runId, setRunId] = createSignal<string | null>(null)
  const [parentRunId, setParentRunId] = createSignal<string | undefined>()
  const [runs, setRuns] = createSignal<RunSummary[]>([])
  const [active, setActive] = createSignal<ActiveRun[]>([])
  const [fatal, setFatal] = createSignal('')
  const [files, setFiles] = createSignal(0)
  const [pane, setPane] = createSignal<Pane>('preview')
  const [settingsOpen, setSettingsOpen] = createSignal(false)
  const [timeLimit, setTimeLimit] = createSignal(600)
  const [now, setNow] = createSignal(Date.now())
  const [started, setStarted] = createSignal(0)
  const [loading, setLoading] = createSignal(true)
  let controller: AbortController | undefined
  let disposed = false
  let polling = false
  let pending: RunEvent[] = []
  let flushTimer: number | undefined

  const selectedHarness = createMemo(() => harnesses().find(h => h.id === selected()))
  const current = createMemo(() => runs().find(r => r.id === runId()))
  const usage = createMemo(() => latestUsage(events(), current()?.usage))
  const isLive = createMemo(() => running() || active().some(r => r.runId === runId()))
  const view = createMemo(() => build(events()))
  const elapsed = createMemo(() => {
    if (running() && started()) return Math.floor((now() - started()) / 1000)
    const live = active().find(r => r.runId === runId())
    if (live) return Math.max(0, Math.floor((now() - Date.parse(live.startedAt)) / 1000))
    return view().summary.elapsedSec
  })
  const summary = createMemo(() => ({ ...view().summary, files: files(), elapsedSec: elapsed() }))
  const ready = createMemo(() => !!(health()?.sandboxdOk && health()?.artifactsOk && selectedHarness()?.installed))
  const title = createMemo(() => current()?.prompt?.split('\n')[0] || (running() ? prompt().split('\n')[0] : 'Your workspace'))
  const status = createMemo(() => stopping() ? 'Stopping safely' : isLive() ? 'Building' : current()?.status || (view().summary.phase === 'done' ? 'completed' : 'Saved workspace'))

  const refreshRuns = async () => {
    const next = await fetchRuns()
    if (!disposed) setRuns(next)
  }

  async function connect() {
    setLoading(true)
    try {
      const [h, agents] = await Promise.all([fetchHealth(), fetchHarnesses()])
      if (disposed) return
      batch(() => {
        setHealth(h)
        setHarnesses(agents)
        if (!agents.some(a => a.id === selected() && a.installed)) {
          setSelected(agents.find(a => a.id === 'opencode' && a.installed)?.id || agents.find(a => a.installed)?.id || '')
        }
        setFatal('')
      })
      await refreshRuns()
    } catch (e) {
      if (!disposed) { setHealth(null); setFatal(`Cannot reach harness++. ${(e as Error).message}`) }
    } finally { if (!disposed) setLoading(false) }
  }

  async function poll() {
    if (polling || disposed) return
    polling = true
    try {
      const next = await fetchActive()
      if (disposed) return
      const wasLive = active().some(r => r.runId === runId())
      setActive(next.active)
      const id = runId()
      if (!running() && id && (wasLive || next.active.some(r => r.runId === id))) {
        const evs = await fetchRunEvents(id)
        if (!disposed && runId() === id && !running()) setEvents(evs)
      }
      if (wasLive && !next.active.some(r => r.runId === runId())) await refreshRuns()
    } catch { /* Connection errors are surfaced by connect and streamRun. */ }
    finally { polling = false }
  }

  onMount(() => {
    try {
      setPrompt(sessionStorage.getItem('harness-draft') || '')
    } catch { /* Private browsing may disable storage. */ }
    void connect()
    void poll()
    const timer = window.setInterval(() => { setNow(Date.now()); void poll() }, 3000)
    const clock = window.setInterval(() => setNow(Date.now()), 1000)
    onCleanup(() => { clearInterval(timer); clearInterval(clock) })
  })
  createEffect(() => {
    try { sessionStorage.setItem('harness-draft', prompt()) } catch { /* Drafts are best effort. */ }
  })
  onCleanup(() => { disposed = true; controller?.abort(); clearTimeout(flushTimer) })

  async function selectRun(id: string) {
    if (running()) return
    batch(() => { setRunId(id || null); setParentRunId(undefined); setEvents([]); setFatal(''); setFiles(0) })
    if (!id) return
    try {
      const evs = await fetchRunEvents(id)
      if (runId() === id && !running()) setEvents(evs)
    } catch (e) { if (runId() === id) setFatal(`Could not load activity. ${(e as Error).message}`) }
  }

  function flush() {
    clearTimeout(flushTimer)
    flushTimer = undefined
    if (!pending.length || disposed) return
    const chunk = pending
    pending = []
    setEvents(previous => [...previous, ...chunk])
  }

  async function run() {
    if (running() || active().length || !ready() || !prompt().trim()) return
    batch(() => {
      setFatal(''); setEvents([]); setRunId(null); setFiles(0)
      setRunning(true); setStopping(false); setStarted(Date.now()); setPane('logs')
    })
    controller = new AbortController()
    pending = []
    try {
      const result = await streamRun({ harnessId: selected(), prompt: prompt().trim(),
        parentRunId: parentRunId(),
        models: model().trim() ? [model().trim()] : undefined,
        autoApprove: autoApprove(), timeoutSec: timeLimit() }, ev => {
        if (disposed) return
        if (ev.kind === 'run.assigned' && ev.runId) setRunId(ev.runId)
        pending.push(ev)
        if (flushTimer === undefined) flushTimer = window.setTimeout(flush, 80)
      }, controller.signal)
      flush()
      setParentRunId(undefined)
      if (!result.ok) setFatal(result.error || 'Run could not finish. Your files are still available.')
      else setPane('preview')
    } finally {
      flush()
      setRunning(false); setStopping(false)
      await Promise.allSettled([refreshRuns(), poll()])
    }
  }

  async function stop() {
    if (stopping()) return
    setStopping(true)
    try {
      if (!runId() && !active().length) await poll()
      const id = runId() || active()[0]?.runId
      if (id) await stopRun(id)
      else setFatal('The run has not been assigned yet. Try Stop run again in a moment.')
      await poll()
    } catch (e) { setFatal((e as Error).message) }
    finally { setStopping(false) }
  }

  return (
    <div class="app-shell">
      <header class="app-header">
        <a href="#" class="brand" onClick={e => { e.preventDefault(); if (!running()) void selectRun('') }} aria-label="harness++ home">
          <span class="brand-mark" aria-hidden="true">h<span>+</span></span><strong>harness++</strong><span class="brand-caption">BUILD WITH INTENT</span>
        </a>
        <div class="header-actions">
          <span class={`connection ${health() ? 'online' : ''}`}><i />{health() ? 'Local workspace' : 'Offline'}</span>
          <button class="quiet-button" onClick={() => setSettingsOpen(true)}>Settings <span aria-hidden="true">↗</span></button>
        </div>
      </header>

      <main class="app-main">
        <div class="workspace-heading">
          <div class="workspace-title"><span class="eyebrow">WORKSPACE / {runId() ? 'PROJECT' : 'OVERVIEW'}</span><h1 title={title()}>{title()}</h1></div>
          <div class="workspace-actions">
            <Show when={runs().length > 0}>
              <select aria-label="Run history" value={runId() || ''} disabled={running()} onChange={e => void selectRun(e.currentTarget.value)}>
                <option value="">Recent projects</option>
                <For each={runs()}>{r => <option value={r.id}>{(r.prompt?.split('\n')[0] || r.id).slice(0, 50)} · {r.status || `${r.files} files`}</option>}</For>
              </select>
            </Show>
            <Show when={runId()}><button class="quiet-button" disabled={isLive()} onClick={() => { setParentRunId(runId()!); setPrompt(''); document.querySelector<HTMLTextAreaElement>('textarea[aria-label="Prompt"]')?.focus() }}>Continue project</button><button class="quiet-button" disabled={running()} onClick={() => void selectRun('')}>＋ New project</button></Show>
          </div>
        </div>

        <Show when={fatal()}><div class="notice" role="alert"><span>{fatal()}</span><button onClick={() => void connect()}>Reconnect</button><button aria-label="Dismiss error" onClick={() => setFatal('')}>×</button></div></Show>
        <Show when={parentRunId() && !running()}><div class="active-notice"><span>Describe the next change. Saved project files will be copied into a new attempt; the previous version stays intact.</span><button onClick={() => setParentRunId(undefined)}>Cancel continuation</button></div></Show>
        <Show when={!loading() && !ready()}><div class="notice">{!health() ? 'Start the control plane, then reconnect.' : !health()?.artifactsOk ? 'Workspace storage is unavailable.' : !health()?.sandboxdOk ? 'The sandbox executor is unavailable.' : 'Install a supported agent to begin.'}<button onClick={() => setSettingsOpen(true)}>Open settings</button></div></Show>
        <Show when={active().length > 0 && !running()}><div class="active-notice"><span><i class="live-dot" /> A run is active on this machine. One run at a time.</span><button onClick={() => void selectRun(active()[0].runId)}>View run</button><button disabled={stopping()} onClick={() => void stop()}>Stop run</button></div></Show>

        <section class="workspace-stage">
          <Show when={runId() || running()} fallback={
            <div class="welcome">
              <div class="welcome-grid" aria-hidden="true" />
              <div class="welcome-content">
                <span class="welcome-label"><i /> YOUR IDEAS. YOUR MACHINE.</span>
                <h2>From an idea.<br /><span>To something real.</span></h2>
                <p>Describe what you want to build. An agent works in its own sandbox.<br class="desktop-break" /> Watch it take shape, then preview and keep every file.</p>
                <div class="workflow" aria-label="How harness++ works"><span><b>01</b> Describe</span><i>→</i><span><b>02</b> Build safely</span><i>→</i><span><b>03</b> Make it yours</span></div>
              </div>
              <div class="welcome-footer"><span>OpenCode / Big Pickle</span><span>Saved locally · Export anytime</span></div>
            </div>
          }>
            <div class="stage-tabs" role="group" aria-label="Workspace view">
              <For each={['preview', 'logs'] as Pane[]}>{p => <button aria-pressed={pane() === p} onClick={() => setPane(p)}>{p === 'preview' ? 'Preview' : 'Activity'}<Show when={p === 'logs' && isLive()}><i class="live-dot" /></Show></button>}</For>
            </div>
            <div class="run-status" aria-live="polite"><span class={`status-pill ${isLive() ? 'building' : ''}`}>{status() === 'completed' ? 'Code generated' : status().replace('_', ' ')}</span><span class="run-sentence">{isLive() ? localSentence(summary()) : current()?.error || `${files()} files saved in your workspace`}</span><span class="run-time">{Math.floor(elapsed() / 60)}:{String(elapsed() % 60).padStart(2, '0')}</span></div>
            <Usage report={usage()} model={current()?.model || (running() ? model() || 'opencode/big-pickle' : 'Model not recorded')} />
            <Show when={runId()}><Database runId={runId()} running={isLive()} /></Show>
            <div class={`stage-pane ${pane() === 'preview' ? '' : 'hidden'}`}>
              <Artifacts runId={runId()} running={isLive()} active={active()} onFiles={setFiles} />
            </div>
            <div class={`stage-pane activity-pane ${pane() === 'logs' ? '' : 'hidden'}`}><Activity items={view().items} summary={summary()} live={isLive()} running={isLive()} /></div>
          </Show>
        </section>
      </main>

      <ChatBar prompt={prompt()} onPrompt={setPrompt} onRun={() => void run()} onStop={() => void stop()}
        onSettings={() => setSettingsOpen(true)} running={running()} ready={ready() && active().length === 0}
        harness={selectedHarness()} autoApprove={autoApprove()} onAutoApprove={setAutoApprove}
        model={model() || (selected() === 'opencode' ? 'Big Pickle' : 'Agent default')}
        elapsedSec={elapsed()} />
      <SettingsDrawer open={settingsOpen()} onClose={() => setSettingsOpen(false)} harnesses={harnesses()}
        selected={selected()} onSelect={id => { setSelected(id); setModel('') }} autoPicked={false}
        model={model()} onModel={setModel} autoApprove={autoApprove()} onAutoApprove={setAutoApprove}
        timeLimitSec={timeLimit()} onTimeLimit={setTimeLimit} fatal={fatal()}
        onDemoPrompt={() => { setPrompt(PORTFOLIO_PROMPT); setSettingsOpen(false) }} />
    </div>
  )
}
