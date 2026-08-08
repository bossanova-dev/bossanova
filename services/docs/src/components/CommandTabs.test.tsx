import React from 'react'
import { describe, expect, it } from 'vitest'
import Tabs from '@theme/Tabs'
import type { Props as TabsProps } from '@theme/Tabs'
import TabItem from '@theme/TabItem'
import type { Props as TabItemProps } from '@theme/TabItem'
import CodeBlock from '@theme/CodeBlock'
import type { Props as CodeBlockProps } from '@theme/CodeBlock'
import CommandTabs, { COMMAND_TABS_GROUP_ID, NO_EQUIVALENT_NOTE } from './CommandTabs'

// `@theme/Tabs`, `@theme/TabItem`, and `@theme/CodeBlock` only resolve inside a
// Docusaurus build, so `vitest.config.ts` aliases them to the doubles in
// `src/test/theme/`. What is under test is our prop-to-tab mapping and the
// no-equivalent path, not Docusaurus's tab widget — so these assertions read the
// element tree the component produces rather than rendering it.

/** The `<Tabs>` element `CommandTabs` produced, with its group and tab children. */
function renderTabs(element: React.ReactElement): {
  groupId: string | undefined
  items: TabItemProps[]
} {
  const tabs = element as React.ReactElement<TabsProps>
  expect(tabs.type).toBe(Tabs)

  const items = React.Children.toArray(tabs.props.children).map((child) => {
    expect(React.isValidElement(child)).toBe(true)
    const item = child as React.ReactElement<TabItemProps>
    expect(item.type).toBe(TabItem)
    return item.props
  })

  return { groupId: tabs.props.groupId, items }
}

function tabFor(element: React.ReactElement, value: string): TabItemProps {
  const found = renderTabs(element).items.find((item) => item.value === value)
  if (!found) {
    throw new Error(`no tab rendered for value ${value}`)
  }
  return found
}

/**
 * The `<CodeBlock>` a populated tab body renders, so both the code text and the
 * `language` it is highlighted as can be asserted. A raw `<pre><code>` here
 * would fail: examples must go through the theme component to get Prism and the
 * copy button.
 */
function codeOf(node: React.ReactNode): { language: string | undefined; text: string } {
  expect(React.isValidElement(node)).toBe(true)
  const block = node as React.ReactElement<CodeBlockProps>
  expect(block.type).toBe(CodeBlock)
  return { language: block.props.language, text: textOf(block.props.children) }
}

/** Flattens the visible text of a node, which is host elements all the way down. */
function textOf(node: React.ReactNode): string {
  if (node === null || node === undefined || typeof node === 'boolean') {
    return ''
  }
  if (typeof node === 'string' || typeof node === 'number') {
    return String(node)
  }
  if (Array.isArray(node)) {
    return node.map(textOf).join('')
  }
  if (React.isValidElement(node)) {
    const element = node as React.ReactElement<{ children?: React.ReactNode }>
    expect(typeof element.type).toBe('string')
    return textOf(element.props.children)
  }
  return ''
}

const allProps = {
  chat: '"list my notes"',
  cli: 'boss notes ls',
  mcp: 'list_notes',
}

describe('CommandTabs', () => {
  it('renders Chat, CLI, and MCP tabs carrying their examples', () => {
    const element = CommandTabs(allProps)

    expect(tabFor(element, 'chat').label).toBe('Chat')
    expect(tabFor(element, 'cli').label).toBe('CLI')
    expect(tabFor(element, 'mcp').label).toBe('MCP')

    expect(codeOf(tabFor(element, 'chat').children).text).toBe('"list my notes"')
    expect(codeOf(tabFor(element, 'cli').children).text).toBe('boss notes ls')
    expect(codeOf(tabFor(element, 'mcp').children).text).toBe('list_notes')
  })

  it('renders examples as theme code blocks, so they are highlighted and copyable', () => {
    const element = CommandTabs(allProps)

    // `codeOf` fails outright on a raw `<pre>`; the languages are what Prism keys on.
    expect(codeOf(tabFor(element, 'chat').children).language).toBe('text')
    expect(codeOf(tabFor(element, 'cli').children).language).toBe('bash')
    expect(codeOf(tabFor(element, 'mcp').children).language).toBe('text')
  })

  it('renders exactly three tabs, in Chat / CLI / MCP order', () => {
    const { items } = renderTabs(CommandTabs(allProps))

    expect(items.map((item) => item.value)).toEqual(['chat', 'cli', 'mcp'])
  })

  it('marks CLI as the default-selected tab', () => {
    const element = CommandTabs(allProps)

    expect(tabFor(element, 'cli').default).toBe(true)
    expect(tabFor(element, 'chat').default).toBeUndefined()
    expect(tabFor(element, 'mcp').default).toBeUndefined()
  })

  it('syncs on the interface group, not the os group quick-start.md owns', () => {
    expect(renderTabs(CommandTabs(allProps)).groupId).toBe('interface')
    expect(COMMAND_TABS_GROUP_ID).toBe('interface')
    expect(COMMAND_TABS_GROUP_ID).not.toBe('os')
  })

  it('still renders the MCP tab, with the no-equivalent note, when mcp is omitted', () => {
    const element = CommandTabs({ chat: allProps.chat, cli: allProps.cli })

    const mcp = tabFor(element, 'mcp')
    expect(mcp.label).toBe('MCP')
    expect(textOf(mcp.children)).toBe(NO_EQUIVALENT_NOTE)

    // The tab is explained, not dropped, and the others keep their examples.
    expect(renderTabs(element).items).toHaveLength(3)
    expect(codeOf(tabFor(element, 'chat').children).text).toBe('"list my notes"')
    expect(codeOf(tabFor(element, 'cli').children).text).toBe('boss notes ls')
  })

  it('still renders the Chat tab, with the no-equivalent note, when chat is omitted', () => {
    const element = CommandTabs({ cli: allProps.cli, mcp: allProps.mcp })

    const chat = tabFor(element, 'chat')
    expect(chat.label).toBe('Chat')
    expect(textOf(chat.children)).toBe(NO_EQUIVALENT_NOTE)

    expect(renderTabs(element).items).toHaveLength(3)
    expect(codeOf(tabFor(element, 'cli').children).text).toBe('boss notes ls')
    expect(codeOf(tabFor(element, 'mcp').children).text).toBe('list_notes')
  })

  it('treats an empty prop as no equivalent, not as an empty code block', () => {
    const element = CommandTabs({ ...allProps, mcp: '' })

    expect(textOf(tabFor(element, 'mcp').children)).toBe(NO_EQUIVALENT_NOTE)
  })

  it('gives every tab a stable hook for browser-level captures', () => {
    const element = CommandTabs(allProps)

    expect(tabFor(element, 'chat').attributes).toEqual({ 'data-testid': 'command-tab-chat' })
    expect(tabFor(element, 'cli').attributes).toEqual({ 'data-testid': 'command-tab-cli' })
    expect(tabFor(element, 'mcp').attributes).toEqual({ 'data-testid': 'command-tab-mcp' })
  })

  it('spells the no-equivalent note exactly, since it is reader-facing copy', () => {
    // Pinned because the wording is the feature: the note is what teaches that a
    // command is local-only. `services/docs/README.md` quotes it verbatim as the
    // authoring convention, so a silent reword here would desync that doc.
    expect(NO_EQUIVALENT_NOTE).toBe('No equivalent — this command runs locally')
  })
})
