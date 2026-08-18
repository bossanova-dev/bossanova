import React from 'react'
import Tabs from '@theme/Tabs'
import TabItem from '@theme/TabItem'
import CodeBlock from '@theme/CodeBlock'

/**
 * Tab-sync group for the Chat | CLI | MCP triplet. Docusaurus persists and
 * syncs the selected tab per `groupId`, so every `<CommandTabs>` on the site
 * must share this one — and it must stay distinct from the `os` group that
 * `quick-start.md` uses for platform tabs, or picking macOS would also pick a
 * command interface.
 */
export const COMMAND_TABS_GROUP_ID = 'interface'

/**
 * Rendered in place of an example when a command has no counterpart on that
 * interface. The tab still renders: the absence is the point, not an oversight.
 */
export const NO_EQUIVALENT_NOTE = 'No equivalent — this command runs locally'

export type CommandTabsProps = {
  /** Natural-language prompt to the `/boss` skill, e.g. `"list my repos"`. */
  chat?: string
  /** The `boss` CLI invocation, e.g. `boss notes ls`. */
  cli?: string
  /**
   * Bare MCP tool name as registered in the `lib/bossalib/bossmcp` package (the
   * tools are registered across `tools.go`, `tools_mutating.go` and
   * `tools_destructive.go` — `manifest.go` itself registers none).
   *
   * `scripts/check-docs-mcp-props.mjs` gates this prop in `make lint`, but
   * presence in the tool set is **necessary, not sufficient**: the gate only
   * proves the name exists. It cannot tell whether the named tool does what the
   * `cli` example beside it does, and a real tool with the wrong parameters
   * passes it while still documenting a wrong equivalence. Before adding or
   * changing this prop, read the named tool's parameters and confirm they
   * actually express the CLI command's effect — a green gate is not an
   * endorsement of the equivalence.
   */
  mcp?: string
}

/**
 * The body of one tab: the example if the caller supplied one, otherwise the
 * explicit no-equivalent note. An empty string counts as not supplied — a
 * conditional call site that renders `mcp={tool ?? ''}` should get the note
 * rather than an empty code block, which reads as a rendering bug.
 *
 * Examples go through `@theme/CodeBlock` rather than a raw `<pre><code>`: MDX
 * maps fenced blocks onto that component, and a hand-rolled `<pre>` in a React
 * component does not go through `MDXComponents`, so it would silently lose
 * Prism highlighting and the copy button that every other code block on the
 * site has.
 */
function pane(value: string | undefined, language: string): React.ReactElement {
  if (value === undefined || value === '') {
    return <p className="commandTabs__noEquivalent">{NO_EQUIVALENT_NOTE}</p>
  }
  return <CodeBlock language={language}>{value}</CodeBlock>
}

/**
 * Renders one operation three ways — as a chat prompt, a CLI command, and an
 * MCP tool name — as a site-synced tab group. Every tab always renders; an
 * omitted prop produces the no-equivalent note rather than a hidden tab.
 */
export default function CommandTabs({ chat, cli, mcp }: CommandTabsProps): React.ReactElement {
  return (
    <Tabs groupId={COMMAND_TABS_GROUP_ID}>
      <TabItem value="chat" label="Chat" attributes={{ 'data-testid': 'command-tab-chat' }}>
        {pane(chat, 'text')}
      </TabItem>
      <TabItem value="cli" label="CLI" default attributes={{ 'data-testid': 'command-tab-cli' }}>
        {pane(cli, 'bash')}
      </TabItem>
      <TabItem value="mcp" label="MCP" attributes={{ 'data-testid': 'command-tab-mcp' }}>
        {pane(mcp, 'text')}
      </TabItem>
    </Tabs>
  )
}
