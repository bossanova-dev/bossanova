import { themes as prismThemes } from 'prism-react-renderer';
import type { Config } from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const posthogHost = process.env.PUBLIC_POSTHOG_HOST ?? 'https://k.bossanova.dev';
const posthogProjectToken = process.env.PUBLIC_POSTHOG_PROJECT_TOKEN;
const bossEnv = process.env.PUBLIC_BOSS_ENV ?? 'production';
const buildSha = process.env.PUBLIC_BUILD_SHA;

const config: Config = {
  title: 'Bossanova',
  tagline: 'Manage multiple AI coding-agent sessions from one terminal.',
  favicon: 'img/favicon.svg',

  url: 'https://docs.bossanova.dev',
  baseUrl: '/',

  organizationName: 'bossanova-dev',
  projectName: 'bossanova',

  onBrokenLinks: 'throw',

  customFields: {
    posthogHost,
    posthogProjectToken,
    bossEnv,
    buildSha,
  },

  clientModules: ['./src/clientModule.ts'],

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
      title: 'bossanova',
      logo: {
        alt: 'Bossanova',
        src: 'img/logo.png',
        href: 'https://bossanova.dev',
        target: '_self',
      },
      items: [
        {
          href: 'https://bossanova.dev/cloud',
          label: 'Cloud',
          position: 'right',
          target: '_self',
        },
        {
          to: '/',
          label: 'Docs',
          position: 'right',
        },
        {
          href: 'https://bossanova.dev/pricing',
          label: 'Pricing',
          position: 'right',
          target: '_self',
        },
        {
          href: 'https://github.com/bossanova-dev/bossanova',
          label: 'GitHub',
          position: 'right',
          target: '_blank',
        },
        {
          href: 'https://app.bossanova.dev',
          label: 'Sign in',
          position: 'right',
          target: '_self',
        },
        {
          href: 'https://bossanova.dev/quick-start',
          label: 'Try Now',
          position: 'right',
          className: 'navbar__link--cta',
          target: '_self',
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
