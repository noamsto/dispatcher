import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { RosterRow, RosterView } from '../types'
import {
  ageLabel,
  isWatchdog,
  parseRoster,
  profile,
  sortRows,
  truncate,
} from './roster'

const PANE = 'crew-roster'
const REFRESH_MS = 12_000
const DETAIL_MAX = 48

const view = atom(
  { plugin: 'dispatcher', key: 'roster' } as const,
  { crew: null, rows: [], error: null, crews: null } as RosterView,
)

let timer: Timer | undefined

async function refresh($: EngineInterface) {
  try {
    const crew = (await read($, view)).crew
    if (!crew) return
    let rows: RosterRow[] = []
    let error: string | null = null
    try {
      const ran = await $.process.run(['crew', 'roster', crew], { timeoutMs: 10_000 })
      if (ran.exitCode !== 0) {
        throw new Error(ran.stderr.trim() || `crew roster exited ${ran.exitCode}`)
      }
      rows = parseRoster(ran.stdout)
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    }
    await update($, view, v =>
      v.crew !== crew ? v : { ...v, rows: error ? v.rows : rows, error },
    )
  } catch {
    // a failing state call must not escape the timer callback
  }
}

async function follow($: EngineInterface, crew: string) {
  timer?.cancel()
  timer = $.clock.every(REFRESH_MS, () => void refresh($))
  await update($, view, () => ({ crew, rows: [], error: null, crews: null }))
  await refresh($)
}

async function listCrews($: EngineInterface) {
  timer?.cancel()
  timer = undefined
  let crews = 'No crews found. Run /roster <crew-id>.'
  try {
    const ran = await $.process.run(['crew', 'crews'], { timeoutMs: 10_000 })
    const body = ran.stdout.trim().split('\n').slice(1).join('\n')
    if (ran.exitCode !== 0) crews = `crew crews failed: ${ran.stderr.trim() || ran.exitCode}`
    else if (body) crews = ran.stdout.trim()
  } catch (err) {
    crews = `crew crews failed: ${err instanceof Error ? err.message : String(err)}`
  }
  await update($, view, v =>
    v.crew ? v : { crew: null, rows: [], error: null, crews },
  )
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    if (await $.env.get('CREW_WORKER_ID')) return next(e)

    await $.command.register({
      name: 'roster',
      description: 'Show the live crew roster in a pane',
      argumentHint: '[crew-id]',
    })
    const crew = await $.env.get('CREW_ID')
    if (crew) {
      follow($, crew).catch(() => {})
      $.ui.open({ id: PANE, title: 'Crew roster' }).catch(() => {})
    }

    return next(e)
  })

  on('command.run', { command: 'roster' }, async ($, e) => {
    const crew = e.args.trim() || (await $.env.get('CREW_ID'))
    if (crew) {
      await follow($, crew)
    } else {
      await listCrews($)
    }
    await $.ui.open({ id: PANE, title: 'Crew roster' })

    return { text: 'Crew roster pane opened.' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Link } = $.ui.resolve(e)
    const { crew, rows, error, crews } = await read($, view)

    if (!crew) {
      return (
        <Box flexDirection="column">
          <Text>No crew selected. Run /roster &lt;crew-id&gt;.</Text>
          {(crews ?? '').split('\n').map(line => (
            <Text key={line} dimColor>{line}</Text>
          ))}
        </Box>
      )
    }

    return (
      <Box flexDirection="column">
        <Text dimColor>crew {crew}</Text>
        {error && <Text color="red">crew roster failed: {error}</Text>}
        {!error && rows.length === 0 && <Text dimColor>No workers yet.</Text>}
        {sortRows(rows).map(row => (
          <Box key={row.name} flexDirection="column">
            <Text>
              <Text color={row.color ?? undefined} bold>
                {row.name}
              </Text>{' '}
              {row.title ?? row.branch}
            </Text>
            <Text dimColor>
              {'  '}
              {profile(row)} ·{' '}
              <Text
                color={isWatchdog(row) ? 'yellow' : undefined}
                bold={isWatchdog(row)}
              >
                {isWatchdog(row) ? 'blocked (watchdog)' : row.state}
              </Text>
              {row.detail ? ` · ${truncate(row.detail, DETAIL_MAX)}` : ''} ·{' '}
              {ageLabel(row.age_s)}
              {row.pr_url ? ' · ' : ''}
              {row.pr_url && <Link href={row.pr_url} label="PR" />}
            </Text>
          </Box>
        ))}
      </Box>
    )
  })
}
