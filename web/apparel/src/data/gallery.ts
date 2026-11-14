/** Gallery role adapter for Product; only colour-bound accepted roles enter the PDP sequence. */
import type { Locale } from '../i18n';
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
/** Owner review pending for non-Chinese translations; never claim technical properties in generated alt. */
export const detailCaption: Record<Locale, string> = {
 en: 'AI-generated illustration; actual details are subject to the confirmed sample.',
 zh: 'AI生成示意，实际细节以确认样品为准',
 es: 'Ilustración generada por IA; los detalles reales dependen de la muestra confirmada.',
 fr: 'Illustration générée par IA ; les détails réels sont ceux de l’échantillon confirmé.',
 de: 'KI-generierte Illustration; maßgeblich sind die tatsächlichen Details des bestätigten Musters.',
 pt: 'Ilustração gerada por IA; os detalhes reais dependem da amostra confirmada.',
 ru: 'Иллюстрация создана ИИ; фактические детали определяются подтверждённым образцом.',
 ar: 'رسم توضيحي مولّد بالذكاء الاصطناعي؛ التفاصيل الفعلية وفق العينة المعتمدة.',
};

export const galleryClose: Record<Locale, string> = { en: 'Close', zh: '关闭', es: 'Cerrar', fr: 'Fermer', de: 'Schließen', pt: 'Fechar', ru: 'Закрыть', ar: 'إغلاق' };
