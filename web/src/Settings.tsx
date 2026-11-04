import { For, onCleanup, onMount, Show } from 'solid-js'
import { Portal } from 'solid-js/web'
import type { Harness } from './types'

interface SettingsProps {
  workflowModels: { id: string; provider: string }[]
  allowGPT: boolean
  onAllowGPT: (value: boolean) => void
  open: boolean
  onClose: () => void
  harnesses: Harness[]
  selected: string
  onSelect: (id: string) => void
  autoPicked: boolean
  model: string
  onModel: (v: string) => void
  autoApprove: boolean
  onAutoApprove: (v: boolean) => void
  timeLimitSec: number
  onTimeLimit: (v: number) => void
  onDemoPrompt: () => void
  fatal: string
}

const costBadge: Record<string, string> = {
  free: 'bg-emerald-500/15 text-emerald-300',
  'free-tier': 'bg-sky-500/15 text-sky-300',
  metered: 'bg-amber-500/15 text-amber-300',
}

/** installed count, so the picker can say what is actually runnable. */
function installedCount(hs: Harness[]): number {
  return hs.filter((h) => h.installed).length
}

export default function SettingsDrawer(props: SettingsProps) {
  let dialog: HTMLDialogElement | undefined
  const close = () => { dialog?.close(); props.onClose() }
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close()
    }
    window.addEventListener('keydown', onKey)
    onCleanup(() => window.removeEventListener('keydown', onKey))
  })

  return (
    <Show when={props.open}>
      <Portal>
        <dialog ref={el => { dialog = el; queueMicrotask(() => el.isConnected && el.showModal()) }}
          aria-label="Settings"
          onCancel={e => { e.preventDefault(); close() }}
          class="fixed inset-0 z-50 m-0 h-dvh max-h-none w-screen max-w-none border-0 bg-transparent p-0 text-inherit">
          <button
            type="button"
            aria-label="Close settings"
            onClick={close}
            class="absolute inset-0 bg-black/60 backdrop-blur-[2px]"
          />
          <aside
            /* Full width on a phone, where a 360px panel would leave a sliver of
               the stage behind it looking like a mistake. */
            class="absolute inset-y-0 left-0 flex w-full animate-[slideIn_.18s_ease-out] flex-col overflow-y-auto border-r border-hairline bg-surface-1 sm:w-[360px]"
          >
            <div class="flex shrink-0 items-center gap-2 border-b border-hairline px-4 py-3">
              <h2 class="text-[11px] font-semibold uppercase tracking-wider text-zinc-400">
                settings
              </h2>
              <span class="flex-1" />
              <button
                type="button"
                onClick={close}
                aria-label="Close settings"
                class="rounded px-2 py-1 text-[11px] text-zinc-500 transition hover:bg-surface-2 hover:text-zinc-200"
              >
                ✕
              </button>
            </div>

            <div class="flex-1 space-y-5 p-4">
              <Show when={props.fatal}>
                <div class="mono rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-[11px] leading-relaxed text-red-300">
                  {props.fatal}
                </div>
              </Show>

              <section>
                <h3 class="mb-2 text-[11px] font-semibold uppercase tracking-wider text-zinc-500">
                  demo
                </h3>
                <button
                  type="button"
                  onClick={() => { dialog?.close(); props.onDemoPrompt() }}
                  class="w-full rounded-lg border border-hairline px-3 py-2 text-left text-[12px] text-zinc-300 transition hover:border-emerald-500/50 hover:bg-emerald-500/5 hover:text-emerald-200"
                >
                  Fill in the portfolio prompt
                  <span class="mt-0.5 block text-[10px] text-zinc-600">
                    Writes a one-page static portfolio from a profile spec carried
                    in the prompt.
                  </span>
                </button>
              </section>

              <section>
                <h3 class="mb-2 text-[11px] font-semibold uppercase tracking-wider text-zinc-500">
                  agent
                </h3>
                <div class="space-y-1">
                  <For each={props.harnesses}>
                    {(h) => (
                      <button
                        type="button"
                        onClick={() => props.onSelect(h.id)}
                        aria-pressed={props.selected === h.id}
                        class={`w-full rounded-lg border px-3 py-2 text-left transition ${
                          props.selected === h.id
                            ? 'border-emerald-500/50 bg-emerald-500/5'
                            : 'border-hairline hover:border-zinc-700 hover:bg-surface-2/60'
                        }`}
                      >
                        <div class="flex items-center gap-2">
                          <span
                            class={`size-1.5 shrink-0 rounded-full ${h.installed ? 'bg-emerald-400' : 'bg-red-400'}`}
                          />
                          <span class="min-w-0 flex-1 truncate text-[13px] text-zinc-200">
                            {h.name}
                          </span>
                          <span
                            class={`shrink-0 rounded px-1.5 py-0.5 text-[9px] uppercase tracking-wider ${
                              costBadge[h.cost] ?? 'bg-zinc-800 text-zinc-500'
                            }`}
                          >
                            {h.cost}
                          </span>
                        </div>
                        <div class="mono mt-1 truncate text-[10px] text-zinc-600">
                          {h.command}
                        </div>
                        <Show when={!h.installed}>
                          <div class="mt-1 text-[10px] text-amber-400/80">
                            not on PATH — a run would fail to start
                          </div>
                        </Show>
                      </button>
                    )}
                  </For>
                </div>
                <p class="mt-2 text-[10px] leading-relaxed text-zinc-700">
                  {installedCount(props.harnesses)} of {props.harnesses.length} installed
                  <Show when={props.autoPicked}> · the first is chosen for you</Show>
                </p>
              </section>

              <section>
                <h3 class="mb-2 text-[11px] font-semibold uppercase tracking-wider text-zinc-500">
                  model
                </h3>
                <select
                  value={props.model}
                  onChange={(e) => props.onModel(e.currentTarget.value)}
                  aria-label="Model"
                  class="mono w-full rounded-lg border border-hairline bg-surface-2 px-3 py-2 text-xs text-zinc-200 outline-none transition placeholder:text-zinc-600 focus:border-emerald-500/60"
                >
                  <option value="">Automatic · Gemma-first</option>
                  <For each={props.workflowModels}>{m=><option value={m.id} disabled={m.provider==='openai' && !props.allowGPT}>{m.id}</option>}</For>
                </select>
                <label class="mt-3 flex items-center gap-2 text-xs text-zinc-400">
                  <input type="checkbox" checked={props.allowGPT} onChange={e=>{props.onAllowGPT(e.currentTarget.checked);if(!e.currentTarget.checked && props.model.startsWith('openai/'))props.onModel('')}} />
                  Allow ChatGPT subscription for follow-up work and repairs
                </label>
                <p class="mt-1.5 text-[10px] leading-relaxed text-zinc-700">
                  Gemma plans and builds the first version. When enabled, GPT-6.1 Sol handles follow-up work and repairs, with up to two attempts per stage. Remaining provider quota and subscription cost are unknown.
                </p>
              </section>

              <section>
                <h3 class="mb-2 text-[11px] font-semibold uppercase tracking-wider text-zinc-500">
                  permissions
                </h3>
                <label class="-mx-1 flex cursor-pointer items-start gap-2.5 rounded-lg px-1 py-1.5 transition hover:bg-surface-2/60">
                  <input
                    type="checkbox"
                    checked={props.autoApprove}
                    onChange={(e) => props.onAutoApprove(e.currentTarget.checked)}
                    class="mt-0.5 size-4 shrink-0 accent-emerald-500"
                  />
                  <span class="text-[12px] leading-snug text-zinc-400">
                    Allow tools inside the sandbox
                    <span class="mt-0.5 block text-[10px] leading-relaxed text-zinc-700">
                      Enables the agent to write files and run commands in its workspace.
                      When off, requests are denied automatically; no approval dialog is shown.
                    </span>
                  </span>
                </label>
              </section>

              <section>
                <h3 class="mb-2 text-[11px] font-semibold uppercase tracking-wider text-zinc-500">
                  time limit
                </h3>
                <div class="flex gap-1">
                  <For each={[0, 300, 600, 1200, 1800]}>
                    {(sec) => (
                      <button
                        type="button"
                        onClick={() => props.onTimeLimit(sec)}
                        aria-pressed={props.timeLimitSec === sec}
                        class={`flex-1 rounded-lg border px-2 py-2 text-[12px] transition ${
                          props.timeLimitSec === sec
                            ? 'border-emerald-500/50 bg-emerald-500/5 text-emerald-200'
                            : 'border-hairline text-zinc-400 hover:border-zinc-700 hover:text-zinc-200'
                        }`}
                      >
                        {sec === 0 ? 'No limit' : `${sec / 60}m`}
                      </button>
                    )}
                  </For>
                </div>
                <p class="mt-1.5 text-[10px] leading-relaxed text-zinc-700">
                  No overall limit by default. Stop cancels the job; repeated failures
                  are bounded. Quiet model generation alone is not considered a loop.
                </p>
              </section>
            </div>

            <div class="shrink-0 border-t border-hairline px-4 py-3 text-[10px] leading-relaxed text-zinc-700">
              Every run executes under Landlock confinement in its own workspace.
              Tool access is controlled above. Cloud models may receive project code.
            </div>
          </aside>
        </dialog>
      </Portal>
    </Show>
  )
}
