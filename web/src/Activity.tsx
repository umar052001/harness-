import { createEffect, createMemo, createSignal, For, onMount, Show } from 'solid-js'
import type { Item, Summary } from './events'

/**
 * The activity feed: named steps instead of a raw event dump.
 *
 * Three details carry most of the weight. Rows for a tool call are correlated
 * by id, so a call appears once and fills in as its frames arrive rather than
 * repeating. Runs of the same action are folded into one row, because thirty
 * "Writing" lines is work to read and no new information. And the feed follows
 * the newest row only while the reader is already at the bottom, so scrolling
 * back to read something is not fought by the stream pushing new rows in.
 */
export default function Activity(props: {
  items: Item[]
  summary: Summary
  live: boolean
  running: boolean
}) {
  let scroller: HTMLDivElement | undefined
  // A signal, not a plain variable: the "jump to latest" button reads this, and
  // a plain variable meant the button appeared only when the next event
  // happened to arrive and never disappeared when the reader scrolled back.
  const [pinned, setPinned] = createSignal(true)

  // Measured in the handler rather than tracked in a signal on every frame: a
  // scroll handler that writes state continuously causes an avalanche of
  // re-renders under a streaming feed. Writing only when the answer changes
  // keeps the common case free.
  const onScroll = () => {
    if (!scroller) return
    const gap = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight
    const atBottom = gap < 80
    if (atBottom !== pinned()) setPinned(atBottom)
  }

  const jumpToLatest = () => {
    if (!scroller) return
    scroller.scrollTop = scroller.scrollHeight
    setPinned(true)
  }

  // The dependency that was missing before: the effect has to read the thing
  // that changes when a row arrives. It used to read only `running`, so it ran
  // once and the feed never scrolled again for the whole run.
  createEffect(() => {
    const count = props.items.length
    if (count === 0) return
    if (pinned() || props.live === false) jumpToLatest()
  })

  onMount(() => jumpToLatest())

  const phases = createMemo(() => props.items.filter((i) => i.kind === 'phase').length)
  const actions = createMemo(() =>
    props.items.reduce((n, i) => n + (i.kind === 'step' ? i.repeats : 0), 0),
  )
  const failures = createMemo(() => props.items.filter((i) => i.kind === 'step' && i.status === 'failed').length)

  // Elapsed time in the one format a person reads at a glance: 4s, 1m 12s.
  const elapsed = createMemo(() => {
    const s = props.summary.elapsedSec
    if (s <= 0) return ''
    if (s < 60) return `${s}s`
    const m = Math.floor(s / 60)
    return s % 60 === 0 ? `${m}m` : `${m}m ${s % 60}s`
  })

  return (
    <div class="flex min-h-0 flex-1 flex-col">
      <div class="flex items-center gap-3 border-b border-hairline px-4 py-1.5 text-[11px] text-zinc-600">
        <Show
          when={props.items.length > 0}
          fallback={<span class="text-zinc-700">no activity yet</span>}
        >
          <span class="mono">
            {elapsed() || `${actions()} action${actions() === 1 ? '' : 's'}`}
          </span>
          <span>
            {actions()} action{actions() === 1 ? '' : 's'}
            <Show when={phases() > 0}> · {phases()} milestone{phases() === 1 ? '' : 's'}</Show>
            <Show when={failures() > 0}>
              <span class="text-red-400/80"> · {failures()} failed</span>
            </Show>
          </span>
        </Show>
        <span class="flex-1" />
        <Show when={!pinned() && props.items.length > 0}>
          <button
            type="button"
            onClick={jumpToLatest}
            class="animate-[fadeIn_.2s_ease-out] rounded border border-zinc-700 px-2 py-0.5 text-zinc-400 transition hover:border-cyan-600/70 hover:text-cyan-300"
          >
            jump to latest ↓
          </button>
        </Show>
      </div>

      <div ref={scroller} onScroll={onScroll} class="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        <Show
          when={props.items.length > 0}
          fallback={
            <div class="flex h-full flex-col items-center justify-center gap-2 text-zinc-700">
              <span class="mono text-lg">σ</span>
              <p class="text-xs">nothing is running yet</p>
              <p class="text-[11px] text-zinc-600">write a prompt and press run</p>
            </div>
          }
        >
          <ol class="space-y-1.5">
            <For each={props.items}>{(item) => <Row item={item} running={props.running} />}</For>
          </ol>
          <Show when={props.running}>
            {/* A live edge on the feed: the newest row is the run's current
                position, and this says so without animating anything that
                moves on its own. */}
            <div class="mt-2 flex items-center gap-2 text-[10px] uppercase tracking-wider text-zinc-700">
              <span class="size-1 animate-pulse rounded-full bg-cyan-400/70" />
              streaming
            </div>
          </Show>
        </Show>
      </div>
    </div>
  )
}

function Row(props: { item: Item; running: boolean }) {
  const at = () => {
    const d = new Date(props.item.at)
    if (Number.isNaN(d.getTime())) return ''
    return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}`
  }

  return (
    <li class="animate-[fadeIn_.25s_ease-out]">
      <SwitchRow item={props.item} at={at()} />
    </li>
  )
}

function SwitchRow(props: { item: Item; at: string }) {
  switch (props.item.kind) {
    case 'phase': {
      const p = props.item
      return (
        <div class="flex items-baseline gap-2.5 py-0.5">
          <Dot tone={p.tone} />
          <span class="text-[13px] font-medium text-zinc-100">{p.title}</span>
          <Show when={p.detail}>
            <span class="mono truncate text-[11px] text-zinc-500">{p.detail}</span>
          </Show>
          <span class="mono ml-auto shrink-0 text-[10px] text-zinc-700">{props.at}</span>
        </div>
      )
    }
    case 'step': {
      const s = props.item
      return (
        <div class="flex items-start gap-2.5 rounded px-1 py-0.5 transition-colors hover:bg-surface-2/60">
          <Dot tone={s.tone} spin={s.status === 'running'} />
          <div class="min-w-0 flex-1">
            <div class="flex items-baseline gap-2">
              <span class="text-[13px] text-zinc-200">{s.title}</span>
              <Show when={s.status === 'failed'}>
                <span class="text-[11px] text-red-400">failed</span>
              </Show>
              <span class="mono ml-auto shrink-0 text-[10px] text-zinc-700">{props.at}</span>
            </div>
            <Show when={s.detail}>
              <p class="mono truncate text-[11px] text-zinc-400" title={s.detail}>
                {s.detail}
              </p>
            </Show>
            <Show when={s.folded && s.folded.length > 0}>
              <Folded rows={s.folded!} />
            </Show>
          </div>
        </div>
      )
    }
    case 'say': {
      const s = props.item
      return (
        <div
          class={`rounded border-l-2 px-3 py-1.5 text-[13px] leading-snug ${
            s.final ? 'border-emerald-400 bg-emerald-500/5 text-emerald-100' : 'border-zinc-700 bg-surface-2/40 text-zinc-200'
          }`}
        >
          {s.text}
        </div>
      )
    }
    case 'log':
      return <Disclosure lines={props.item.lines} title={props.item.title} tone={props.item.tone} />
  }
}

/**
 * Folded lists what a grouped row covered.
 *
 * This is the reason grouping is safe: the row above says "Reading ×12" and
 * this says which twelve. Collapsed by default because the count is what a
 * reader wants, and the names are what a reviewer wants once.
 */
function Folded(props: { rows: { title: string; detail?: string; at: string }[] }) {
  return (
    <details class="mt-0.5">
      <summary class="cursor-pointer text-[10px] uppercase tracking-wider text-zinc-700 transition hover:text-zinc-400">
        what was folded
      </summary>
      <ul class="mono mt-1 space-y-0.5 border-l border-hairline pl-2 text-[10px] leading-relaxed text-zinc-600">
        <For each={props.rows}>
          {(r) => (
            <li class="flex gap-2">
              <span class="truncate" title={r.detail ?? r.title}>
                {r.title}
              </span>
              <span class="ml-auto shrink-0 text-zinc-700">{r.at}</span>
            </li>
          )}
        </For>
      </ul>
    </details>
  )
}

/**
 * Disclosure keeps noisy output available without putting it in the flow.
 *
 * The sandbox's own policy report and raw agent output are both evidence a
 * reviewer wants and clutter everyone else does not, so they collapse to one
 * line that says how much is inside.
 */
function Disclosure(props: { lines: string[]; title: string; tone: string }) {
  return (
    <details class="rounded border border-hairline bg-surface-2/40">
      <summary class="cursor-pointer px-2.5 py-1 text-[11px] text-zinc-500 hover:text-zinc-300">
        <span class="mono text-zinc-600">{props.tone === 'good' ? '✓ ' : ''}</span>
        {props.title}
      </summary>
      <pre class="mono max-h-64 overflow-auto border-t border-hairline px-2.5 py-1.5 text-[11px] leading-relaxed text-zinc-400">
        {props.lines.join('\n')}
      </pre>
    </details>
  )
}

function Dot(props: { tone: string; spin?: boolean }) {
  const color = () => {
    switch (props.tone) {
      case 'good':
        return 'bg-emerald-400'
      case 'run':
        return 'bg-cyan-400'
      case 'warn':
        return 'bg-amber-400'
      case 'bad':
        return 'bg-red-400'
      default:
        return 'bg-zinc-700'
    }
  }
  return <span class={`mt-1.5 size-1.5 shrink-0 rounded-full ${color()} ${props.spin ? 'animate-pulse' : ''}`} />
}
