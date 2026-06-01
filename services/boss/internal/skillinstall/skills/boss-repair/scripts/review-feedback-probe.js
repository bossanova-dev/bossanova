#!/usr/bin/env node

const { execFileSync } = require('child_process');

function runGh(args) {
  const out = execFileSync('gh', args, {
    encoding: 'utf8',
    maxBuffer: 20 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  if (!out.trim()) {
    throw new Error(`gh ${args.join(' ')} produced empty stdout`);
  }

  return out;
}

function compact(value, max = 360) {
  return String(value || '')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, max);
}

function main() {
  const prView = JSON.parse(runGh(['pr', 'view', '--json', 'number,latestReviews']));
  const repoView = JSON.parse(runGh(['repo', 'view', '--json', 'owner,name']));
  const owner = repoView.owner.login;
  const name = repoView.name;
  const repo = `${owner}/${name}`;
  const pr = prView.number;

  const comments = JSON.parse(runGh(['api', `repos/${repo}/pulls/${pr}/comments`, '--paginate']));

  const query = `
    query($owner: String!, $name: String!, $number: Int!) {
      repository(owner: $owner, name: $name) {
        pullRequest(number: $number) {
          reviewThreads(first: 100) {
            nodes {
              id
              isResolved
              comments(first: 20) {
                nodes {
                  databaseId
                  body
                  path
                  line
                  author { login }
                  url
                }
              }
            }
          }
        }
      }
    }`;

  const graph = JSON.parse(
    runGh([
      'api',
      'graphql',
      '-f',
      `owner=${owner}`,
      '-f',
      `name=${name}`,
      '-F',
      `number=${pr}`,
      '-f',
      `query=${query}`,
    ]),
  );

  const threads = graph?.data?.repository?.pullRequest?.reviewThreads?.nodes || [];
  const unresolved = threads.filter((thread) => !thread.isResolved);
  const latestCommented = (prView.latestReviews || []).some(
    (review) => review.state === 'COMMENTED',
  );
  const suspiciousZero = latestCommented && comments.length === 0 && threads.length === 0;

  console.log(`repo=${repo} pr=${pr}`);
  console.log(`inline_comments=${comments.length}`);
  console.log(`review_threads=${threads.length} unresolved_threads=${unresolved.length}`);
  console.log(`latest_review_commented=${latestCommented}`);
  console.log(`probe_status=${suspiciousZero ? 'suspicious_zero' : 'ok'}`);

  if (suspiciousZero) {
    console.log(
      'ERROR latestReviews contains COMMENTED, but REST and GraphQL found zero comments. Retry with explicit repo/pr before concluding no feedback exists.',
    );
    process.exit(2);
  }

  if (unresolved.length > 0) {
    console.log('');
    console.log('UNRESOLVED_THREADS');
    unresolved.forEach((thread, index) => {
      const first = thread.comments.nodes[0] || {};
      const last = thread.comments.nodes[thread.comments.nodes.length - 1] || first;
      console.log(
        `#${index + 1} thread=${thread.id} comment_id=${first.databaseId || ''} path=${first.path || last.path || ''} line=${first.line || last.line || ''}`,
      );
      console.log(`author=${first.author?.login || last.author?.login || ''}`);
      console.log(`url=${first.url || last.url || ''}`);
      console.log(`body=${compact(first.body || last.body)}`);
    });
    return;
  }

  if (comments.length > 0) {
    console.log('');
    console.log('INLINE_COMMENTS_NO_UNRESOLVED_THREAD_STATE');
    comments.forEach((comment, index) => {
      console.log(
        `#${index + 1} comment_id=${comment.id} reply_to=${comment.in_reply_to_id || ''} path=${comment.path || ''} line=${comment.line || comment.original_line || ''}`,
      );
      console.log(`author=${comment.user?.login || ''}`);
      console.log(`url=${comment.html_url || ''}`);
      console.log(`body=${compact(comment.body)}`);
    });
  }
}

try {
  main();
} catch (error) {
  console.log('probe_status=failed');
  console.log(`ERROR ${error.message}`);
  process.exit(1);
}
