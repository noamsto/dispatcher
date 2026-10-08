import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { RosterSection, RosterView } from '../types'
import {
  ageLabel,
  isWatchdog,
  parseCrewIds,
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
  { sections: [], pinned: null, crews: null } as RosterView,
)

let timer: Timer | undefined

// The id of the newest refresh. Ticks fire every REFRESH_MS and a `crew` call
// can take its full 10s timeout, so an older tick can finish after a newer one
// and paint stale rows over fresh; a refresh writes the view only while it is
// still the newest.
let refreshId = 0

const message = (err: unknown): string =>
  err instanceof Error ? err.message : String(err)

async function fetchSection(
  $: EngineInterface,
  crew: string,
  previous: RosterSection[],
): Promise<RosterSection> {
  try {
    const ran = await $.process.run(['crew', 'roster', crew], { timeoutMs: 10_000 })
    if (ran.exitCode !== 0) {
      throw new Error(ran.stderr.trim() || `crew roster exited ${ran.exitCode}`)
    }
    return { crew, rows: parseRoster(ran.stdout), error: null }
  } catch (err) {
    // One crew failing is that section's error line, not the pane's: the other
    // crews still poll, and this one keeps the rows it had.
    return {
      crew,
      rows: previous.find(section => section.crew === crew)?.rows ?? [],
      error: message(err),
    }
  }
}

// Auto mode's follow list: $CREW_ID plus every other crew this process owns —
// the ones `crew adopt` re-attached to it after a restart, which is where the
// workers still post (#824). Null when the scan itself failed (a `crew` older
// than `--mine`, no repo): the caller then keeps what it was showing instead of
// emptying the pane.
async function ownedCrews($: EngineInterface): Promise<string[] | null> {
  const own = await $.env.get('CREW_ID')
  try {
    const ran = await $.process.run(['crew', 'crews', '--mine'], { timeoutMs: 10_000 })
    if (ran.exitCode !== 0) return null
    const crews = parseCrewIds(ran.stdout)
    return [...new Set(own ? [own, ...crews] : crews)]
  } catch {
    return null
  }
}

async function paint($: EngineInterface, crews: string[], id: number) {
  const previous = (await read($, view)).sections
  const sections = await Promise.all(
    crews.map(crew => fetchSection($, crew, previous)),
  )
  await update($, view, v => (refreshId !== id ? v : { ...v, sections }))
}

async function refresh($: EngineInterface) {
  const id = ++refreshId
  try {
    const current = await read($, view)
    if (current.pinned) {
      await paint($, [current.pinned], id)
      return
    }
    // Nothing followed: the pane is showing the `crew crews` list.
    if (current.sections.length === 0) return
    const crews = (await ownedCrews($)) ?? current.sections.map(s => s.crew)
    if (crews.length === 0) return
    await paint($, crews, id)
  } catch {
    // a failing state call must not escape the timer callback
  }
}

async function follow($: EngineInterface, crews: string[], pinned: string | null) {
  refreshId++
  timer?.cancel()
  timer = $.clock.every(REFRESH_MS, () => void refresh($))
  await update($, view, () => ({
    sections: crews.map(crew => ({ crew, rows: [], error: null })),
    pinned,
    crews: null,
  }))
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
    crews = `crew crews failed: ${message(err)}`
  }
  await update($, view, v =>
    v.sections.length ? v : { sections: [], pinned: null, crews },
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
      follow($, [crew], null).catch(() => {})
      $.ui.open({ id: PANE, title: 'Crew roster' }).catch(() => {})
    }

    return next(e)
  })

  on('command.run', { command: 'roster' }, async ($, e) => {
    const arg = e.args.trim()
    const own = await $.env.get('CREW_ID')
    if (arg) {
      await follow($, [arg], arg)
    } else if (own) {
      await follow($, [own], null)
    } else {
      await listCrews($)
    }
    await $.ui.open({ id: PANE, title: 'Crew roster' })

    return { text: 'Crew roster pane opened.' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Link } = $.ui.resolve(e)
    const { sections, crews } = await read($, view)

    if (sections.length === 0) {
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
        {sections.map(section => (
          <Box key={section.crew} flexDirection="column">
            <Text dimColor>crew {section.crew}</Text>
            {section.error && (
              <Text color="red">crew roster failed: {section.error}</Text>
            )}
            {!section.error && section.rows.length === 0 && (
              <Text dimColor>No workers yet.</Text>
            )}
            {sortRows(section.rows).map(row => (
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
        ))}
      </Box>
    )
  })
}
