/** Stable served paths shared by picture markup and deploy packaging; external review data stays external. */
export const widths = [640, 1024, 1600] as const;
export const publicId = (id: string) => id.replaceAll('aston-hoodie', 'pullover-hoodie');
export const imageBase = (id: string) => `/img/site/illustrative_${publicId(id).replace('/', '_')}`;
export const imageSrcset = (id: string, format: string) => widths.map(w => `${imageBase(id)}-${w}.${format} ${w}w`).join(', ');
export const heroSizes = '100vw';
