#!/usr/bin/env node

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');
const slowTestFiles = [
  'services/boss/internal/tuitest/cron_test.go',
  'services/boss/internal/tuitest/newsession_test.go',
  'services/boss/internal/tuitest/navigation_test.go',
  'services/boss/internal/tuitest/login_subscription_test.go',
  'services/bossd/internal/session/tmux_chat_test.go',
  'services/bossd/internal/status/tmux_poller_test.go',
  'services/bossd/internal/tmux/tmux_test.go',
  'services/bossd/internal/testharness/e2e_record_chat_tmux_test.go',
];

test('slow tmux and TUI test files explicitly support go test -short', () => {
  for (const file of slowTestFiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8');
    assert.match(text, /testing\.Short\(\)/, file);
    assert.match(text, /run .*full|make test-|go test/i, file);
  }
});
