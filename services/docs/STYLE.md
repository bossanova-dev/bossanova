# Documentation prose style

Write for a reader trying to finish a task. Prefer concrete actors, commands, and
outcomes over slogans or marketing language. The shared banned register lives in
`styles/BossanovaProse/BannedWords.yml`; the email course's
`docs/email-course/VOICE.md` explains the voice behind that list.

## Em dashes

Treat em dashes as a limited resource. Use no more than one in a sentence, then
keep each file at or below its recorded em-dash density in `prose-baseline.json`.
Prefer a full stop, colon, comma, or shorter sentence when the thought works
without an aside.

The density gate ignores places where the dash labels structure rather than
setting prose rhythm:

- fenced code blocks;
- Markdown table rows; and
- definition labels in the form `**Term** — definition`.

## Bold text

Bold a term when the emphasis helps a reader scan a procedure, definition, or
warning. Do not bold a bare function word in the middle of a sentence. Bold is
not vocal stress, so a sentence should remain clear when its emphasis is removed.

Two forms are deliberate exceptions:

- `**Term** — definition` introduces a labelled definition; and
- bolded negation such as `does **not** require` is allowed in reference
  documentation when missing the negative would make the instruction unsafe or
  materially wrong.

## Mechanical checks

`pnpm run lint:prose` runs Vale for pattern rules and the per-file density
ratchet. Add every new Markdown or MDX document to `prose-baseline.json`; missing
entries fail instead of inheriting a loose global ceiling. Run the Vale fixture
tests when changing a rule so every rule proves it still reports a real alert.
