// Generates CSS and typed constants from tokens.json; called by workspace gates and developers.
// Depends on Bun and node stdlib only. --check never writes; output ordering is stable.
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
const base = new URL('./', import.meta.url);
export function renderTokens(tokens: Record<string,string>) {
  const entries=Object.entries(tokens).sort(([a],[b])=>a.localeCompare(b,'en'));
  for (const [key,value] of entries) if (!/^[a-z][a-z0-9-]*$/.test(key) || typeof value !== 'string' || /[;{}\n]/.test(value)) throw new Error(`Invalid token ${key}`);
  return {
    css: `/* Generated from tokens.json. Run bun packages/design-tokens/generate.ts. */\n:root {\n${entries.map(([key,value])=>`  --${key}: ${value};`).join('\n')}\n}\n`,
    ts: `// Generated from tokens.json; consumed by UI. Never edit derived constants.\nexport const tokens = ${JSON.stringify(Object.fromEntries(entries),null,2)} as const;\nexport type TokenName = keyof typeof tokens;\nexport const tokenVar = (name: TokenName): string => \`var(--\${name})\`;\n`,
  };
}
if (import.meta.main) {
 const source=JSON.parse(readFileSync(new URL('tokens.json',base),'utf8'));
 const outputs=renderTokens(source.tokens);
 for (const [extension,content] of Object.entries(outputs)) {
   const path=new URL(`src/tokens.${extension}`,base);
   if (process.argv.includes('--check')) {
     let actual='';try {actual=readFileSync(path,'utf8');}catch {}
     if (actual!==content) {console.error(`Stale generated file: ${fileURLToPath(path)}`);process.exitCode=1;}
   } else writeFileSync(path,content);
 }
}
