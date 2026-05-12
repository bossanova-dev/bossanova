# bossanova-docs

End-user documentation site for bossanova, served at
[docs.bossanova.dev](https://docs.bossanova.dev). Built with
[Docusaurus](https://docusaurus.io).

## Local development

This package has its own `pnpm-lock.yaml` and is **not** part of the pnpm
workspace at the repo root. Docusaurus pulls in deps that conflict with the
marketing/web vite tree, so we keep them isolated.

```bash
make deps     # one-time: pnpm install --ignore-workspace
make dev      # http://localhost:3001
make build    # produces build/
make test     # typecheck + build (catches broken links and MDX errors)
make format   # prettier
```

## Where things live

- `docs/`: Markdown content (one file per page).
- `sidebars.ts`: Sidebar definition.
- `docusaurus.config.ts`: Site metadata, theme, plugins.
- `src/css/custom.css`: Theme tokens (mirrors `services/web/src/index.css`).
- `static/img/`: Logo, favicon, screenshots.
- `SCREENSHOTS.md`: Inventory of placeholder screenshots and what each
  should depict. Replace placeholders one at a time and update this file.

## Deploy

Production and staging deploy automatically on release via
`.github/workflows/perform-{production,staging}-release.yml`.
