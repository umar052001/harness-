import { createEffect, createSignal, onCleanup, Show } from 'solid-js'

interface Status { status: string; host?: string; port?: string }

export default function Database(props: { runId: string | null; running: boolean }) {
  const [status, setStatus] = createSignal<Status>()
  const [busy, setBusy] = createSignal(false)
  const [error, setError] = createSignal('')
  let generation = 0
  onCleanup(() => { generation++ })

  createEffect(() => {
    const id = props.runId
    const request = ++generation
    setStatus(undefined); setError(''); setBusy(false)
    if (!id) return
    void fetch(`/api/workspace/${encodeURIComponent(id)}/database`, { signal: AbortSignal.timeout(10000) })
      .then(async res => {
        const body = await res.json()
        if (request !== generation) return
        if (!res.ok) throw new Error(body.error || 'Could not read database status')
        setStatus(body)
      })
      .catch(e => { if (request === generation) setError(e.message) })
  })

  async function change(method: 'POST' | 'DELETE') {
    const id = props.runId
    if (!id || busy()) return
    const request = generation
    setBusy(true); setError('')
    try {
      const res = await fetch(`/api/workspace/${encodeURIComponent(id)}/database`, { method, signal: AbortSignal.timeout(190000) })
      const body = await res.json()
      if (request !== generation) return
      if (!res.ok) throw new Error(body.error || 'Database operation failed')
      setStatus(body)
    } catch (e) { if (request === generation) setError((e as Error).message) }
    finally { if (request === generation) setBusy(false) }
  }

  return (
    <details class="usage-panel">
      <summary>Project database · {busy() ? 'Working…' : status()?.status.replaceAll('_', ' ') || (error() ? 'Unavailable' : 'Checking')}</summary>
      <p>Rootless PostgreSQL, shared by attempts in this project. Stopping retains its data. The next continuation receives DATABASE_URL automatically.</p>
      <Show when={status()?.port}><p>Listening on {status()?.host}:{status()?.port}</p></Show>
      <button class="quiet-button" disabled={busy() || props.running} onClick={() => void change(status()?.status === 'running' ? 'DELETE' : 'POST')}>
        {busy() ? 'Working…' : status()?.status === 'running' ? 'Stop database' : 'Start PostgreSQL'}
      </button>
      <Show when={error()}><p role="alert">{error()}</p></Show>
    </details>
  )
}
