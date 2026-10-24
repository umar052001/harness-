import { createEffect, Show } from 'solid-js'
import type { Harness } from './types'

export default function ChatBar(props: {
  prompt: string; onPrompt: (value: string) => void; onRun: () => void; onStop: () => void
  onSettings: () => void; running: boolean; ready: boolean; harness: Harness | undefined
  autoApprove: boolean; onAutoApprove: (value: boolean) => void; model: string; elapsedSec: number
}) {
  let area: HTMLTextAreaElement | undefined
  const canSend = () => props.ready && !props.running && !!props.prompt.trim()
  createEffect(() => {
    props.prompt
    if (area) { area.style.height = 'auto'; area.style.height = `${Math.min(area.scrollHeight, 132)}px` }
  })
  return (
    <footer class="composer-wrap">
      <div class="composer">
        <textarea ref={area} rows={1} value={props.prompt} aria-label="Prompt" disabled={props.running}
          placeholder="Describe what you want to build…" onInput={e => props.onPrompt(e.currentTarget.value)}
          onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); if (canSend()) props.onRun() } }} />
        <div class="composer-controls">
          <button class="model-button" onClick={props.onSettings}><span class="model-icon">◈</span>{props.model}<span class="chevron">⌄</span></button>
          <label class="tool-toggle" title="Allow the agent to use tools inside this run’s sandbox">
            <input type="checkbox" checked={props.autoApprove} disabled={props.running} onChange={e => props.onAutoApprove(e.currentTarget.checked)} />
            <span>Allow tools</span>
          </label>
          <span class="composer-spacer" />
          <Show when={!props.running} fallback={<button class="stop-button" onClick={props.onStop}>■ Stop run</button>}>
            <button class="build-button" disabled={!canSend()} onClick={props.onRun}>Build <span aria-hidden="true">↑</span></button>
          </Show>
        </div>
      </div>
      <div class="composer-caption"><span>{props.autoApprove ? 'Tools enabled inside the sandbox' : 'Tools are off — enable “Allow tools” to let the agent build'}</span><span>Enter to build · Shift + Enter for a new line</span></div>
    </footer>
  )
}
