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
 if(width>=768)await page.locator('.gallery-thumbs:not([hidden]) a[data-role=back]').click();await expect(main).toHaveAttribute('src',image('front'));await expect(back).toHaveAttribute('src',image('back'));
 const tabs=page.locator('[data-gallery-view]');
 const widths=await tabs.evaluateAll(nodes=>nodes.map(n=>n.getBoundingClientRect().width));expect(Math.max(...widths)-Math.min(...widths)).toBeLessThanOrEqual(1);
 for(const tab of await tabs.all()){await expect(tab).toHaveAccessibleName(/.+/);expect((await tab.boundingBox())!.height).toBeGreaterThanOrEqual(44)}
 const captions=await page.locator('.gallery-thumbs:not([hidden]) a').evaluateAll(nodes=>nodes.map(n=>{const label=n.querySelector('span')!.getBoundingClientRect(),image=n.querySelector('img')!.getBoundingClientRect();return {labelTop:label.top,imageBottom:image.bottom}}));for(const c of captions)expect(c.labelTop).toBeGreaterThanOrEqual(c.imageBottom);await tabs.nth(1).click();await expect(main).toHaveAttribute('src',image('turn'));await expect(page.locator('[data-gallery-slide=back]')).toBeHidden();
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

// Build the real icon variant with the installed Astro CLI; no mock markup or runtime.
// Shop consumes this variant after PR657, so its standalone production render is the fixture.
import {createRequire} from 'node:module';
import {readFileSync,realpathSync,mkdirSync,mkdtempSync,writeFileSync,symlinkSync,readdirSync,rmSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {dirname,join} from 'node:path';
import {execFileSync} from 'node:child_process';
const dictionaries=Object.fromEntries(['en','zh','ar'].map(locale=>[locale,JSON.parse(readFileSync(new URL(`../src/i18n/${locale}.json`,import.meta.url),'utf8'))]));
const requireAstro=createRequire(realpathSync(new URL('../node_modules/astro/package.json',import.meta.url)));
let fixtureRoot:string;
test.beforeAll(()=>{
 const output=fileURLToPath(new URL('../test-results/',import.meta.url));mkdirSync(output,{recursive:true});fixtureRoot=mkdtempSync(join(output,'view-toggle-fixture-'));
 mkdirSync(join(fixtureRoot,'src/pages'),{recursive:true});writeFileSync(join(fixtureRoot,'package.json'),'{}');writeFileSync(join(fixtureRoot,'astro.config.mjs'),'export default {build:{inlineStylesheets:"never"}}');symlinkSync(fileURLToPath(new URL('../node_modules/',import.meta.url)),join(fixtureRoot,'node_modules'),'dir');
 for(const locale of ['en','zh','ar'])writeFileSync(join(fixtureRoot,`src/pages/${locale}.astro`),`---\nimport ViewToggle from ${JSON.stringify(fileURLToPath(new URL('../src/components/ViewToggle.astro',import.meta.url)))};\n---\n<ViewToggle variant="icon" side="front" target="card-fixture" label={${JSON.stringify(dictionaries[locale].galleryShowBack)}} />`);
 execFileSync(process.execPath,[join(dirname(requireAstro.resolve('astro/package.json')),'bin/astro.mjs'),'build','--root',fixtureRoot],{timeout:60000,stdio:'pipe'});
});
test.afterAll(()=>{if(fixtureRoot)rmSync(fixtureRoot,{recursive:true,force:true})});
function iconFixture(locale:string){return {html:readFileSync(join(fixtureRoot,`dist/${locale}/index.html`),'utf8').replace(/<link\b[^>]*>/g,''),css:readdirSync(join(fixtureRoot,'dist/_astro')).filter(f=>f.endsWith('.css')).map(f=>readFileSync(join(fixtureRoot,'dist/_astro',f),'utf8')).join('\n')}}
for(const locale of ['en','zh','ar'] as const)for(const width of [390,1440])test(`shared ViewToggle icon ${locale} ${width}`,async({page})=>{
 await page.setViewportSize({width,height:900});await page.goto(`${locale==='en'?'':'/'+locale}/products/hoodie-set-k02/`);
 const fixture=iconFixture(locale);
 await page.evaluate(({html,css})=>{const style=document.createElement('style');style.textContent=css;document.head.append(style);const host=document.createElement('div');host.id='icon-fixture';host.style.cssText='position:relative;width:240px;height:100px;';host.innerHTML=html;document.body.prepend(host)},fixture);
 const toggle=page.locator('#icon-fixture [data-view-toggle=icon]');await expect(toggle).toHaveAccessibleName(dictionaries[locale].galleryShowBack);await expect(toggle).toHaveAttribute('data-view-target','card-fixture');
 const box=(await toggle.boundingBox())!,host=(await page.locator('#icon-fixture').boundingBox())!;
 expect(box.width).toBeGreaterThanOrEqual(44);expect(box.height).toBeGreaterThanOrEqual(44);
 expect(locale==='ar'?box.x-host.x:host.x+host.width-box.x-box.width).toBe(8);
 await expect(toggle.locator('span')).toHaveCSS('width','36px');
});
