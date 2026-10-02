export interface Harness {
  id: string
  name: string
  kind: string
  cost: string
  license?: string
  notes?: string
  installed: boolean
  command: string
}

export interface Health {
  ok: boolean
  sandboxd: string
  sandboxdOk: boolean
  plannerOk?: boolean
  artifactsOk?: boolean
  artifactsError?: string
  workspaces?: string
  activeRuns?: number
  maxRuns?: number
}

/** ActiveRun is a run that is happening right now, not one that already happened. */
export interface ActiveRun {
  runId: string
  harnessId: string
  prompt: string
  startedAt: string
  workspace?: string
}

export interface RunEvent {
  kind: string
  seq?: number
  at: string
  text?: string
  data?: Record<string, unknown>
  /** run.assigned carries these at the top level; everything else uses `data`. */
  runId?: string
  workspace?: string
}

export interface RunRequest {
  harnessId: string
  prompt: string
  models?: string[]
  autoApprove?: boolean
  timeoutSec?: number
}

export interface RunResult {
  ok: boolean
  error?: string
}

export interface WorkspaceFile {
  path: string
  kind: string
  size: number
  modTime: string
}

export interface Listing {
  id: string
  dir: string
  files: WorkspaceFile[]
  preview: string
  total: number
  truncated: boolean
  bytes: number
}

export interface RunSummary {
  id: string
  dir: string
  modTime: string
  files: number
}

export interface NarrateStatus {
  phase: string
  harness?: string
  step?: string
  tools?: number
  files?: number
  elapsedSec?: number
  lastMessage?: string
  error?: string
}

export interface Narration {
  narration: string
  source: string
  model: boolean
}
