/** Gallery role adapter for Product; only colour-bound accepted roles enter the PDP sequence. */
import { dictionaries, locales, type Locale } from '../i18n';
export type GalleryRole = 'front' | 'turn' | 'back' | 'garment' | 'print' | 'structure' | 'fabric';
export interface GalleryImage { src: string; role?: string; colour?: string }
const order: Record<GalleryRole, number> = { front: 0, turn: 1, back: 2, garment: 3, print: 4, structure: 5, fabric: 6 };
export const isDetail = (image: GalleryImage) => ['structure', 'fabric', 'print'].includes(image.role || '');
export const isGeneratedDetail = (image: GalleryImage) => ['structure', 'fabric'].includes(image.role || '');
export const primaryColour = (images: GalleryImage[]) => images.find(image => image.role === 'front' && image.colour)?.colour;
export function orderedGallery<T extends GalleryImage>(images: T[], selected = primaryColour(images), primary = primaryColour(images)): T[] {
  const seen = new Set<string>();
  return images.filter(image => {
    if (!selected || image.colour !== selected || !Object.hasOwn(order, image.role || '')) return false;
    if (selected !== primary && image.role !== 'front') return false;
    if (image.role !== 'print' && seen.has(image.role!)) return false;
    seen.add(image.role!); return true;
  }).sort((a,b) => order[a.role as GalleryRole] - order[b.role as GalleryRole]);
}
export function galleryColours<T extends GalleryImage>(images: T[]): T[] {
  const seen = new Set<string>();
  return images.filter(image => {
    if (image.role !== 'front' || !image.colour || seen.has(image.colour)) return false;
    seen.add(image.colour); return true;
  });
}
/** Frozen FD1 pattern: primary accepted W1 front plus same-colour G structure (fabric fallback). */
export function pdpMediaPair<T extends GalleryImage>(images: T[]): { whole: T; detail: T } {
 const whole = images.find(image => image.role === 'front' && image.colour === primaryColour(images));
 const detail = images.find(image => image.role === 'structure' && image.colour === whole?.colour)
  || images.find(image => image.role === 'fabric' && image.colour === whole?.colour);
 if (!whole || !detail) throw Error('PDP requires an accepted whole garment and same-colour G detail');
 return { whole, detail };
}
/** Compatibility exports for existing gallery callers; visible copy is owned by the dictionaries. */
export const galleryClose = Object.fromEntries(locales.map(locale => [locale, dictionaries[locale].galleryClose])) as Record<Locale, string>;
