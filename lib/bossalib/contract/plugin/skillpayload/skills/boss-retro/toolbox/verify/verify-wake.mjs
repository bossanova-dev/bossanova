export function wakePlan({ verdict, reason, prView }) {
  let waitingOn = 'ci'
  if (prView?.mergeStateStatus === 'DIRTY') waitingOn = 'conflict'
  else if (verdict === 'defect') waitingOn = 'findings'
  else if (verdict === 'blocked') waitingOn = 'blockers'
  else if (reason === 'pr-draft') waitingOn = 'draft'
  const owner = ['conflict', 'findings', 'blockers'].includes(waitingOn)
    ? 'repair'
    : waitingOn === 'draft'
      ? 'person'
      : 'verify'
  return {
    owner,
    waitingOn,
    trigger:
      prView?.isDraft === true && waitingOn !== 'draft' ? 'checks_passed' : 'checks_passed_ready',
    onTransition: true,
  }
}

export function rearmArgv({ pr, chat, repo, trigger = 'checks_passed_ready' }) {
  return [
    'callback',
    'add',
    String(pr),
    trigger,
    '--on-transition',
    '--chat',
    chat,
    ...(typeof repo === 'string' && repo.trim() ? ['--repo', repo] : []),
    '--message',
    `/boss-verify ${pr}`,
    '--json',
  ]
}
