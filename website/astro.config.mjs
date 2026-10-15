// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// The landing page is src/pages/index.astro; Starlight serves the docs under
// /docs/ from src/content/docs/docs/.
export default defineConfig({
	site: 'https://agtd.sh',
	integrations: [
		starlight({
			title: 'agtd docs',
			description: 'Run coding agents like processes. Supervise them like jobs.',
			favicon: '/favicon.svg',
			head: [
				{ tag: 'link', attrs: { rel: 'icon', href: '/favicon-32.png', sizes: '32x32', type: 'image/png' } },
				{ tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' } },
				{ tag: 'meta', attrs: { property: 'og:image', content: 'https://agtd.sh/agtd-icon-512.png' } },
			],
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/robmorgan/agentd' }],
			editLink: { baseUrl: 'https://github.com/robmorgan/agentd/edit/main/website/' },
			customCss: [
				'@fontsource/ibm-plex-mono/400.css',
				'@fontsource/ibm-plex-mono/500.css',
				'@fontsource/ibm-plex-sans/400.css',
				'@fontsource/ibm-plex-sans/500.css',
				'./src/styles/tokens.css',
				'./src/styles/starlight.css',
			],
			components: {
				SiteTitle: './src/components/starlight/SiteTitle.astro',
				ThemeSelect: './src/components/starlight/ThemeSelect.astro',
			},
			expressiveCode: {
				themes: ['github-dark-default', 'github-light-default'],
				styleOverrides: {
					borderRadius: '10px',
					borderColor: 'var(--line)',
					codeFontFamily: 'var(--font-mono)',
					uiFontFamily: 'var(--font-sans)',
					codeBackground: 'var(--surface)',
					frames: {
						editorTabBarBackground: 'var(--surface)',
						terminalTitlebarBackground: 'var(--surface)',
						terminalBackground: 'var(--surface)',
						frameBoxShadowCssValue: 'none',
					},
				},
			},
			sidebar: [
				{
					label: 'Getting started',
					items: [
						{ label: 'Quickstart', slug: 'docs' },
						{ label: 'Install', link: '/docs/#install' },
						{ label: 'How it works', slug: 'docs/how-it-works' },
					],
				},
				{
					label: 'Concepts',
					items: [
						{ label: 'Tasks, threads and artifacts', slug: 'docs/concepts/tasks-threads-artifacts' },
						{ label: 'Attention model', slug: 'docs/attention-model' },
						{ label: 'Working directories and worktrees', slug: 'docs/concepts/working-directories' },
						{ label: 'What a session produced', slug: 'docs/concepts/what-a-session-produced' },
					],
				},
				{
					label: 'Using agt',
					items: [
						{ label: 'Attach and detach', slug: 'docs/using/attach-detach' },
						{ label: 'Events and notifications', slug: 'docs/using/events' },
						{ label: 'Workspaces', slug: 'docs/using/workspaces' },
						{ label: 'Configure agents', slug: 'docs/using/configure-agents' },
					],
				},
				{
					label: 'Remote access',
					items: [
						{ label: 'Enable on a devbox', slug: 'docs/remote/enable' },
						{ label: 'Pair a laptop', slug: 'docs/remote/pair' },
						{ label: 'Working across hosts', slug: 'docs/remote/across-hosts' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'CLI commands', slug: 'docs/reference/cli' },
						{ label: 'config.toml', slug: 'docs/reference/config' },
						{ label: 'Troubleshooting', slug: 'docs/reference/troubleshooting' },
					],
				},
			],
		}),
	],
});
