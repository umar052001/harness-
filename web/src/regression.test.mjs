import test from 'node:test'
import assert from 'node:assert/strict'
import { streamRun } from './api.ts'
import { build } from './events.ts'

test('routing decisions are visible and stage switches do not reset elapsed time',()=>{
  const {items,summary}=build([
    {kind:'workflow.record',seq:1,at:'2026-01-01T00:00:00Z',text:'planning-routing: complete'},
    {kind:'workflow.stage',seq:2,at:'2026-01-01T00:00:01Z',text:'ollama/gemma4:cloud'},
    {kind:'run.started',seq:3,at:'2026-01-01T00:00:02Z'},
    {kind:'workflow.record',seq:4,at:'2026-01-01T00:00:05Z',text:'fallback: openai/gpt-6.1-sol'},
    {kind:'run.started',seq:5,at:'2026-01-01T00:00:06Z'},
    {kind:'run.finished',seq:6,at:'2026-01-01T00:00:10Z',data:{ok:true,status:'completed'}},
  ])
  assert.equal(summary.elapsedSec,10)
  assert.ok(items.some(i=>i.title==='fallback: openai/gpt-6.1-sol'))
})

test('network failure, abort, and truncated streams cannot look successful', async t => {
  const original = globalThis.fetch
  t.after(() => { globalThis.fetch = original })
  const req = { harnessId: 'opencode', prompt: 'hello' }
  globalThis.fetch = async () => { throw new TypeError('offline') }
  assert.equal((await streamRun(req, () => {})).ok, false)
  const controller = new AbortController()
  controller.abort()
  assert.match((await streamRun(req, () => {}, controller.signal)).error, /Disconnected/)
  globalThis.fetch = async () => new Response('event: message\ndata: {"kind":"message","text":"partial"}\n\n')
  assert.equal((await streamRun(req, () => {})).ok, false)
  globalThis.fetch = async () => new Response('event: stream.end\ndata: {"ok":true}\n\n')
  assert.equal((await streamRun(req, () => {})).ok, true)
})

test('message deltas preserve repeated tokens, whitespace, and punctuation', () => {
  const chunks = ['Hello', ' ', 'ha', 'ha', '!\n', 'Done.']
  const events = chunks.map((text, seq) => ({ kind: 'message', text, seq, at: '2026-01-01T00:00:00Z' }))
  events.push({ kind: 'message', text: chunks.join(''), data: { final: true }, at: '2026-01-01T00:00:01Z' })
  const { items } = build(events)
  assert.equal(items.length, 1)
  assert.equal(items[0].text, 'Hello haha!\nDone.')
})

test('finished failures freeze elapsed time and permissions never wait forever', () => {
  const { items, summary } = build([
    { kind: 'run.started', at: '2026-01-01T00:00:00Z' },
    { kind: 'permission', at: '2026-01-01T00:00:02Z', data: { status: 'denied' } },
    { kind: 'run.finished', at: '2026-01-01T00:00:10Z', data: { ok: false, status: 'cancelled' } },
  ])
  assert.equal(summary.elapsedSec, 10)
  assert.equal(items.find(i => i.kind === 'step').status, 'done')
})
