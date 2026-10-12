// Token-only style gate; scans UI/admin sources, called by CI and focused regression tests.
// Depends only on stdlib. Zero/unitless layout values are allowed; physical spacing is forbidden.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
export function lintText(source:string,path:string): string[] {
 const text=source.replace(/\/\*[\s\S]*?\*\//g,'').replace(/(^|\n)\s*\/\/[^\n]*/g,'$1');
 const errors:string[]=[];
 function reject(pattern:RegExp,rule:string) {for(const match of text.matchAll(pattern)) errors.push(`${path}:${text.slice(0,match.index).split('\n').length}: ${rule}: ${match[0]}`);}
 reject(/#[\da-f]{3,8}\b|\b(?:rgb|rgba|hsl|hsla)\s*\(/gi,'literal color');
 reject(/(?:\d*\.)?\d+(?:px|rem|em|pt|pc|cm|mm|in|vh|vw|vmin|vmax)\b/gi,'literal length');
 reject(/\b(?:margin|padding)-(?:left|right)\s*:|\b(?:margin|padding)(?:Left|Right)\s*:/g,'physical spacing');
 const properties=/\b((?:margin|padding|gap|row-gap|column-gap|font-size|border-radius|color|background-color|border-color)(?:-(?:inline|block)(?:-start|-end)?)?|(?:margin|padding)(?:Inline|Block)?(?:Start|End)?|fontSize|borderRadius|rowGap|columnGap|backgroundColor|borderColor)\s*:\s*([^;\n}]+)/g;
 for(const match of text.matchAll(properties)) {
  const value=match[2].replace(/[,\s]+$/,'').trim().replace(/^['"]|['"]$/g,'');
  // Token references and CSS keywords are valid; calc/min/max/clamp may only derive sizes from tokens.
  const withoutVars=value.replace(/(?:var|env)\([^)]*\)/g,'TOKEN');
  const valid=/^(?:0|auto|inherit|initial|unset|revert|normal|none|transparent|currentColor)$/.test(value)
    || (/TOKEN/.test(withoutVars) && !/(?:#[\da-f]|\b\d+(?:\.\d+)?\s*(?:%|px|rem|em|vh|vw)|\b(?:red|blue|white|black)\b)/i.test(withoutVars));
  if(!valid) errors.push(`${path}:${text.slice(0,match.index).split('\n').length}: non-token ${match[1]}: ${value}`);
 }
 return errors;
}
export function collectStyleFiles(directory:string):string[] {
 let entries;try {entries=readdirSync(directory,{withFileTypes:true});}catch {return [];}
 return entries.flatMap(entry=>{
  if(['node_modules','dist','dist-gallery','build','.git','coverage'].includes(entry.name)) return [];
  const path=join(directory,entry.name);return entry.isDirectory()?collectStyleFiles(path):/\.(css|tsx|ts)$/.test(path)?[path]:[];
 });
}
if(import.meta.main) {
 const paths=['packages/ui','web/admin'].flatMap(collectStyleFiles);
 if(!paths.length) {console.error('No UI/admin source files found');process.exitCode=1;}
 const errors=paths.flatMap(path=>lintText(readFileSync(path,'utf8'),path));
 if(errors.length) {console.error(errors.join('\n'));process.exitCode=1;}else if(paths.length) console.log(`Styles PASS (${paths.length} files)`);
}
