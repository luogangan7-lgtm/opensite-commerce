/** Stable served paths shared by picture markup and deploy packaging; external review data stays external. */
export const widths = [640, 1024, 1600] as const;
export const publicId = (id: string) => id.replaceAll('aston-hoodie', 'pullover-hoodie');
export const imageBase = (id: string) => `/img/site/illustrative_${publicId(id).replace('/', '_')}`;
export const imageSrcset = (id: string, format: string) => widths.map(w => `${imageBase(id)}-${w}.${format} ${w}w`).join(', ');
export const heroSizes = '100vw';

/** Reviewed model metadata only. Source paths and review receipts stay in the packaging module. */
import digital from './digital-imagery.json';
import type { Locale } from '../i18n';
export const digitalImages = digital;
const vocabulary: Record<Locale, { label: string; colors: string[]; garments: string[]; views: string[] }> = {
  en: { label: 'Digital model illustration', colors: ['black','white','cream','grey','grey-green','brown','beige','navy','pink','purple','red','burgundy','charcoal','green'], garments: ['T-shirt','sweatshirt','hoodie','hoodie and sweatpants set','zip jacket'], views: ['front view','three-quarter side view','back view'] },
  zh: { label: '数字模特示意', colors: ['黑色','白色','奶油色','灰色','灰绿色','棕色','米色','藏青色','粉色','紫色','红色','酒红色','炭灰色','绿色'], garments: ['T恤','卫衣','连帽卫衣','连帽卫衣及运动裤套装','拉链夹克'], views: ['正面','侧转','背面'] },
  es: { label: 'Ilustración con modelo digital', colors: ['negro','blanco','crema','gris','verde grisáceo','marrón','beige','azul marino','rosa','morado','rojo','burdeos','gris carbón','verde'], garments: ['camiseta','sudadera','sudadera con capucha','conjunto de sudadera con capucha y pantalón','chaqueta con cremallera'], views: ['vista frontal','vista lateral de tres cuartos','vista posterior'] },
  fr: { label: 'Illustration avec mannequin numérique', colors: ['noir','blanc','crème','gris','vert grisé','marron','beige','bleu marine','rose','violet','rouge','bordeaux','gris anthracite','vert'], garments: ['T-shirt','sweat-shirt','sweat à capuche','ensemble sweat à capuche et pantalon','veste zippée'], views: ['vue de face','vue de trois quarts latérale','vue de dos'] },
  de: { label: 'Digitale Modelldarstellung', colors: ['Schwarz','Weiß','Creme','Grau','Graugrün','Braun','Beige','Marineblau','Rosa','Violett','Rot','Burgunderrot','Anthrazit','Grün'], garments: ['T-Shirt','Sweatshirt','Kapuzenpullover','Set aus Kapuzenpullover und Jogginghose','Jacke mit Reißverschluss'], views: ['Vorderansicht','seitliche Dreiviertelansicht','Rückansicht'] },
  pt: { label: 'Ilustração com modelo digital', colors: ['preto','branco','creme','cinza','verde acinzentado','marrom','bege','azul-marinho','rosa','roxo','vermelho','bordô','cinza-carvão','verde'], garments: ['camiseta','moletom','moletom com capuz','conjunto de moletom com capuz e calça','jaqueta com zíper'], views: ['vista frontal','vista lateral em três quartos','vista de costas'] },
  ru: { label: 'Иллюстрация с цифровой моделью', colors: ['чёрный','белый','кремовый','серый','серо-зелёный','коричневый','бежевый','тёмно-синий','розовый','фиолетовый','красный','бордовый','угольный','зелёный'], garments: ['футболка','свитшот','худи','комплект худи и спортивных брюк','куртка на молнии'], views: ['вид спереди','вид сбоку в три четверти','вид сзади'] },
  ar: { label: 'رسم توضيحي بعارض رقمي', colors: ['أسود','أبيض','كريمي','رمادي','أخضر رمادي','بني','بيج','كحلي','وردي','بنفسجي','أحمر','عنابي','فحمي','أخضر'], garments: ['قميص تي شيرت','سويت شيرت','سترة بغطاء رأس','طقم سترة بغطاء رأس وبنطال رياضي','سترة بسحاب'], views: ['منظر أمامي','منظر جانبي بزاوية ثلاثة أرباع','منظر خلفي'] },
};
export const digitalLabel = (locale: Locale) => vocabulary[locale].label;
export function digitalAlt(src: string, locale: Locale): string | undefined {
  const image = digital.find(row => row.src === src);
  if (!image) return undefined;
  const t = vocabulary[locale], color = t.colors['kwcgobtnpvruhe'.indexOf(image.color)], garment = t.garments[['tee','sweatshirt','hoodie','set','jacket'].indexOf(image.garment)], view = t.views[['front','turn','back'].indexOf(image.view)];
  if (!color || !garment || !view) throw Error(`Invalid digital image description: ${src}`);
  return `${t.label}: ${color}; ${garment}; ${view}`;
}
