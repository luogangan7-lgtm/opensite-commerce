/** Digital delivery derivatives; called by copy-images and tested with accepted SHA-bound masters. */
import {mkdirSync, readFileSync, realpathSync} from 'node:fs';
import {dirname, join, resolve} from 'node:path';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
import { digitalDelivery } from './image-plan';
const sharp=createRequire(import.meta.resolve('astro/package.json'))('sharp');
/** Preserve masters; delivery WebP candidates may shrink to 640/1024px and must never enlarge. */
export async function packageDigital(image: {source:string;src:string;sha256:string}, sourceRoot:string, destination:string) {
 const input=realpathSync(resolve(sourceRoot,image.source));
 if(!input.startsWith(realpathSync(sourceRoot)+'/')) throw Error('Source escaped approved directory');
 const bytes=readFileSync(input);
 if(createHash('sha256').update(bytes).digest('hex')!==image.sha256) throw Error('Digital source digest mismatch');
 const output=join(destination,image.src);
 mkdirSync(dirname(output),{recursive:true});
 for (const delivery of digitalDelivery(image)) await sharp(bytes).resize({width:delivery.width,withoutEnlargement:true}).webp({quality:82}).toFile(join(destination,delivery.src));
 return output;
}
