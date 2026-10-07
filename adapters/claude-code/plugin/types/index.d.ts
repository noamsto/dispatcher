export type RosterRow = {
  name: string
  color: string | null
  branch: string
  state: string
  detail: string | null
  source: string | null
  age_s: number
  title: string | null
  tier: string | null
  engine: string | null
  model: string | null
  pr_url: string | null
}

export type RosterView = {
  crew: string | null
  rows: RosterRow[]
  error: string | null
  crews: string | null
}

declare module 'claude-code' {
  interface PluginState {
    dispatcher: { roster: RosterView }
  }
}
