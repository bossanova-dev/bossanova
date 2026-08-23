# bossanova-docs

End-user documentation site for bossanova, served at
[docs.bossanova.dev](https://docs.bossanova.dev). Built with
[Docusaurus](https://docusaurus.io).

## Local development

This package has its own `pnpm-lock.yaml` and is **not** part of the pnpm
workspace at the repo root. Docusaurus pulls in deps that conflict with the
marketing/web vite tree, so we keep them isolated.

```bash
make deps     # one-time: pnpm install (docs is its own pnpm workspace root)
make dev      # http://localhost:3001
make build    # produces build/
make test     # typecheck + unit tests + build (catches broken links and MDX errors)
make format   # prettier
```

## Authoring `<CommandTabs>` examples

Bossanova exposes most operations three ways — a chat prompt to the `/boss`
skill, the `boss` CLI, and the MCP tool surface. `src/components/CommandTabs.tsx`
renders all three as one tab group, so a reader can stay on the interface they
actually use:

```mdx
import CommandTabs from '@site/src/components/CommandTabs'

<CommandTabs chat='"list my repos"' cli="boss repo ls" mcp="list_repos" />
```

Conventions, so the examples stay consistent across pages:

- **Chat** entries are quoted natural-language prompts to `/boss` — `"list my
repos"` — not commands. Write what a reader would actually type at the agent.
- **Quote the attribute so the example's own quotes survive.** JSX attribute
  strings are raw — a backslash is not an escape there, so
  `chat="\"list my repos\""` escapes nothing: the value ends at the second `"`,
  the rest is re-read as attribute names, and `make build` fails with
  `Unexpected character \ (U+005C) in attribute name`. Wrap an example
  containing `"` in single quotes (`chat='"list my repos"'`, as above) and one
  containing `'` in double quotes. An example needing both has to be reworded.
- **Use a template-literal expression for multi-line CLI examples.** A
  backslash line continuation inside a quoted JSX attribute is parsed as raw
  MDX, not shell syntax, so `cli="boss run \` followed by another line fails
  with `Unexpected character \ (U+005C) in attribute name`. Put multi-line
  commands in a JSX expression instead:

  ```mdx
  <CommandTabs
    cli={`boss session new \\
  --repo recurser/bossanova \\
  --base main`}
    chat='"start a session for recurser/bossanova"'
    mcp="create_session"
  />
  ```

- **MCP** entries are the bare tool name, `list_repos`, exactly as registered in
  `lib/bossalib/bossmcp/manifest.go`. That file is the authoritative list, so
  check a name there rather than guessing it.
- **A command with no registered MCP tool omits the `mcp` prop** rather than
  inventing one. The same goes for `chat`. The tab still renders, carrying an
  explicit "No equivalent — this command runs locally" note: several commands
  (`boss daemon install`, `boss fix-terminal`, `boss tail`, and `boss upgrade`)
  genuinely have no agent-reachable counterpart, and saying so is part of what
  the docs teach.
- **Use the component rather than a hand-written `<Tabs>` block.** Docusaurus
  syncs and persists tab choice per `groupId`, so a reader who picks _Chat_ on
  one page sees _Chat_ everywhere. One wrong `groupId` at one call site silently
  breaks that for the whole site. `CommandTabs` owns `groupId="interface"`, kept
  distinct from the `groupId="os"` platform tabs in `docs/quick-start.md`.

**MDX caveat.** Docusaurus 3 parses `.md` as MDX, so a JSX component works
inline without renaming the file — but the page needs the `import` line above,
and any pre-existing `<` or `{` in that page's prose can start erroring once MDX
takes the file seriously. `make build` (and so `make test`) is what catches
this; run it after converting a page.

## Testing `@theme/*` components

Docusaurus resolves `@theme/*` modules through webpack aliases that do not
exist in bare Vitest. A test that imports `@theme/Tabs`, `@theme/TabItem`, or
`@theme/CodeBlock` without a local alias fails during Vite import analysis
before any `vi.mock` factory can run.

Extend the existing `resolve.alias` block in `services/docs/vitest.config.ts`
when a component needs another `@theme/*` module, and put the double under
`services/docs/src/test/theme/`. The existing doubles live at
`services/docs/src/test/theme/Tabs.tsx`,
`services/docs/src/test/theme/TabItem.tsx`, and
`services/docs/src/test/theme/CodeBlock.tsx`. Keep the same two safety
conditions: production code must never import the double, and `tsc --noEmit`
must not need it to typecheck the site. The double should expose only the props
the unit test asserts; Docusaurus behavior belongs to Docusaurus.

## Linking between docs pages

**Link to another docs page relatively (`./notes.md`), never by absolute
`https://docs.bossanova.dev/...` URL** — `onBrokenLinks: 'throw'` only resolves
relative links, so an absolute self-link is never checked and a dead one never
reds the build. `scripts/check-docs-absolute-selflinks.mjs` (run by `make lint`)
enforces this; a link that genuinely must be absolute opts out with
`<!-- absolute-link: intentional -->` on that line or the line above it, which
stays greppable so every exception can be audited in one command.

## Where things live

- `docs/`: Markdown content (one file per page).
- `sidebars.ts`: Sidebar definition.
- `docusaurus.config.ts`: Site metadata, theme, plugins.
- `src/css/custom.css`: Theme tokens (mirrors `services/web/src/index.css`).
- `static/img/`: Logo, favicon, screenshots.
- `SCREENSHOTS.md`: Inventory of placeholder screenshots and what each should
  depict. Replace placeholders one at a time and update this file.

## Deploy

Production and staging deploy automatically on release via
`.github/workflows/perform-{production,staging}-release.yml`.
