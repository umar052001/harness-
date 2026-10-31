import { Show } from 'solid-js'
import { formatCost } from './usage'
import type { UsageReport } from './types'

export default function Usage(props: { report?: UsageReport; model: string }) {
  const number = (value?: number) => value === undefined ? 'Unknown' : value.toLocaleString()
  return (
    <details class="usage-panel">
      <summary>{props.model} · {props.report?.tokens ? `${number(props.report.tokens.totalTokens)} tokens` : 'Tokens unknown'} · {formatCost(props.report)}</summary>
      <dl>
        <div><dt>Input</dt><dd>{number(props.report?.tokens?.inputTokens)}</dd></div>
        <div><dt>Output</dt><dd>{number(props.report?.tokens?.outputTokens)}</dd></div>
        <div><dt>Reasoning</dt><dd>{number(props.report?.tokens?.thoughtTokens)}</dd></div>
        <div><dt>Cache read</dt><dd>{number(props.report?.tokens?.cachedReadTokens)}</dd></div>
        <div><dt>Cache write</dt><dd>{number(props.report?.tokens?.cachedWriteTokens)}</dd></div>
        <div><dt>Context window</dt><dd>{props.report?.context ? `${number(props.report.context.used)} / ${number(props.report.context.size)}` : 'Unknown'}</dd></div>
      </dl>
      <p>Reported by the agent via ACP. Cost is an estimate, not a provider invoice. Token categories may overlap; context size is not total consumption.</p>
      <Show when={!props.report?.tokens}><p>Token totals may arrive when the run finishes. Interrupted runs may have incomplete accounting.</p></Show>
    </details>
  )
}
