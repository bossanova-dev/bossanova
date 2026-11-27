package session

// ReviewAppURLEnv names the review app that opens a PR by its
// /review/<owner>/<name>/<pr> route. bossd sets it in its own environment from
// its orchestrator, so cron gates inherit it and managed chats are given it;
// verify's pages link stacked PRs there.
const ReviewAppURLEnv = "BOSS_REVIEW_APP_URL"
