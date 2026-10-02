import { createEffect, createSignal, Show } from 'solid-js'
import type { Harness } from './types'

/**
 * The chat bar: one input for the whole product.
 *
 * It floats at the foot of the stage rather than sitting in a sidebar, for the
 * same reason every assistant does it there: the conversation is the primary
 * object, and a box in the corner of the screen makes it look like one field
 * among several. Floating it also keeps it in the same place whether the stage
 * is showing a preview, a file list or a long run log.
 *
 * Configuration is attached rather than separated. The agent chip and the gear
 * are part of the same bar, so "who am I talking to, and with what" is read in
 * one glance instead of across the screen - and a run started with the wrong
 * model is the most expensive mistake this interface can allow.
 */

export default function ChatBar(props: {
  prompt: string
  onPrompt: (v: string) => void
  onRun: () => void
  onStop: () => void
  onSettings: () => void
  running: boolean
  ready: boolean
  harness: Harness | undefined
  autoApprove: boolean
  elapsedSec: number
}) {
  const [focused, setFocused] = createSignal(false)
  let area: HTMLTextAreaElement | undefined

  const canSend = () => props.ready && !props.running && props.prompt.trim().length > 0

  // The box grows with what is typed, up to a point. A textarea that never grows
  // hides a long prompt behind a scrollbar; one that grows forever pushes the
  // preview off screen.
  const autoGrow = () => {
    if (!area) return
    area.style.height = 'auto'
    area.style.height = `${Math.min(area.scrollHeight, 168)}px`
  }

  createEffect(() => {
    props.prompt
    autoGrow()
  })

  const onKeyDown = (e: KeyboardEvent) => {
    // Enter sends, Shift+Enter breaks the line. Cmd/Ctrl+Enter also sends,
    // because that is what muscle memory from a code editor expects.
    if (e.key !== 'Enter') return
    if (e.shiftKey) return
    e.preventDefault()
    if (canSend()) props.onRun()
  }

  const elapsed = () => {
    const s = props.elapsedSec
    if (s < 60) return `${s}s`
    const m = Math.floor(s / 60)
    return s % 60 === 0 ? `${m}m` : `${m}m ${s % 60}s`
  }

  return (
    <div class="pointer-events-none shrink-0 px-3 pb-4 sm:px-6 sm:pb-6">
      <div class="mx-auto w-full max-w-[860px]">
        <div
          class={`pointer-events-auto flex items-end gap-1.5 rounded-2xl border bg-surface-2/80 p-1.5 shadow-[0_18px_50px_-20px_rgba(0,0,0,0.9)] backdrop-blur-xl transition-colors ${
            focused() ? 'border-zinc-700' : 'border-hairline'
          }`}
        >
          {/* Who you are talking to. Tapping it opens the same settings as the
              gear, because "switch agent" is the most common reason to go
              there and burying it in a drawer would be silly. */}
          <button
            type="button"
            onClick={props.onSettings}
            title="Agent settings"
            class={`flex h-9 shrink-0 items-center gap-1.5 rounded-xl px-2 text-[11px] transition ${
              focused() ? 'text-zinc-300' : 'text-zinc-500 hover:text-zinc-200'
            }`}
          >
            <span
              class={`size-1.5 rounded-full ${props.harness?.installed ? 'bg-emerald-400' : 'bg-red-400'}`}
            />
            <span class="max-w-[120px] truncate">{props.harness?.name ?? 'agent'}</span>
            <Show when={props.autoApprove}>
              <span
                class="rounded bg-amber-500/15 px-1 text-[9px] uppercase tracking-wider text-amber-300"
                title="Every tool call is approved automatically"
              >
                auto
              </span>
            </Show>
          </button>

          <textarea
            ref={area}
            value={props.prompt}
            rows={1}
            onInput={(e) => props.onPrompt(e.currentTarget.value)}
            onKeyDown={onKeyDown}
            onFocus={() => setFocused(true)}
            onBlur={() => setFocused(false)}
            placeholder={props.running ? 'Working…' : 'Ask ai-os to build something'}
            aria-label="Prompt"
            class="max-h-[168px] min-h-9 flex-1 resize-none bg-transparent py-2 text-[13px] leading-relaxed text-zinc-100 outline-none placeholder:text-zinc-600"
          />

          <Show
            when={!props.running}
            fallback={
              <button
                type="button"
                onClick={props.onStop}
                aria-label="Stop the run"
                title="Stop the run"
                class="grid size-9 shrink-0 place-items-center rounded-xl border border-red-500/40 bg-red-500/10 text-red-300 transition hover:bg-red-500/20"
              >
                <span class="size-2.5 rounded-[1px] bg-current" />
              </button>
            }
          >
            <button
              type="button"
              onClick={() => props.onRun()}
              disabled={!canSend()}
              aria-label="Run"
              title="Run (Enter)"
              class={`grid size-9 shrink-0 place-items-center rounded-xl transition ${
                canSend()
                  ? 'bg-emerald-500 text-emerald-950 hover:bg-emerald-400'
                  : 'cursor-not-allowed bg-zinc-800 text-zinc-600'
              }`}
            >
              <span class="text-[15px] leading-none">↑</span>
            </button>
          </Show>
        </div>

        <div class="pointer-events-auto mt-1.5 flex items-center justify-center gap-2 px-1 text-[10px] text-zinc-700">
          <Show when={props.running}>
            <span class="mono text-zinc-600">{elapsed()}</span>
            <span>·</span>
          </Show>
          <span>Enter to run · shift+enter for a new line</span>
          <Show when={!props.ready}>
            <span>·</span>
            <span class="text-amber-400/80">sandbox not ready</span>
          </Show>
        </div>
      </div>
    </div>
  )
}