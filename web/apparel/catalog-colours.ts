/** Catalog audit adapter; slash strings are colour lists, view annotations are not front colours. */
export function catalogColours(value:string|string[]):string[]{
 const colours=typeof value==='string'?value.split('/'):value;
 return [...new Set(colours.map(c=>c.trim()).filter(c=>c&&!/\bback view\b/i.test(c)))];
}
