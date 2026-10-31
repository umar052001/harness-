import type { RunEvent, UsageReport } from './types'

/** Accounting events contain cumulative snapshots, never additive increments. */
export function latestUsage(events: RunEvent[], saved?: UsageReport): UsageReport | undefined {
  // Terminal metadata can be more complete than a size-limited transcript replay.
  if (saved?.tokens || saved?.cost || saved?.context) return saved
  for (let i = events.length - 1; i >= 0; i--) {
    const report = events[i].data?.usage
    if (events[i].kind === 'usage' && report && typeof report === 'object') return report as UsageReport
  }
  return saved
}

export function formatCost(report?: UsageReport): string {
  const cost = report?.cost
  if (!cost || !Number.isFinite(cost.amount) || cost.amount < 0) return 'Cost unknown'
  const amount = cost.amount > 0 && cost.amount < 0.0001 ? '<0.0001' : cost.amount.toFixed(4)
  return `${cost.currency} ${amount} estimated`
}

export function formatWorkflowCost(total: { knownCostEstimateUSDRange?: number[] | null; unknownCostCalls: number }): string {
  const range = total.knownCostEstimateUSDRange
  if (!range || range.length !== 2 || range.some(n => !Number.isFinite(n) || n < 0)) return 'Cost unknown / subscription'
  return `$${range[0].toFixed(6)}–$${range[1].toFixed(6)} known-rate equivalent${total.unknownCostCalls ? ' (partial; unknown costs excluded)' : ''}`
}
