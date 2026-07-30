export const meta = {
  name: 'spec-plan-critic',
  description: 'Spec then plan, each gated by an adversarial critic with a 2-revision cap',
  phases: [
    { title: 'Spec' },
    { title: 'Spec critique' },
    { title: 'Plan' },
    { title: 'Plan critique' },
  ],
}

const VERDICT = {
  type: 'object',
  required: ['verdict', 'blocking', 'summary'],
  properties: {
    verdict: { enum: ['accept', 'revise', 'reject'] },
    blocking: {
      type: 'array',
      items: {
        type: 'object',
        required: ['issue', 'why', 'fix'],
        properties: { issue: { type: 'string' }, why: { type: 'string' }, fix: { type: 'string' } },
      },
    },
    nonblocking: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
  },
}

const MAX_REVS = 2
const { tier, task, repoPath } = args

// Returns { text, escalation } — escalation set when the critic never accepted.
async function gated(makePrompt, critic, phase, criticPhase, label) {
  let draft = await agent(makePrompt(null), { phase, label: `${label}:draft` })
  for (let rev = 1; rev <= MAX_REVS; rev++) {
    const v = await agent(
      `Adversarially review this ${label}. Repo: ${repoPath}.\n\n${draft}`,
      { phase: criticPhase, label: `${critic}:rev${rev}`, schema: VERDICT, agentType: critic },
    )
    if (!v || v.verdict === 'accept') return { text: draft, escalation: null }
    if (v.verdict === 'reject')
      return { text: draft, escalation: { stage: label, reason: 'critic rejected', detail: v } }
    log(`${label} rev ${rev}: ${v.blocking.length} blocking`)
    draft = await agent(
      `Revise this ${label} to resolve the blocking findings.\n\nFINDINGS:\n${JSON.stringify(v.blocking, null, 2)}\n\nCURRENT:\n${draft}`,
      { phase, label: `${label}:rev${rev}` },
    )
  }
  return { text: draft, escalation: { stage: label, reason: `unresolved after ${MAX_REVS} revs` } }
}

const escalations = []

// deep tier specs; standard/trivial skip straight to (or past) planning.
let spec = null
if (tier === 'deep') {
  const r = await gated(
    () => `Write a spec for this task using brainstorming discipline.\n\nTASK:\n${task}`,
    'spec-critic', 'Spec', 'Spec critique', 'spec',
  )
  spec = r.text
  if (r.escalation) escalations.push(r.escalation)
}

// standard + deep get a plan; trivial returns immediately (handled by the worker, not here).
const planInput = spec ? `SPEC:\n${spec}` : `TASK:\n${task}`
const pr = await gated(
  () => `Write a bite-sized implementation plan (writing-plans discipline).\n\n${planInput}`,
  'plan-critic', 'Plan', 'Plan critique', 'plan',
)
if (pr.escalation) escalations.push(pr.escalation)

return { spec, plan: pr.text, escalations }
