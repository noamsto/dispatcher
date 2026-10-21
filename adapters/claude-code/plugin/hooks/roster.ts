import type { RosterRow } from '../types'

const ACTIVE = new Set(['working', 'blocked'])

export const isWatchdog = (row: RosterRow): boolean =>
  row.state === 'blocked' && row.source === 'watchdog'

export const sortRows = (rows: RosterRow[]): RosterRow[] =>
  rows
    .map((row, index) => ({ row, index }))
    .sort(
      (a, b) =>
        Number(ACTIVE.has(b.row.state)) - Number(ACTIVE.has(a.row.state)) ||
        a.row.age_s - b.row.age_s ||
        a.index - b.index,
    )
    .map(({ row }) => row)

export const truncate = (text: string, max: number): string =>
  text.length > max ? `${text.slice(0, max - 1)}…` : text

export const ageLabel = (seconds: number): string => {
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}

export const profile = (row: RosterRow): string =>
  [row.tier, row.engine, row.model].map(part => part ?? '?').join('·')

export const parseRoster = (stdout: string): RosterRow[] => {
  if (!stdout.trim()) return []
  const rows: unknown = JSON.parse(stdout)
  if (!Array.isArray(rows)) throw new Error('crew roster did not print a JSON array')
  return (rows as RosterRow[]).map(row => ({
    ...row,
    age_s: Number.isFinite(row.age_s) ? row.age_s : 0,
    detail: typeof row.detail === 'string' ? row.detail : null,
    repo: typeof row.repo === 'string' ? row.repo : undefined,
  }))
}

// The repo to name on a row, from `crew fleet`'s `owner/name` label. A bare
// name (a repo with no parseable origin) has no slash and is its own basename.
export const repoName = (row: RosterRow): string | null =>
  row.repo ? row.repo.slice(row.repo.lastIndexOf('/') + 1) || row.repo : null

// Whether a section's rows come from more than one repo — the only case where
// naming the repo on each row says anything. Distinct on the full `owner/name`,
// so two owners of a same-named repo still count as two.
export const spansRepos = (rows: RosterRow[]): boolean =>
  new Set(rows.map(row => row.repo).filter((repo): repo is string => !!repo)).size > 1

// The id grammar `crew register`/`crew adopt` enforce. A `crew` older than
// `--mine` prints the whole `crew crews` table, and without this its `crew_id`
// header would become a crew to poll.
const isCrewId = (id: string): boolean =>
  /^[A-Za-z0-9._-]+$/.test(id) && !id.startsWith('-') && id !== '.' && id !== '..'

// `crew crews --mine` — one bare crew id per line, no header.
export const parseCrewIds = (stdout: string): string[] => {
  const ids: string[] = []
  for (const line of stdout.split('\n')) {
    const id = line.trim()
    if (isCrewId(id) && !ids.includes(id)) ids.push(id)
  }
  return ids
}
