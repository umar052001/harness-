import type { RunEvent } from './types'

/**
 * Turns the raw event stream into something a person can read.
 *
 * The raw stream is a control-plane format: typed events, sequence numbers, a
 * sandbox policy dump on stderr and one event per protocol frame. It is correct
 * and unreadable, and rendering it verbatim was the first version of this
 * dashboard. Everything here exists to answer "what is happening right now"
 * without throwing away evidence: nothing is dropped silently, it is either
 * named, grouped, or moved behind a disclosure.
 */

/** A tone maps to the colour language used across the app. */
export type Tone = 'idle' | 'run' | 'good' | 'warn' | 'bad'

/** One line in the activity feed. */
export type Item =
  | {
      kind: 'phase'
      id: string
      title: string
      detail?: string
      tone: Tone
      at: string
    }
  | {
      kind: 'step'
      id: string
      title: string
      detail?: string
      status: 'running' | 'done' | 'failed'
      tone: Tone
      at: string
      repeats: number
      /** The verb the action was grouped under, e.g. "Reading". */
      verb: string
      /**
       * Rows folded into this one, oldest first.
       *
       * Grouping is a presentation decision, not a deletion: everything a
       * grouped row swallowed is still here, behind a disclosure.
       */
      folded?: { title: string; detail?: string; at: string }[]
    }
  | { kind: 'say'; id: string; text: string; at: string; final: boolean }
  | { kind: 'log'; id: string; lines: string[]; tone: Tone; title: string; at: string }

/** Phases a run moves through. Used for both the feed and the status line. */
export type Phase = 'idle' | 'starting' | 'working' | 'done' | 'failed'

/** Counters and the sentence describing what the run is doing. */
export type Summary = {
  phase: Phase
  step: string
  harness: string
  tools: number
  files: number
  elapsedSec: number
  error: string
  version: string
}

/**
 * sandboxBanner matches the executor's own policy report.
 *
 * It is roughly twenty lines of "+ /usr (ro,exec)" and ABI notes emitted on
 * every single run. Shown inline it drowned out the agent's actual work - in a
 * thirty-four event run, twenty of them were this - so it is captured once and
 * rendered behind a single disclosure instead.
 */
const sandboxBanner = /^(Landlock|compiled vs|fs scoping|net scoping|verdict|policy:|no_new_privs|writable :|read-only :|\+|everything else:|confinement:|rules :|enforcement:|exec :|---)/

/** toolLabels give a tool a verb instead of a bare noun. */
const toolLabels: Record<string, { verb: string; icon: string }> = {
  bash: { verb: 'Running', icon: '⌘' },
  shell: { verb: 'Running', icon: '⌘' },
  execute: { verb: 'Running', icon: '⌘' },
  edit: { verb: 'Editing', icon: '✎' },
  write: { verb: 'Writing', icon: '✎' },
  multiedit: { verb: 'Editing', icon: '✎' },
  patch: { verb: 'Editing', icon: '✎' },
  read: { verb: 'Reading', icon: '☰' },
  view: { verb: 'Reading', icon: '☰' },
  glob: { verb: 'Finding', icon: '☰' },
  grep: { verb: 'Searching', icon: '⌕' },
  search: { verb: 'Searching', icon: '⌕' },
  list: { verb: 'Listing', icon: '☰' },
  ls: { verb: 'Listing', icon: '☰' },
  webfetch: { verb: 'Fetching', icon: '↓' },
  fetch: { verb: 'Fetching', icon: '↓' },
  websearch: { verb: 'Searching the web', icon: '⌕' },
  todowrite: { verb: 'Planning tasks', icon: '✓' },
  task: { verb: 'Delegating', icon: '⇢' },
  todoread: { verb: 'Reading tasks', icon: '✓' },
}

const fallbackTool = { verb: 'Using', icon: '•' }

/** str is a small, total helper: unknown values become an empty string. */
function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

/** oneLine flattens text so a multi-line agent message cannot break layout. */
function oneLine(s: string, max = 400): string {
  const flat = s.replace(/\s*\n+\s*/g, ' ').trim()
  return flat.length > max ? `${flat.slice(0, max - 1)}…` : flat
}

/**
 * dedupeError removes the repeated prefixes a wrapped error accumulates.
 *
 * "acp: session/prompt: acp: session/prompt: no response after 2m" is what a
 * user saw: the same message wrapped twice on its way out. Stripping a prefix
 * that is already there costs nothing and reads like one error.
 */
function dedupeError(msg: string): string {
  let out = msg
  for (let i = 0; i < 4; i++) {
    const next = out.replace(/^(acp:\s*)+/, '')
    if (next === out) break
    out = next
  }
  return oneLine(out)
}

/** pathish reports whether a tool's "command" is really a file it touched. */
function pathish(s: string): boolean {
  return s.length > 0 && !/\s/.test(s) && /^[\w.~/-]+$/.test(s)
}

/**
 * squash removes whitespace so two versions of the same sentence can be compared.
 *
 * Chunk boundaries land wherever the tokeniser stopped, so "hello" + ".txt"
 * becomes "hello . txt" once flattened for display. Comparing the flattened text
 * to decide whether a message is a restatement therefore fails on exactly the
 * messages where it matters, and the sentence gets printed twice.
 */
function squash(s: string): string {
  return s.replace(/\s+/g, '')
}

/**
 * toolLabel turns a tool event into a sentence about an action.
 *
 * The important input is `command`, which the runner extracts from the call's
 * raw input. Without it the best available text is the tool's name, and "bash"
 * on its own tells a waiting user nothing. A path-shaped argument gets the file
 * named instead of the tool, because "Writing App.tsx" and "Reading App.tsx" are
 * what happened while "Writing command" and "Reading command" are not - the
 * latter names the wrong kind of thing.
 */
function toolLabel(ev: RunEvent): { title: string; detail?: string; verb: string } {
  const tool = str(ev.data?.tool) || str(ev.data?.title) || str(ev.data?.toolKind) || 'tool'
  const key = tool.toLowerCase().replace(/[^a-z]/g, '')
  const meta = toolLabels[key] ?? fallbackTool
  const command = oneLine(str(ev.data?.command), 220)
  // A shell tool's argument is a command even when it is a single word, so only
  // the file tools are allowed to name a path.
  const namesPaths = !['bash', 'shell', 'execute'].includes(key)

  if (command) {
    if (namesPaths && pathish(command)) {
      const base = command.split('/').filter(Boolean).pop() ?? command
      return { title: `${meta.verb} ${base}`, detail: shortPath(command), verb: meta.verb }
    }
    if (command !== tool) return { title: `${meta.verb} command`, detail: command, verb: meta.verb }
    return { title: `${meta.verb} ${tool}`, verb: meta.verb }
  }

  const paths = Array.isArray(ev.data?.paths) ? (ev.data!.paths as unknown[]).map(str).filter(Boolean) : []
  if (paths.length > 0) {
    const base = paths[0].split('/').filter(Boolean).pop() ?? paths[0]
    return { title: `${meta.verb} ${base}`, detail: shortPath(paths[0]), verb: meta.verb }
  }
  return { title: `${meta.verb} ${tool}`, verb: meta.verb }
}

/** exitCode pulls a process exit code out of an executor report. */
function exitCode(s: string): number {
  const m = /exit(?:ed)?(?:\s+with)?\s*(?:code\s*)?(\d+)/i.exec(s)
  if (!m) return -1
  const n = Number(m[1])
  return Number.isNaN(n) ? -1 : n
}

/** shortPath trims a workspace path to something that fits a line. */
function shortPath(p: string): string {
  const parts = p.split('/').filter(Boolean)
  return parts.length <= 3 ? p : `…/${parts.slice(-3).join('/')}`
}

/**
 * toolStatus maps the protocol's statuses onto three states a person reads.
 */
function toolStatus(status: string): 'running' | 'done' | 'failed' {
  switch (status) {
    case 'completed':
    case 'success':
      return 'done'
    case 'failed':
    case 'error':
      return 'failed'
    default:
      return 'running'
  }
}

/**
 * build is the whole interpretation, as a pure function of the event log.
 *
 * Recomputing from the full log on every event is deliberate: the stream is
 * short (hundreds of events, not millions), and deriving the view means the feed
 * cannot drift out of sync with the events - a client that mutates rows in place
 * gets it wrong the first time an event arrives out of order.
 */
export function build(events: RunEvent[]): { items: Item[]; summary: Summary } {
  const items: Item[] = []
  const summary: Summary = {
    // idle, not starting: with no events there is no run, and a page that has
    // just loaded was claiming "Starting the agent." until you typed something.
    phase: 'idle',
    step: '',
    harness: '',
    tools: 0,
    files: 0,
    elapsedSec: 0,
    error: '',
    version: '',
  }
  // Tool calls are correlated by id so one call is one row that updates in
  // place, instead of the four duplicate rows a re-titled call produces.
  const stepIndex = new Map<string, Item & { kind: 'step' }>()
  const said = new Set<string>()
  let logLines: string[] = []
  let sandboxPolicy: string[] = []
  let started = 0
  // finished flips when the run reaches its terminal event, which is what tells
  // the sandbox's teardown report apart from its start-up report.
  let finished = false

  const flushLog = () => {
    if (logLines.length === 0) return
    items.push({
      kind: 'log',
      id: `log-${logLines.length}-${items.length}`,
      lines: logLines,
      tone: 'idle',
      title: `${logLines.length} line${logLines.length === 1 ? '' : 's'} of agent output`,
      at: new Date().toISOString(),
    })
    logLines = []
  }

  for (const ev of events) {
    const d = ev.data as Record<string, unknown> | undefined
    const evText = str(ev.text)
    switch (ev.kind) {
      case 'run.assigned':
        // The workspace id is handled by the caller; it is plumbing, not news.
        break

      case 'harness.selected': {
        const agent = str(d?.agent)
        if (agent) {
          summary.version = str(d?.version)
          flushLog()
          items.push({
            kind: 'phase',
            id: `phase-agent-${ev.seq}`,
            title: `${agent} connected`,
            detail: summary.version ? `version ${summary.version}` : undefined,
            tone: 'good',
            at: ev.at,
          })
        } else {
          summary.harness = evText || 'agent'
          summary.phase = 'starting'
        }
        break
      }

      case 'run.started':
        started = Date.parse(ev.at) || Date.now()
        items.push({
          kind: 'phase',
          id: `phase-start-${ev.seq}`,
          title: 'Preparing a confined workspace',
          detail: evText,
          tone: 'run',
          at: ev.at,
        })
        break

      case 'sandbox.enforcement': {
        flushLog()
        // The executor reports twice per run: once when the ruleset is applied,
        // and once when the agent process is reaped. An ACP agent is a
        // long-lived server that has to be killed at teardown, so the second
        // report almost always carries a signal exit code on an otherwise
        // successful run. Rendering that in error red, right under "Finished",
        // reads as a failure that did not happen.
        const reaped = finished
        const code = exitCode(evText)
        const clean = code === 0
        // Teardown produces one report for the ruleset and one for the process.
        // Two near-identical lines at the end of a successful run read as two
        // separate problems, so they share a row.
        const prev = items.length > 0 ? items[items.length - 1] : undefined
        if (reaped && prev?.kind === 'phase' && prev.id.startsWith('phase-sandbox')) {
          prev.title = clean ? 'Agent process exited cleanly' : 'Session closed, agent process reaped'
          prev.detail = prev.detail && prev.detail !== evText ? `${prev.detail} · ${oneLine(evText, 60)}` : oneLine(evText, 120)
          break
        }
        items.push({
          kind: 'phase',
          id: `phase-sandbox-${ev.seq ?? items.length}`,
          title: reaped
            ? clean
              ? 'Agent process exited cleanly'
              : 'Session closed, agent process reaped'
            : 'Sandbox enforced',
          detail: oneLine(evText, 120),
          tone: reaped ? 'idle' : 'good',
          at: ev.at,
        })
        break
      }

      case 'tool.call':
      case 'tool.update': {
        summary.phase = 'working'
        const id = str(d?.id) || `${ev.kind}-${ev.seq}`
        const { title, detail, verb } = toolLabel(ev)
        const status = toolStatus(str(d?.status))
        const existing = stepIndex.get(id)
        if (existing) {
          // Update in place: the command arrives on a later frame than the name.
          existing.title = title
          if (detail) existing.detail = detail
          existing.status = status
          existing.tone = status === 'failed' ? 'bad' : status === 'done' ? 'idle' : 'run'
          if (status !== 'running') summary.step = ''
        } else {
          // Counted here, once per call. Counting on every frame counted the
          // update that follows each call too, so the status line reported
          // roughly twice as many tool calls as the agent actually made.
          summary.tools++
          const step: Item & { kind: 'step' } = {
            kind: 'step',
            id,
            title,
            detail,
            status,
            tone: status === 'failed' ? 'bad' : status === 'done' ? 'idle' : 'run',
            at: ev.at,
            repeats: 1,
            verb,
          }
          stepIndex.set(id, step)
          items.push(step)
        }
        summary.step = status === 'running' ? (detail ?? title) : summary.step
        if (status === 'running') summary.step = detail ? `${title}: ${detail}` : title
        break
      }

      case 'permission':
        flushLog()
        items.push({
          kind: 'step',
          id: `perm-${ev.seq}`,
          title: d?.status === 'approved' ? 'Tool approved' : 'Tool denied — enable Allow tools for a new run',
          detail: evText,
          status: 'done',
          tone: 'warn',
          at: ev.at,
          repeats: 1,
          verb: 'Waiting',
        })
        break

      case 'message': {
        // An answer arrives as deltas and then, usually, as the assembled text
        // again. Rendering each delta as its own quote splits one sentence
        // across rows and then says it a second time, so consecutive chunks are
        // folded into a single row: a chunk that restates the row replaces it, a
        // chunk that extends it is appended, and the assembled restatement is
        // absorbed rather than repeated.
        const text = evText
        if (!text) break
        const final = d?.final === true
        if (final) {
          const messages = items.filter((item): item is Item & { kind: 'say' } => item.kind === 'say')
          if (squash(messages.map(item => item.text).join('')) === squash(text) && messages.length) {
            messages[messages.length - 1].final = true
            break
          }
        }
        const last = items.length > 0 ? items[items.length - 1] : undefined
        const open = last?.kind === 'say' && !last.final ? last : null

        if (open) {
          if (final) open.text = text
          else open.text += text
          if (final) open.final = true
          break
        }
        if (said.has(text)) break
        said.add(text)
        items.push({ kind: 'say', id: `say-${ev.seq ?? items.length}`, text, at: ev.at, final })
        break
      }

      case 'plan':
        flushLog()
        items.push({
          kind: 'phase',
          id: `phase-plan-${ev.seq}`,
          title: 'Agent laid out a plan',
          detail: oneLine(evText, 160),
          tone: 'idle',
          at: ev.at,
        })
        break

      case 'output':
        // The executor's policy report is collected, not interleaved.
        if (sandboxBanner.test(evText)) {
          sandboxPolicy.push(evText)
          break
        }
        logLines.push(evText)
        break

      case 'error':
        summary.phase = 'failed'
        summary.error = dedupeError(evText)
        flushLog()
        items.push({
          kind: 'phase',
          id: `phase-error-${ev.seq}`,
          title: 'Run failed',
          detail: summary.error,
          tone: 'bad',
          at: ev.at,
        })
        break

      case 'run.finished': {
        flushLog()
        const ok = d?.ok !== false
        finished = true
        summary.phase = ok ? 'done' : 'failed'
        if (!ok && !summary.error) summary.error = str(d?.error) || evText || 'the run failed'
        if (sandboxPolicy.length > 0) {
          items.push({
            kind: 'log',
            id: `sandbox-policy-${items.length}`,
            lines: sandboxPolicy,
            tone: 'good',
            title: `Sandbox policy · ${sandboxPolicy.length} line${sandboxPolicy.length === 1 ? '' : 's'}`,
            at: ev.at,
          })
          sandboxPolicy = []
        }
        items.push({
          kind: 'phase',
          id: `phase-done-${ev.seq}`,
          title: ok ? 'Finished' : d?.status === 'cancelled' ? 'Run stopped' : d?.status === 'timed_out' ? 'Time limit reached' : 'Stopped with an error',
          detail: summary.error || undefined,
          tone: ok ? 'good' : 'bad',
          at: ev.at,
        })
        if (started > 0) summary.elapsedSec = Math.max(0, Math.round((Date.parse(ev.at) - started) / 1000))
        break
      }

      default:
        if (evText) logLines.push(evText)
    }
  }
  flushLog()
  if (started > 0 && !finished) {
    summary.elapsedSec = Math.max(0, Math.round((Date.now() - started) / 1000))
  }
  return { items: groupRuns(items), summary }
}

/**
 * groupRuns folds a run of identical actions into a single row.
 *
 * A build that writes thirty files emits thirty `Writing` rows, and reading
 * them is work rather than information: the reader wants to know the agent
 * wrote thirty files and which ones failed, not the thirty near-identical lines
 * in between. So consecutive steps sharing a verb collapse into one row that
 * counts them and lists what was folded away inside a disclosure - the evidence
 * is moved, never dropped.
 *
 * Only finished work is folded. A running row is the current position in the
 * run and stays visible, and so does any failure, because those are the two
 * things a reader is actually looking for.
 */
function groupRuns(items: Item[]): Item[] {
  const out: Item[] = []
  for (const item of items) {
    const prev = out.length > 0 ? out[out.length - 1] : undefined
    const foldable =
      item.kind === 'step' &&
      prev?.kind === 'step' &&
      prev.status !== 'running' &&
      item.status !== 'running' &&
      item.status === prev.status &&
      item.verb === prev.verb

    if (!foldable || !prev || item.kind !== 'step' || prev.kind !== 'step') {
      out.push(item)
      continue
    }

    // The container row's own entry is recorded once, on the first merge.
    // Recording it again on every later merge would push the already-rewritten
    // "Writing ×7" title into the list and count the container as its own
    // member.
    const folded = prev.folded ?? [{ title: prev.title, detail: prev.detail, at: prev.at }]
    folded.push({ title: item.title, detail: item.detail, at: item.at })
    prev.folded = folded
    prev.repeats = folded.length
    prev.title = `${item.verb} ×${folded.length}`
    prev.detail = summariseTitles(folded)
    prev.at = item.at
  }
  return out
}

/** summariseTitles lists what a folded row covered without becoming a wall. */
function summariseTitles(rows: { title: string }[]): string {
  const names = rows.map((r) => r.title.replace(/^\S+ /, '')).filter(Boolean)
  if (names.length <= 3) return names.join(', ')
  return `${names.slice(0, 3).join(', ')} and ${names.length - 3} more`
}

/**
 * localSentence is the fallback status line, computed in the browser.
 *
 * It duplicates the server's narrator on purpose: the narrator is optional and
 * model-backed, this is free and instant, and between them the status line is
 * never empty.
 */
export function localSentence(s: Summary): string {
  switch (s.phase) {
    case 'failed':
      return s.error || 'The run failed before the agent reported a reason.'
    case 'done':
      return s.files > 0
        ? `Done. ${s.files} file${s.files === 1 ? '' : 's'} in the workspace.`
        : 'Done. Nothing was written.'
    case 'starting':
      return s.harness ? `Starting ${s.harness} in a confined workspace.` : 'Starting the agent.'
    case 'idle':
      return s.files > 0
        ? `Idle. ${s.files} file${s.files === 1 ? '' : 's'} in the last workspace.`
        : 'Idle. Describe what you want built.'
    default:
      return s.step || (s.tools > 0 ? `Working · ${s.tools} tool call${s.tools === 1 ? '' : 's'}` : 'Thinking it through.')
  }
}
