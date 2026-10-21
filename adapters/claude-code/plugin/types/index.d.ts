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
  // The repo this row's bus came from, as `owner/name`. Only `crew fleet` fills
  // it; a `crew roster` row has none.
  repo?: string
}

export type RosterSection = {
  crew: string
  rows: RosterRow[]
  error: string | null
}

export type RosterView = {
  // One section per crew this session follows (#824).
  sections: RosterSection[]
  // An explicit `/roster <crew-id>`: follow only that crew, never scan.
  pinned: string | null
  // The last ownership scan's failure — kept while its sections stay on screen.
  scanError: string | null
  crews: string | null
}

declare module 'claude-code' {
  interface PluginState {
    dispatcher: { roster: RosterView }
  }
}
