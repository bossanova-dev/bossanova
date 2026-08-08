import React from 'react'

/**
 * Test double for Docusaurus's `@theme/TabItem`. See `./Tabs.tsx` for why the
 * alias exists.
 *
 * `attributes` mirrors the real prop, which Docusaurus spreads onto the
 * `<li role="tab">` — that is where the `data-testid` hooks used by browser-level
 * proof captures end up.
 */
export type TabItemProps = {
  value: string
  label?: string
  default?: boolean
  attributes?: Record<string, string>
  children?: React.ReactNode
}

export default function TabItem({ children }: TabItemProps): React.ReactElement {
  return <div>{children}</div>
}
