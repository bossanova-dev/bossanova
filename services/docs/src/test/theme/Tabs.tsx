import React from 'react'

/**
 * Test double for Docusaurus's `@theme/Tabs`.
 *
 * The real module only resolves inside a Docusaurus build, so `vitest.config.ts`
 * aliases `@theme/Tabs` here. Tests identify tab groups by comparing an element's
 * `type` against this component and then read its props; they do not exercise
 * Docusaurus's selection behaviour, which is not ours to test.
 */
export type TabsProps = {
  groupId?: string
  children?: React.ReactNode
}

export default function Tabs({ children }: TabsProps): React.ReactElement {
  return <div>{children}</div>
}
