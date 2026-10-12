// WCAG contrast gate; reads the shared registry in tokens.json; used by CI and tests.
// Every registered pair must resolve to strict opaque hex tokens and satisfy its declared minimum.
import source from '../../packages/design-tokens/tokens.json';
export type ContrastSource = {tokens: Record<string,string>; contrastPairs: {name:string;foreground:string;background:string;minimum:number}[]};
function luminance(hex: string): number {
 if (!/^#[0-9a-f]{6}$/i.test(hex)) throw new Error(`Invalid opaque color: ${hex}`);
 const channels=[1,3,5].map(i=>parseInt(hex.slice(i,i+2),16)/255).map(v=>v<=0.04045?v/12.92:((v+0.055)/1.055)**2.4);
 return channels[0]*0.2126+channels[1]*0.7152+channels[2]*0.0722;
}
export function contrastRatio(a:string,b:string): number {const x=luminance(a),y=luminance(b);return (Math.max(x,y)+0.05)/(Math.min(x,y)+0.05);}
export function checkContrast(input:ContrastSource): string[] {
 if (!input.contrastPairs.length) return ['Missing contrast pair registry'];
 return input.contrastPairs.flatMap(pair=>{
  if (!Number.isFinite(pair.minimum) || pair.minimum<3) return [`${pair.name}: invalid contrast threshold`];
  try {const ratio=contrastRatio(input.tokens[pair.foreground],input.tokens[pair.background]);return ratio<pair.minimum?[`${pair.name}: ${ratio.toFixed(3)} < ${pair.minimum}`]:[];} catch (error) {return [`${pair.name}: ${String(error)}`];}
 });
}
if(import.meta.main) {const errors=checkContrast(source);if(errors.length){console.error(errors.join('\n'));process.exitCode=1;}else console.log(`Contrast PASS (${source.contrastPairs.length} registered pairs)`);}
