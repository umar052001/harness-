import { For, Show } from 'solid-js'
import type { RunEvent } from './types'
import { formatWorkflowCost } from './usage'

type Accounting = { tokens?: { input?:number; output?:number; cacheRead?:number }; estimatedUSD?:number; estimatedUSDRange?:number[]; billing:string }
type RecordRow = { at:string; phase:string; model?:string; reason?:string; result:string; accounting?:Accounting; evidence?:{graphifyCompleted:boolean;ponytailInjected:boolean} }
type Plan = { problem:string; requirements:string[]; constraints:string[]; assumptions:string[]; acceptanceCriteria:string[]; codebaseFindings:string[]; research:string[]; sequence:string[]; validation:string[]; quality:string[]; stages:{model:string;task:string;reason:string;escalation?:string}[] }
type Total = { input?:number;output?:number;cacheRead?:number;unknownUsageCalls:number;unknownCostCalls:number;knownCostEstimateUSDRange?:number[] }

export default function Workflow(props:{events:RunEvent[]}) {
  const rows=()=>props.events.filter(e=>e.kind==='workflow.record').map(e=>e.data as unknown as RecordRow)
  const plan=()=>props.events.findLast(e=>e.kind==='workflow.plan')?.data as unknown as Plan|undefined
  const summary=()=>props.events.findLast(e=>e.kind==='workflow.summary')?.data as unknown as {totals:Total;models:Record<string,Total>;note:string}|undefined
  const number=(n?:number|null)=>n==null?'Unknown':n.toLocaleString()
  const cost=(a?:Accounting)=>a?.estimatedUSD!=null?`$${a.estimatedUSD.toFixed(6)} equivalent`:a?.estimatedUSDRange?.length===2?`$${a.estimatedUSDRange[0].toFixed(6)}–$${a.estimatedUSDRange[1].toFixed(6)} equivalent`:'Unknown / subscription'
  const range=formatWorkflowCost
  return <details class="usage-panel" open>
    <summary>Routed build · Gemma / subscription GPT · {summary()?`${number(summary()?.totals.input)} input, ${number(summary()?.totals.output)} output`:'usage updates after each session'}</summary>
    <Show when={plan()}>{p=><details><summary>Engineering plan: {p().problem}</summary>
      <For each={[['Requirements',p().requirements],['Assumptions',p().assumptions],['Acceptance',p().acceptanceCriteria]] as [string,string[]][]}>{section=><div><strong>{section[0]}</strong><ul><For each={section[1]}>{item=><li>{item}</li>}</For></ul></div>}</For>
      <For each={[['Constraints',p().constraints],['Codebase findings',p().codebaseFindings],['Research',p().research],['Sequence',p().sequence],['Validation',p().validation],['Quality',p().quality]] as [string,string[]][]}>{section=><div><strong>{section[0]}</strong><ul><For each={section[1]}>{item=><li>{item}</li>}</For></ul></div>}</For>
      <For each={p().stages}>{s=><p><strong>{s.model}</strong> · {s.task}<br />{s.reason} {s.escalation}</p>}</For>
    </details>}</Show>
    <For each={rows()}>{r=><details><summary>{r.phase} {r.model && `· ${r.model}`} · {r.result}</summary>
      <Show when={r.reason}><p>{r.reason}</p></Show>
      <Show when={r.accounting}>{a=><p>Input {number(a().tokens?.input)} · Output {number(a().tokens?.output)} · Cache read {number(a().tokens?.cacheRead)} · {cost(a())}<br />{a().billing}</p>}</Show>
      <Show when={r.evidence}>{e=><p>Graphify retrieved: {e().graphifyCompleted?'yes':'not observed'} · Ponytail injected: {e().ponytailInjected?'yes':'not observed'}</p>}</Show>
    </details>}</For>
    <Show when={summary()}>{s=><div><For each={Object.entries(s().models)}>{([model,t])=><p><strong>{model}</strong>: {number(t.input)} input · {number(t.output)} output · {number(t.cacheRead)} cache read · {t.unknownUsageCalls} calls with missing usage · {t.unknownCostCalls} calls with unknown cost<br />{range(t)}</p>}</For><p>Combined: {range(s().totals)}</p><p>{s().note}</p></div>}</Show>
  </details>
}
