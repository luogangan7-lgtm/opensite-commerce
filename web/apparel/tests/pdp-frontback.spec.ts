/** CP-PDP-FB role/colour, native ratios, RTL, keyboard and layout stability on real templates. */
import {test,expect} from '@playwright/test';
import products from '../src/data/products.json' with {type:'json'};
for(const locale of ['en','zh','ar'])for(const width of [1440,768,390])test(`front/back gallery ${locale} ${width}`,async({page},info)=>{
 await page.setViewportSize({width,height:1000});
 await page.addInitScript(()=>{(window as any).galleryCLS=0;new PerformanceObserver(list=>{for(const e of list.getEntries() as any)if(!e.hadRecentInput)(window as any).galleryCLS+=e.value}).observe({type:'layout-shift',buffered:true})});
 await page.goto(`${locale==='en'?'':'/'+locale}/products/hoodie-set-k02/`);
 await page.evaluate(()=>document.fonts.ready);
 const main=page.locator('[data-gallery-main]'),back=page.locator('[data-gallery-back]');
 const product=products.find(p=>p.slug==='hoodie-set-k02')!;
 const primary=product.images.find(i=>i.role==='front')!.colour;
 const image=(role:string)=>product.images.find(i=>i.colour===primary&&i.role===role)!.src;
 await expect(main).toHaveAttribute('src',image('front'));await expect(back).toHaveAttribute('src',image('back'));
 for(const img of [main,back]){const box=(await img.boundingBox())!;expect(Math.abs(box.width/box.height/.8-1)).toBeLessThanOrEqual(.02);await expect(img).toHaveCSS('object-position','50% 0px')}
 if(width>=768){const a=(await main.boundingBox())!,b=(await back.boundingBox())!;expect(locale==='ar'?a.x>b.x:a.x<b.x).toBe(true)}
 await page.locator('.gallery-thumbs:not([hidden]) a[data-role=back]').click();await expect(main).toHaveAttribute('src',image('front'));await expect(back).toHaveAttribute('src',image('back'));
 const tabs=page.locator('[data-gallery-view]');await tabs.nth(1).click();await expect(main).toHaveAttribute('src',image('turn'));await expect(page.locator('[data-gallery-slide=back]')).toBeHidden();
 await tabs.nth(2).click();await expect(main).toHaveAttribute('src',image('structure'));
 await tabs.first().focus();await page.keyboard.press(locale==='ar'?'ArrowLeft':'ArrowRight');await expect(tabs.nth(1)).toBeFocused();await expect(tabs.nth(1)).toHaveAttribute('aria-selected','true');
 const swatch=page.locator('[data-gallery-swatch]').nth(1);await swatch.click();
 const colour=await swatch.getAttribute('data-gallery-swatch');await expect(main).toHaveAttribute('src',product.images.find(i=>i.role==='front'&&i.colour===colour)!.src);
 await expect(back).not.toHaveAttribute('src',/.+/);await expect(page.locator('[data-view-unavailable]')).toBeVisible();
 for(const role of ['turn','structure','fabric']){const tile=page.locator(`[data-gallery-tile=${role}]`);await expect(tile).toBeHidden();await expect(tile.locator('img')).not.toHaveAttribute('src',/.+/)}
 await page.locator('[data-gallery-swatch]').first().click();await expect(back).toHaveAttribute('src',image('back'));
 await tabs.nth(2).click();await expect(main).toHaveAttribute('src',image('structure'));
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
 const cls=await page.evaluate(()=>(window as any).galleryCLS);expect(cls).toBeLessThanOrEqual(.05);await info.attach('CLS',{body:JSON.stringify({locale,width,cls}),contentType:'application/json'});
});
