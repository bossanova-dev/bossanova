#!/usr/bin/env node
import 'reflect-metadata';
import { Text, render } from 'ink';
import React from 'react';
import { container, setupContainer } from './di/container.js';
import { Service } from './di/tokens.js';
import { parseArgs, resolveRoute, type Route } from './router.js';
import { HomeScreen, SessionList } from './views/HomeScreen.js';
import type { IpcClient } from '@bossanova/shared';

// --- Stub views (replaced in subsequent tasks) ---

function HelpView() {
  return (
    <Text>
{`boss — Bossanova CLI

Usage:
  boss                    Interactive home screen
  boss new [plan]         Start a new session
  boss ls                 List sessions
  boss attach <id>        Attach to a session
  boss stop <id>          Stop a session
  boss pause <id>         Pause a session
  boss resume <id>        Resume a session
  boss logs <id>          View session logs
  boss retry <id>         Retry a session
  boss close <id>         Close a session
  boss rm <id>            Remove a session
  boss repo add           Register a repository
  boss repo ls            List repositories
  boss repo remove <id>   Remove a repository
  boss help               Show this help`}
    </Text>
  );
}

function ErrorView({ message }: { message: string }) {
  return <Text color="red">{message}</Text>;
}

function StubView({ label }: { label: string }) {
  return <Text dimColor>[{label}] — not yet implemented</Text>;
}

// --- App component ---

export function App({ route, client }: { route: Route; client: IpcClient }) {
  switch (route.view) {
    case 'help':
      return <HelpView />;
    case 'error':
      return <ErrorView message={route.message} />;
    case 'home':
      return (
        <HomeScreen
          client={client}
          onNewSession={() => {/* TODO: navigate to new session */}}
          onAddRepo={() => {/* TODO: navigate to add repo */}}
          onAttach={() => {/* TODO: navigate to attach */}}
        />
      );
    case 'new':
      return <StubView label={`new session${route.plan ? `: ${route.plan}` : ''}`} />;
    case 'ls':
      return <SessionList client={client} />;
    case 'attach':
      return <StubView label={`attach ${route.sessionId}`} />;
    case 'session-action':
      return <StubView label={`${route.action} ${route.sessionId}`} />;
    case 'repo-add':
      return <StubView label="repo add" />;
    case 'repo-ls':
      return <StubView label="repo list" />;
    case 'repo-remove':
      return <StubView label={`repo remove ${route.repoId}`} />;
  }
}

// --- Bootstrap ---

const parsed = parseArgs(process.argv);
const route = resolveRoute(parsed);

setupContainer();

const client = container.resolve<IpcClient>(Service.IpcClient);

render(<App route={route} client={client} />);
