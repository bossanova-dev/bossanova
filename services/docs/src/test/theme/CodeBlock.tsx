import React from 'react'

/**
 * Test double for Docusaurus's `@theme/CodeBlock`. See `./Tabs.tsx` for why the
 * alias exists.
 *
 * The real component runs Prism over `children` and adds the copy button. The
 * double keeps only what the tests assert — that the example text and its
 * `language` reach a code block at all, rather than a raw `<pre>` that would
 * get neither.
 */
export type CodeBlockProps = {
  language?: string
  children?: React.ReactNode
}

export default function CodeBlock({ language, children }: CodeBlockProps): React.ReactElement {
  return (
    <pre>
      <code className={language === undefined ? undefined : `language-${language}`}>
        {children}
      </code>
    </pre>
  )
}
