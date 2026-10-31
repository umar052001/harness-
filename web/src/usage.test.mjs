import test from 'node:test'
import assert from 'node:assert/strict'
import { latestUsage, formatCost, formatWorkflowCost } from './usage.ts'

test('combined workflow cost never treats missing subscription prices as free',()=>{
  assert.equal(formatWorkflowCost({knownCostEstimateUSDRange:null,unknownCostCalls:1}),'Cost unknown / subscription')
  assert.match(formatWorkflowCost({knownCostEstimateUSDRange:[0.01,0.02],unknownCostCalls:1}),/partial; unknown costs excluded/)
  assert.equal(formatWorkflowCost({knownCostEstimateUSDRange:[0,0],unknownCostCalls:0}),'$0.000000–$0.000000 known-rate equivalent')
})

test('missing usage is unknown, an explicit zero estimate is preserved', () => {
  assert.equal(formatCost(), 'Cost unknown')
  assert.equal(formatCost({ cost: { amount: 0, currency: 'USD' } }), 'USD 0.0000 estimated')
  assert.equal(formatCost({ cost: { amount: 0.000001, currency: 'USD' } }), 'USD <0.0001 estimated')
  assert.equal(formatCost({ cost: { amount: NaN, currency: 'USD' } }), 'Cost unknown')
})

test('repeated snapshots are replaced rather than summed and history survives reload', () => {
  const report = { tokens: { totalTokens: 100, inputTokens: 80, outputTokens: 20 } }
  const events = [1, 2].map(() => ({ kind: 'usage', data: { usage: report } }))
  events.push({ kind: 'usage', data: { durationMs: 1000 } })
  assert.deepEqual(latestUsage(events), report)
  assert.deepEqual(latestUsage([], report), report)
  assert.deepEqual(latestUsage([{ kind: 'usage', data: { usage: { context: { used: 10, size: 100 } } } }], report), report)
  assert.equal(latestUsage([]), undefined)
})
