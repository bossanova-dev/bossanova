#!/usr/bin/env node

import fs from 'node:fs';

function usage() {
  console.error("Usage: trim-cast.mjs input.cast output.cast 'text to find'");
  process.exit(1);
}

function formatNeedle(needle) {
  return `'${needle.replaceAll('\\', '\\\\').replaceAll("'", "\\'")}'`;
}

function readCast(inputPath) {
  const lines = fs.readFileSync(inputPath, 'utf8').split('\n');
  const headerLine = lines.shift();
  const header = JSON.parse(headerLine);
  const rawEvents = lines.filter((line) => line.trim()).map((line) => JSON.parse(line));

  return { header, rawEvents };
}

function trimVersion2(rawEvents, needle) {
  let matchTime = null;

  for (const event of rawEvents) {
    if (event.length < 3) continue;

    const [time, eventType, data] = event;
    if (eventType === 'o' && data.includes(needle)) {
      matchTime = time;
      break;
    }
  }

  if (matchTime === null) {
    throw new Error(`Could not find text: ${formatNeedle(needle)}`);
  }

  return rawEvents
    .filter((event) => event[0] >= matchTime)
    .map((event) => [Math.max(0, event[0] - matchTime), ...event.slice(1)]);
}

function trimVersion3(rawEvents, needle) {
  let elapsed = 0.0;
  let matchIndex = null;

  for (const [index, event] of rawEvents.entries()) {
    elapsed += event[0];

    if (event.length < 3) continue;

    const [, eventType, data] = event;
    if (eventType === 'o' && data.includes(needle)) {
      matchIndex = index;
      break;
    }
  }

  if (matchIndex === null) {
    throw new Error(`Could not find text: ${formatNeedle(needle)}`);
  }

  const trimmedEvents = rawEvents.slice(matchIndex).map((event) => [...event]);
  trimmedEvents[0][0] = 0;
  return trimmedEvents;
}

function trimCast(inputPath, outputPath, needle) {
  const { header, rawEvents } = readCast(inputPath);
  const { version } = header;

  if (version !== 2 && version !== 3) {
    throw new Error(`Unsupported or missing asciicast version: ${version}`);
  }

  const trimmedEvents =
    version === 2 ? trimVersion2(rawEvents, needle) : trimVersion3(rawEvents, needle);

  const outputLines = [
    JSON.stringify(header),
    ...trimmedEvents.map((event) => JSON.stringify(event)),
  ];
  fs.writeFileSync(outputPath, `${outputLines.join('\n')}\n`, 'utf8');
}

const [, , inputPath, outputPath, needle] = process.argv;

if (process.argv.length !== 5) {
  usage();
}

try {
  trimCast(inputPath, outputPath, needle);
} catch (error) {
  console.error(error.message);
  process.exit(1);
}
