import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import plugin from './index.mjs'

test('actual injection is observed and Graphify gates edits', async () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'harness-audit-'))
  const original = process.env.HOME
  process.env.HOME = home
  try {
    const hooks = {}
    await plugin.setup({
      session: { hook: async (name, fn) => { hooks[name] = fn } },
      tool: { hook: async (name, fn) => { hooks[name] = fn } },
    })
    hooks.context({ system: [{ text: 'ordinary prompt' }] })
    hooks.context({ system: [{ text: 'You are a lazy senior developer. Ponytail.' }] })
    assert.throws(() => hooks['execute.before']({ tool: 'write' }), /Graphify is mandatory/)
    hooks['execute.after']({ tool: 'graphify_query_graph' })
    assert.doesNotThrow(() => hooks['execute.before']({ tool: 'write' }))
    const rows = fs.readFileSync(path.join(home, 'workflow-evidence.jsonl'), 'utf8').trim().split('\n').map(JSON.parse)
    assert.equal(rows[0].ponytailInjected, false)
    assert.equal(rows[1].ponytailInjected, true)
    assert.equal(rows[2].type, 'graphify-tool')
  } finally {
    process.env.HOME = original
    fs.rmSync(home, { recursive: true })
  }
})
