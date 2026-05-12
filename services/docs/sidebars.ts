import type { SidebarsConfig } from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docs: [
    {
      type: 'category',
      label: 'Get Started',
      collapsed: false,
      items: ['intro', 'install', 'quick-start'],
    },
    {
      type: 'category',
      label: 'Concepts',
      collapsed: false,
      items: ['how-it-works', 'concepts/worktrees', 'plugins'],
    },
    {
      type: 'category',
      label: 'Guides',
      collapsed: false,
      items: [
        'guides/agent-runners',
        'guides/setup-scripts',
        'guides/pr-lifecycle',
        'guides/scheduled-sessions',
        'guides/web',
      ],
    },
    {
      type: 'category',
      label: 'Configuration',
      collapsed: false,
      items: ['reference/settings', 'reference/cli-reference'],
    },
    {
      type: 'category',
      label: 'Security & Privacy',
      collapsed: false,
      items: ['reference/privacy', 'reference/security-and-permissions'],
    },
    {
      type: 'category',
      label: 'Help',
      collapsed: false,
      items: ['help/faq', 'help/troubleshooting', 'help/uninstall'],
    },
  ],
};

export default sidebars;
