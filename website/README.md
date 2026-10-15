# agtd.sh

The agtd marketing site and docs: Astro with Starlight, styled with the agtd
design system (colours, type, logos and the Button, Terminal and Dot components).

```sh
npm install
npm run dev      # http://localhost:4321
npm run build    # static output in dist/
```

- `src/pages/index.astro`: the landing page, on `src/layouts/Marketing.astro`.
- `src/content/docs/docs/`: the docs, served under `/docs/` by Starlight. The
  sidebar is in `astro.config.mjs`.
- `src/styles/tokens.css`: the design system's tokens and components, shared by
  both. `src/styles/starlight.css` maps Starlight's variables onto them.
- `src/components/`: `Terminal` and `AgtLs` show agt as it prints; `ThemeToggle`
  replaces Starlight's theme select and shares its storage key.

`npm run dev` and `npm run build` copy the repository's `install.sh` to
`public/install.sh` first, so `curl -fsSL https://agtd.sh/install.sh | bash`
always serves the script on the deployed commit. The copy is not checked in.

## Deploying

Vercel, with the project's Root Directory set to `website`. `vercel.json` sets
the build and serves `install.sh` as a shell script. The build reads
`../install.sh`, so keep "Include files outside the root directory" enabled.

Vercel only builds a commit that changes `website/` or `install.sh` since the
branch's last deployment (`ignoreCommand` in `vercel.json`); everything else is
skipped as "Canceled". If git cannot tell, it builds.
