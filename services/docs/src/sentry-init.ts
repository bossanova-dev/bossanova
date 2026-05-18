// biome-ignore lint/performance/noNamespaceImport: Sentry SDK namespace import required by integration spec.
import * as Sentry from '@sentry/react';

const dsn =
  // biome-ignore lint/security/noSecrets: Public Sentry DSN required by deployment spec.
  'https://f2047aedfb788b237eaa08d0a692fc3d@o4511396716871680.ingest.de.sentry.io/4511396756062288';
const redacted = '[REDACTED]';
const messageCap = 2000;
const truncMarker = '...[truncated]';

const reGitHubToken = /(?:github_pat_[A-Za-z0-9_]{20,}|(?:ghs|gho|ghp|ghu|ghr)_[A-Za-z0-9]{30,})/gi;
const reJwt = /eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{20,}/g;
// reAuthHeader matches "Authorization: <scheme> <value>" for single-token
// schemes (Basic, Bearer, Token). reAuthHeaderMulti handles multi-parameter
// schemes (Digest, MAC, AWS4-HMAC-SHA256) whose credential contains commas
// and spaces. reBearer is the fallback for bare "Bearer <token>" without
// the Authorization: prefix.
const reAuthHeader = /(\bAuthorization:\s*(?:Basic|Bearer|Token)\s+)\S+/gi;
const reAuthHeaderMulti = /(\bAuthorization:\s*(?:Digest|MAC|AWS4-HMAC-SHA256)\s+)[^\r\n]+/gi;
const reBearer = /(\bBearer\s+)[A-Za-z0-9._~+/-]{20,}/gi;
const reEmail = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;
const reEnvSecret = /((?:api[_-]?key|secret|token|password|passwd)\s*[=:]\s*)\S+/gi;

export function scrub(input: string): string {
  if (input === '') {
    return input;
  }

  return input
    .replace(reGitHubToken, redacted)
    .replace(reJwt, redacted)
    .replace(reAuthHeader, `$1${redacted}`)
    .replace(reAuthHeaderMulti, `$1${redacted}`)
    .replace(reBearer, `$1${redacted}`)
    .replace(reEmail, redacted)
    .replace(reEnvSecret, `$1${redacted}`);
}

export function initSentry(opts: { env?: string; release?: string } = {}): void {
  Sentry.init({
    dsn,
    environment: opts.env ?? 'production',
    release: opts.release,
    sampleRate: 1.0,
    tracesSampleRate: 0,
    sendDefaultPii: false,
    integrations: [],
    beforeSend(event) {
      scrubEvent(event);
      return event;
    },
  });
  Sentry.setTag('app', 'docs');
}

function scrubEvent(event: unknown): void {
  const eventRecord = asRecord(event);
  if (!eventRecord) {
    return;
  }

  eventRecord.message = capMessage(scrubString(eventRecord.message));
  eventRecord.transaction = scrubString(eventRecord.transaction);
  eventRecord.server_name = scrubString(eventRecord.server_name);
  eventRecord.user = undefined;

  const exception = asRecord(eventRecord.exception);
  const values = asArray(exception?.values);
  for (const value of values) {
    const exceptionValue = asRecord(value);
    if (!exceptionValue) {
      continue;
    }
    exceptionValue.type = scrubString(exceptionValue.type);
    exceptionValue.value = scrubString(exceptionValue.value);
    scrubStacktrace(exceptionValue.stacktrace);
  }

  for (const thread of asArray(eventRecord.threads)) {
    const threadRecord = asRecord(thread);
    scrubStacktrace(threadRecord?.stacktrace);
  }

  for (const breadcrumb of asArray(eventRecord.breadcrumbs)) {
    const crumb = asRecord(breadcrumb);
    if (!crumb) {
      continue;
    }
    crumb.message = scrubString(crumb.message);
    if (crumb.data !== undefined) {
      crumb.data = scrubValue(crumb.data);
    }
  }

  const request = asRecord(eventRecord.request);
  if (request) {
    request.url = scrubString(request.url);
    request.query_string = scrubString(request.query_string);
    request.data = undefined;
    request.cookies = undefined;
    request.headers = undefined;
    request.env = undefined;
  }

  eventRecord.tags = scrubValue(eventRecord.tags);
  eventRecord.contexts = scrubValue(eventRecord.contexts);
  eventRecord.fingerprint = scrubValue(eventRecord.fingerprint);
}

function scrubStacktrace(stacktrace: unknown): void {
  const stacktraceRecord = asRecord(stacktrace);
  if (!stacktraceRecord) {
    return;
  }

  for (const frameValue of asArray(stacktraceRecord.frames)) {
    const frame = asRecord(frameValue);
    if (!frame) {
      continue;
    }
    frame.filename = scrubString(frame.filename);
    frame.abs_path = scrubString(frame.abs_path);
    frame.function = scrubString(frame.function);
    frame.module = scrubString(frame.module);
    frame.package = scrubString(frame.package);
    frame.context_line = scrubString(frame.context_line);
    frame.pre_context = scrubValue(frame.pre_context);
    frame.post_context = scrubValue(frame.post_context);
    frame.vars = undefined;
  }
}

function scrubValue(value: unknown): unknown {
  if (typeof value === 'string') {
    return scrub(value);
  }

  if (Array.isArray(value)) {
    return value.map(scrubValue);
  }

  const record = asRecord(value);
  if (!record) {
    return value;
  }

  const scrubbed: Record<string, unknown> = {};
  for (const [key, nestedValue] of Object.entries(record)) {
    scrubbed[scrub(key)] = scrubValue(nestedValue);
  }
  return scrubbed;
}

function scrubString(value: unknown): string {
  return typeof value === 'string' ? scrub(value) : '';
}

function capMessage(message: string): string {
  if (message.length <= messageCap) {
    return message;
  }
  return `${message.slice(0, messageCap)}${truncMarker}`;
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return;
  }
  return value as Record<string, unknown>;
}
