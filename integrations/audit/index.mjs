import fs from 'node:fs'
import path from 'node:path'

// Observe real plugin injection and tool execution, not configuration presence.
export default {
  id: 'harness.workflow-evidence',
  async setup(ctx) {
    const file = path.join(process.env.HOME, 'workflow-evidence.jsonl')
    const record = value => fs.appendFileSync(file, JSON.stringify(value) + '\n', { mode: 0o600 })
    await ctx.session.hook('context', event => {
      const system = JSON.stringify(event.system)
      record({ type: 'context', ponytailInjected: /lazy senior|smallest complete change|Ponytail/i.test(system) })
    })
    let graphUsed = false
    await ctx.tool.hook('execute.before', event => {
      if (!graphUsed && ['write', 'edit', 'patch', 'shell'].includes(event.tool)) {
        throw new Error('Graphify is mandatory before edits. Use execute/search to discover graphify query_graph, then query the graph.')
      }
    })
    await ctx.tool.hook('execute.after', event => {
      if (/graphify/i.test(event.tool) || /graphify.*query_graph/.test(JSON.stringify(event.input))) {
        graphUsed = true
        record({ type: 'graphify-tool', tool: event.tool })
      }
    })
  },
}
