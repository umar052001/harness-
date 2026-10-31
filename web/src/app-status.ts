export interface AppStatus {
  status: string
  active: boolean
  verification: string
  checksPassed: number
  checksTotal: number
  error?: string
  url?: string
}

export function appStatusLabel(app?: AppStatus): string {
  if (!app) return 'Runtime status unavailable · reconnecting…'
  if (app.status === 'stopped') return 'App stopped · code generation does not start services'
  if (app.status === 'failed') return `App failed · ${app.error || 'check startup or verification'}`
  if (app.status === 'ready') return app.verification === 'passed'
    ? `App running · ${app.checksPassed}/${app.checksTotal} project checks passed`
    : 'App running · functional checks not supplied'
  if (app.status === 'checking') return `Checking app · ${app.checksPassed}/${app.checksTotal} passed`
  return `${app.status === 'building' ? 'Building' : 'Starting'} app services…`
}
