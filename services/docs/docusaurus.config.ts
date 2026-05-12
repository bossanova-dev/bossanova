import { themes as prismThemes } from 'prism-react-renderer';
import type { Config } from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Bossanova',
  tagline: 'Manage multiple AI coding-agent sessions from one terminal.',
  favicon: 'img/favicon.svg',

  url: 'https://docs.bossanova.dev',
  baseUrl: '/',

  organizationName: 'bossanova-dev',
  projectName: 'bossanova',

  onBrokenLinks: 'throw',

  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/bossanova-dev/bossanova/edit/main/services/docs/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  plugins: [
    [
      require.resolve('@easyops-cn/docusaurus-search-local'),
      {
        hashed: true,
        indexBlog: false,
        docsRouteBasePath: '/',
      },
    ],
  ],

  themeConfig: {
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'Bossanova',
      logo: {
        alt: 'Bossanova',
        src: 'img/logo.png',
      },
      items: [
        {
          href: 'https://github.com/bossanova-dev/bossanova',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Project',
          items: [
            {
              label: 'GitHub',
              href: 'https://github.com/bossanova-dev/bossanova',
            },
            {
              label: 'docs.bossanova.dev',
              href: 'https://docs.bossanova.dev',
            },
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Recurser Inc.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'go', 'json', 'toml', 'yaml'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
