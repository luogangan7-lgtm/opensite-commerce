/** Digital packaging boundary; W3 preserves reviewed native bytes, W1/W2 retain existing web conversion. */
import {writeFileSync} from 'node:fs';
import {createRequire} from 'node:module';
const sharp=createRequire(import.meta.resolve('astro/package.json'))('sharp');
export async function packageDigital(image:{wave:string;nativeBytes?:boolean},bytes:Uint8Array,output:string){
 if(image.nativeBytes===true) writeFileSync(output,bytes);
 else await sharp(bytes).resize({width:1024,withoutEnlargement:true}).webp({quality:82}).toFile(output);
}
