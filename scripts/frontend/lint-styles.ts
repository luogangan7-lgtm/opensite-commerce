// Token-only style gate; scans UI/admin sources, called by CI and focused regression tests.
// Depends only on stdlib. Zero/unitless layout values are allowed; physical spacing is forbidden.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

// Read CSS declarations and quoted/camelCase React style properties. Delimiters
// inside functions/strings belong to the value, including comma-separated shadows.
function* declarations(text:string) {
 const properties=/([a-zA-Z][\w-]*|"[a-zA-Z][\w-]*"|'[a-zA-Z][\w-]*')\s*:\s*/y;
 for(let cursor=0;cursor<text.length;) {
  properties.lastIndex=cursor;
  const match=/[\w-]/.test(text[cursor-1]??'')?null:properties.exec(text);
  if(!match) {
   const quote=text[cursor++];
   if(quote==='"'||quote==="'"||quote==='`') {
    while(cursor<text.length) {if(text[cursor++]==='\\') cursor++;else if(text[cursor-1]===quote) break;}
   }
   continue;
  }
  const start=properties.lastIndex;
  let end=start,depth=0,quote='';
  const quoted=text[start]==='"'||text[start]==="'"||text[start]==='`';
  for(;end<text.length;end++) {
   const char=text[end];
   if(quote) {if(char==='\\') end++;else if(char===quote) {quote='';if(quoted) {end++;break;}}continue;}
   if(char==='"'||char==="'"||char==='`') {quote=char;continue;}
   // A selector pseudo-class or an enclosing JS object is not a declaration;
   // resume inside its block so its first actual property is still checked.
   if(depth===0&&char==='{') {end++;break;}
   // An unmatched closer belongs to an enclosing at-rule condition such as
   // @media (x: y); end here so the scan resumes and checks the block body.
   if(depth===0&&(char===')'||char===']')) break;
   if(char==='('||char==='[') depth++;
   else if(char===')'||char===']') depth--;
   if(depth===0&&(char===';'||char==='}'||(char===','&&/^\s*["']?[a-zA-Z][\w-]*["']?\s*:/.test(text.slice(end+1))))) break;
  }
  const value=text.slice(start,end).trim().replace(/,\s*$/,'').replace(/^['"`]|['"`]$/g,'');
  const raw=match[1].replace(/^['"]|['"]$/g,'');
  // CSS names are case insensitive; React multiword names use camelCase.
  const folded=raw.toLowerCase();
  const property=raw.includes('-')||/^(?:color|background|border|outline|fill|stroke|margin|padding|gap)$/.test(folded)
   ?folded:raw.replace(/[A-Z]/g,char=>`-${char.toLowerCase()}`);
  yield {property,value,index:match.index};
  cursor=end;
 }
}

const colorFunctions=new Set(['var','calc','min','max','clamp','color-mix','linear-gradient','repeating-linear-gradient','radial-gradient','repeating-radial-gradient','conic-gradient','repeating-conic-gradient']);
// Grammar keywords only: every other bare word (including all CSS named colors)
// is a literal. Token names are removed individually, never their fallback values.
const colorKeywords=new Set(('inherit initial unset revert revert-layer none transparent currentcolor auto invert important '+
 'solid dashed dotted double groove ridge inset outset hidden '+
 'repeat repeat-x repeat-y no-repeat space round cover contain scroll fixed local border-box padding-box content-box text '+
 'to at top bottom left right center start end circle ellipse closest-side farthest-side closest-corner farthest-corner '+
 'in srgb srgb-linear display-p3 a98-rgb prophoto-rgb rec2020 lab oklab lch oklch xyz xyz-d50 xyz-d65 longer shorter increasing decreasing hue').split(' '));
function tokenColor(value:string) {
 const text=value.replace(/url\((?:[^()"'\\]|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')*\)/gi,'')
  .replace(/\bvar\(\s*--[\w-]+\s*(?=[,)])/gi,'var(');
 for(const match of text.matchAll(/[a-z_-][\w-]*/gi)) {
  const word=match[0].toLowerCase();
  const isFunction=/^\s*\(/.test(text.slice(match.index+word.length));
  if(!(isFunction?colorFunctions:colorKeywords).has(word)) return false;
 }
 // url() images carry no color; any color words beside them were already vetted above.
 return /\bvar\(/i.test(text)||/\burl\(/i.test(value)||/^(?:0|inherit|initial|unset|revert|revert-layer|none|transparent|currentColor|auto|invert)$/i.test(value.replace(/\s*!important\s*$/i,'').trim());
}

export function lintText(source:string,path:string): string[] {
 const text=source.replace(/\/\*[\s\S]*?\*\//g,'').replace(/(^|\n)\s*\/\/[^\n]*/g,'$1');
 const errors:string[]=[];
 function reject(pattern:RegExp,rule:string) {for(const match of text.matchAll(pattern)) errors.push(`${path}:${text.slice(0,match.index).split('\n').length}: ${rule}: ${match[0]}`);}
 reject(/#[\da-f]{3,8}\b|\b(?:rgb|rgba|hsl|hsla)\s*\(/gi,'literal color');
 reject(/(?:\d*\.)?\d+(?:px|rem|em|pt|pc|cm|mm|in|vh|vw|vmin|vmax)\b/gi,'literal length');
 for(const {property,value,index} of declarations(text)) {
  if(/^(?:margin|padding)-(?:left|right)$/.test(property)) {
   errors.push(`${path}:${text.slice(0,index).split('\n').length}: physical spacing: ${property}`);
   continue;
  }
  const color=property==='color'||property.endsWith('-color')||/^(?:background(?:-image)?|border(?:-(?:top|right|bottom|left|block|inline)(?:-(?:start|end))?)?|border-image(?:-source)?|outline|(?:box|text)-shadow|fill|stroke)$/.test(property);
  if(color) {
   if(!tokenColor(value)) errors.push(`${path}:${text.slice(0,index).split('\n').length}: non-token ${property}: ${value}`);
   continue;
  }
  if(!/^(?:(?:margin|padding)(?:-(?:inline|block)(?:-start|-end)?)?|gap|row-gap|column-gap|font-size|border-radius)$/.test(property)) continue;
  // Token references and CSS keywords are valid; calc/min/max/clamp may only derive sizes from tokens.
  const withoutVars=value.replace(/(?:var|env)\([^)]*\)/g,'TOKEN');
  const valid=/^(?:0|auto|inherit|initial|unset|revert|normal|none|transparent|currentColor)$/.test(value)
    || (/TOKEN/.test(withoutVars) && !/(?:#[\da-f]|\b\d+(?:\.\d+)?\s*(?:%|px|rem|em|vh|vw)|\b(?:red|blue|white|black)\b)/i.test(withoutVars));
  if(!valid) errors.push(`${path}:${text.slice(0,index).split('\n').length}: non-token ${property}: ${value}`);
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
