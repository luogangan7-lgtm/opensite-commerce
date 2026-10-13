/** Regenerates OpenAPI TS into an owned temporary file and rejects drift; Bun source gates call it before typecheck. */
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
mkdirSync('.work',{recursive:true});
const directory=mkdtempSync('.work/api-contract-');
try {
  const output=join(directory,'schema.d.ts');
  const result=Bun.spawnSync(['bun','packages/api-client/node_modules/openapi-typescript/bin/cli.js','api/openapi.yaml','-o',output],{stdout:'pipe',stderr:'inherit'});
  if(result.exitCode!==0) throw new Error('OpenAPI client generation failed');
  if(readFileSync(output,'utf8')!==readFileSync('packages/api-client/src/schema.d.ts','utf8')) throw new Error('Generated API client drift; regenerate and commit schema.d.ts');
  console.log('PASS: generated OpenAPI TS matches committed types');
} finally {rmSync(directory,{recursive:true,force:true});}
