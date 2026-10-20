# Cron gate

Schedule weekly with prompt `/boss-retro`. Gate cwd is the repository root. A repository carrying
the toolbox uses `node skills-toolbox/cron-gates/boss-retro.mjs [--threshold N]`; otherwise resolve
the installed skill's toolbox and run `node "$BOSS_RETRO_TOOLBOX/cron-gates/boss-retro.mjs"`.

Threshold precedence: flag, `BOSS_RETRO_GATE_THRESHOLD`, `retro.gateThreshold`, then 5. The gate
counts improvement notes created after the last completed write run. Only below threshold does it
collect preview signals, bounded to 40 seconds. Signal failures contribute zero. Missing or corrupt
state counts all notes with a warning. Guidance audit findings and completed-ticket follow-through
are considered by the run, not the gate; manual `/boss-retro` can inspect them without new notes.

Exit 0 starts a session when the threshold is met, or when the notes CLI cannot be resolved and the
session must decide. Exit 1 skips without agent tokens for invalid input, unreadable notes or no
work. Read the scheduler's `gate_output` once to learn why it skipped. Over-cap leftovers wait for
new activity or a manual run. Dry runs never advance the watermark.
