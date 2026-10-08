import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

const START = { cwd: '/repo', surface: 'terminal', isInteractive: true } as const
const PANE = {
  title: 'Crew roster',
  isFocused: false,
  bodyColumns: 100,
  placement: 'dock',
  scroll: { offset: 0, bodyRows: 40 },
  view: {},
} as const

const ROWS = [
  {
    name: 'mauve', color: 'palevioletred', branch: 'feat/a', state: 'done',
    detail: null, source: null, age_s: 5, title: 'Finished task', tier: 'trivial',
    engine: 'claude', model: 'haiku', pr_url: 'https://example.test/pr/1',
  },
  {
    name: 'plum', color: 'plum', branch: 'feat/b', state: 'blocked',
    detail: 'prompt: waiting on a permission dialog that is very very long indeed',
    source: 'watchdog', age_s: 130, title: 'Stuck task', tier: 'standard',
    engine: 'claude', model: 'sonnet', pr_url: null,
  },
  {
    name: 'teal', color: 'teal', branch: 'feat/c', state: 'working',
    detail: null, source: null, age_s: 9000, title: 'Busy task', tier: 'deep',
    engine: 'codex', model: 'gpt', pr_url: null,
  },
]

const done = (exitCode: number, stdout: string, stderr = '') => ({
  value: { exitCode, stdout, stderr, isStdoutTruncated: false, isStderrTruncated: false },
})
const ok = (stdout: string) => done(0, stdout)

function stubEngine(on: On) {
  const calls: string[] = []
  on('session.start', async (_$, e) => ({ cwd: e.cwd }))
  on('command.register', async (_$, e) => {
    calls.push(`command ${e.name}`)
    return { value: { command: e.name } }
  })
  on('ui.open', async (_$, e) => {
    calls.push(`open ${e.id}`)
    return { value: { isPlaced: true } }
  })
  return calls
}

test('a worker session registers nothing and runs no process', async ($, on) => {
  mock.env(on, { CREW_WORKER_ID: 'worker:x', CREW_ID: 'c1' })
  const calls = stubEngine(on)
  mock.clock(on)
  const ran: string[][] = []
  on('process.run', async (_$, e) => {
    ran.push([...e.argv])
    return ok('[]')
  })
  await $.session.start(START)
  expect(ran).toEqual([])
  expect(calls).toEqual([])
})

test('rows render: active first, watchdog distinct, detail truncated, PR linked', async ($, on) => {
  mock.env(on, { CREW_ID: 'c1' })
  stubEngine(on)
  const clock = mock.clock(on)
  on('process.run', async () => ok(JSON.stringify(ROWS)))
  await $.session.start(START)
  await clock.settle()
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  const names = (await ui.findAll({ type: 'Text', text: /^(plum|teal|mauve)$/ })).map(x => x.text)
  expect(names).toEqual(['plum', 'teal', 'mauve'])
  expect(await ui.find({ text: /blocked \(watchdog\)/ })).toBeDefined()
  expect(await ui.find({ text: /standard·claude·sonnet/ })).toBeDefined()
  expect(await ui.find({ text: /waiting on a permission dialog.*…/ })).toBeDefined()
  expect(await ui.find({ type: 'Link' })).toBeDefined()
})

test('a failing crew call shows an error line', async ($, on) => {
  mock.env(on, { CREW_ID: 'c1' })
  stubEngine(on)
  const clock = mock.clock(on)
  on('process.run', async () => done(2, '', 'boom'))
  await $.session.start(START)
  await clock.advance(13_000)
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  expect(await ui.find({ text: /crew roster failed: boom/ })).toBeDefined()
})

test('the roster refreshes on the timer and treats empty output as no workers', async ($, on) => {
  mock.env(on, { CREW_ID: 'c1' })
  stubEngine(on)
  const clock = mock.clock(on)
  const argv: string[][] = []
  on('process.run', async (_$, e) => {
    argv.push([...e.argv])
    return ok('')
  })
  await $.session.start(START)
  await clock.advance(25_000)
  // Every tick is one ownership scan plus one roster call per followed crew.
  const rosters = argv.filter(call => call[1] === 'roster')
  expect(rosters.length).toBe(3)
  expect(rosters[0]).toEqual(['crew', 'roster', 'c1'])
  expect(
    argv.filter(call => call[1] === 'crews' && call[2] === '--mine').length,
  ).toBe(3)
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  expect(await ui.find({ text: /No workers yet/ })).toBeDefined()
})

test('the pane follows every crew this session owns, not only $CREW_ID', async ($, on) => {
  mock.env(on, { CREW_ID: 'A' })
  stubEngine(on)
  const clock = mock.clock(on)
  const argv: string[][] = []
  on('process.run', async (_$, e) => {
    argv.push([...e.argv])
    if (e.argv[2] === '--mine') return ok('A\nB\n')
    return ok(JSON.stringify(e.argv[2] === 'A' ? [ROWS[0]] : [ROWS[2]]))
  })
  await $.session.start(START)
  await clock.settle()
  expect(argv).toContainEqual(['crew', 'crews', '--mine'])
  expect(argv).toContainEqual(['crew', 'roster', 'A'])
  expect(argv).toContainEqual(['crew', 'roster', 'B'])
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  const headers = (await ui.findAll({ type: 'Text', text: /^crew [AB]$/ })).map(x => x.text)
  expect(headers).toEqual(['crew A', 'crew B'])
  const names = (await ui.findAll({ type: 'Text', text: /^(mauve|teal)$/ })).map(x => x.text)
  expect(names).toEqual(['mauve', 'teal'])
})

test('a failing ownership scan keeps the rows and shows an error line', async ($, on) => {
  mock.env(on, { CREW_ID: 'c1' })
  stubEngine(on)
  const clock = mock.clock(on)
  on('process.run', async (_$, e) =>
    e.argv[2] === '--mine' ? done(2, '', 'scan boom') : ok(JSON.stringify(ROWS)),
  )
  await $.session.start(START)
  await clock.settle()
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  expect(await ui.find({ text: /crew crews --mine failed: scan boom/ })).toBeDefined()
  expect(await ui.find({ text: /blocked \(watchdog\)/ })).toBeDefined()
})

test('/roster <crew-id> pins that one crew and stops scanning', async ($, on) => {
  mock.env(on, { CREW_ID: 'A' })
  stubEngine(on)
  const clock = mock.clock(on)
  const argv: string[][] = []
  on('process.run', async (_$, e) => {
    argv.push([...e.argv])
    if (e.argv[2] === '--mine') return ok('A\nB\n')
    return ok('')
  })
  await $.session.start(START)
  await clock.settle()
  await $.command.run({
    command: 'roster',
    args: 'C',
    origin: { kind: 'composer' },
    presentation: { isFullscreen: false, columns: 100 },
  })
  // The pin's own refresh already ran; a tick later polls only crew C, and the
  // ownership scan never runs again while pinned.
  argv.length = 0
  await clock.advance(13_000)
  expect(argv).toEqual([['crew', 'roster', 'C']])
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  const headers = (await ui.findAll({ type: 'Text', text: /^crew [ABC]$/ })).map(x => x.text)
  expect(headers).toEqual(['crew C'])
})

test('/roster with no crew lists crews, or hints when only the header exists', async ($, on) => {
  mock.env(on, {})
  stubEngine(on)
  mock.clock(on)
  on('process.run', async () => ok('crew_id\tlast_event_s\n'))
  await $.session.start(START)
  await $.command.run({
    command: 'roster',
    args: '',
    origin: { kind: 'composer' },
    presentation: { isFullscreen: false, columns: 100 },
  })
  const ui = await $.ui.mount({
    plugin: 'dispatcher', surface: 'terminal', component: 'Pane',
    props: PANE, requestId: 'crew-roster',
  })
  expect(await ui.find({ text: /No crews found/ })).toBeDefined()
})
