import test from 'node:test'
import assert from 'node:assert/strict'
import { appStatusLabel } from './app-status.ts'

test('HTTP readiness is not reported as functional verification', () => {
  assert.match(appStatusLabel(), /status unavailable/)
  assert.match(appStatusLabel({ status: 'ready', verification: 'not_run' }), /checks not supplied/)
  assert.match(appStatusLabel({ status: 'ready', verification: 'passed', checksPassed: 3, checksTotal: 3 }), /3\/3 project checks passed/)
  assert.match(appStatusLabel({ status: 'failed', verification: 'passed', error: 'api exited' }), /App failed.*api exited/)
  assert.match(appStatusLabel({ status: 'stopped', verification: 'passed' }), /App stopped/)
})
