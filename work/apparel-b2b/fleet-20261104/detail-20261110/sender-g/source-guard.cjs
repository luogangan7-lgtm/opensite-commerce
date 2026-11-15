// Ephemeral source/DOM/network binding for one owned loopback fixture exchange.
// Depends on Node fs/crypto and Playwright Page; called by gen.one/run and tests.
// No production profile, selector injection, callback authority or persistent attempt state.
const fs=require('node:fs'),crypto=require('node:crypto');
const hash=b=>crypto.createHash('sha256').update(b).digest('hex');
const profiles=new WeakMap(), prepared=new WeakMap();
function localFixtureProfile(origin){
 const u=new URL(origin);
 if(u.protocol!=='http:'||u.hostname!=='127.0.0.1'||!u.port||u.pathname!=='/'||u.search||u.hash||u.username||u.password)throw Error('PROFILE_UNKNOWN');
 const handle=Object.freeze({});profiles.set(handle,u.origin);return handle;
}
function assertProfile(profile){if(!profile||!profiles.has(profile))throw Error('PROFILE_UNKNOWN');return profiles.get(profile)}
function preflight(job,profile){
 assertProfile(profile);
 if(['beforeSend','selector','selectors','callback','endpoint','stateFile','dir'].some(k=>k in job))throw Error('CARD_FIELD_FORBIDDEN');
 if(typeof job.prompt_send!=='string'||hash(job.prompt_send)!==job.reviewed_prompt_sha256)throw Error('REVIEWED_PROMPT_MISMATCH');
 if(!Array.isArray(job.refs)||job.refs.length!==2)throw Error('SOURCE_ACK_MISMATCH');
 const bytes=job.refs.map(p=>fs.readFileSync(p)), hashes=bytes.map(hash);
 if(hashes[0]!==job.source_sha256||hashes[1]!==job.roi_sha256)throw Error('SOURCE_ACK_MISMATCH');
 const h=Object.freeze({});prepared.set(h,{bytes,hashes,prompt:job.prompt_send});return h;
}
async function execute(page,handle,profile){
 const origin=assertProfile(profile), expected=prepared.get(handle);if(!expected)throw Error('INVALID_HANDLE');
 prepared.delete(handle); // single use even on failure
 if(new URL(page.url()).origin!==origin)throw Error('PROFILE_UNKNOWN');
 const traffic=[],pending=[],requests=new WeakSet();
 const requestListener=req=>{if(req.frame()===page.mainFrame())requests.add(req)};
 page.on('request',requestListener);
 const responseListener=response=>{
  const req=response.request(); if(!requests.has(req))return; const u=new URL(req.url());
  if(u.origin!==origin||!['/upload','/submit','/output'].includes(u.pathname))return;
  const raw=req.postDataBuffer();
  const p=(async()=>{const body=await response.body();traffic.push({path:u.pathname,status:response.status(),request:raw,response:body,url:response.url()})})().catch(e=>{traffic.push({error:e.message})});
  pending.push(p);
 };
 page.on('response',responseListener);
 let clicked=false;
 const wall=setTimeout(()=>{void page.close().catch(()=>{})},15000);
 try{
  // Retained handles live in one JS closure; no JSON observation can mint them.
  const session=await page.evaluateHandle(()=>{
   if(window.frames.length)throw Error('SELECTOR_UNKNOWN');
   if(document.documentElement.lang!=='en'||document.body.dataset.profile!=='owned-loopback-v1')throw Error('SELECTOR_UNKNOWN');
   const one=s=>{const all=document.querySelectorAll(s);if(all.length!==1)throw Error('SELECTOR_UNKNOWN');return all[0]};
   const form=one('form#composer'),input=one('#refs'),editor=one('#prompt'),button=one('#send');
   if(input.form!==form||editor.form!==form||button.form!==form||input.type!=='file'||button.type!=='button')throw Error('SELECTOR_UNKNOWN');
   if(editor.value||form.querySelector('[data-asset]')||document.querySelector('[data-turn]'))throw Error('STALE_ATTACHMENT_ACK');
   const s={form,input,editor,button,changes:0,files:null,poison:false};
   const digest=async b=>Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',b)),x=>x.toString(16).padStart(2,'0')).join('');
   input.addEventListener('change',()=>{s.changes++;s.files=Promise.all([...input.files].map(async f=>({hash:await digest(await f.arrayBuffer()),length:f.size,name:f.name})))},true);
   s.identity=()=>form.isConnected&&input.isConnected&&editor.isConnected&&button.isConnected&&document.querySelector('#composer')===form&&document.querySelector('#refs')===input&&document.querySelector('#prompt')===editor&&document.querySelector('#send')===button;
   return s;
  });
  try{
   const input=await session.evaluateHandle(s=>s.input);
   await input.asElement().setInputFiles(expected.bytes.map((buffer,i)=>({name:`ref-${i}.png`,mimeType:'image/png',buffer})));
   await page.waitForFunction(()=>document.querySelector('#composer')?.dataset.ready==='yes',{},{timeout:4000});
   await Promise.all(pending);
   const local=await session.evaluate(async s=>({identity:s.identity(),changes:s.changes,files:await s.files,cards:[...s.form.querySelectorAll('[data-asset]')].map(e=>e.dataset.asset)}));
   if(!local.identity||local.changes!==1||local.files?.length!==2)throw Error('STALE_ATTACHMENT_ACK');
   if(local.files.some((f,i)=>f.hash!==expected.hashes[i]))throw Error('UPLOAD_ORDER');
   const uploads=traffic.filter(x=>x.path==='/upload');
   if(uploads.length!==2)throw Error('UPLOAD_SOURCE_ACK_UNKNOWN');
   const ids=uploads.map((r,i)=>{
    if(r.status!==200||!r.request||hash(r.request)!==expected.hashes[i])throw Error('SOURCE_ACK_MISMATCH');
    const ack=JSON.parse(r.response);
    if(typeof ack.asset_id!=='string'||!ack.asset_id||ack.ordinal!==i)throw Error('UPLOAD_SOURCE_ACK_UNKNOWN');return ack.asset_id;
   });
   if(new Set(ids).size!==2||JSON.stringify(ids)!==JSON.stringify(local.cards))throw Error('UPLOAD_ORDER');
   await session.evaluate((s,{prompt,ids})=>{
    if(!s.identity()||s.editor.value)throw Error('UNKNOWN_COMPOSER_TEXT');
    s.editor.focus();
    if(!s.identity()||s.editor.value)throw Error('UNKNOWN_COMPOSER_TEXT');
    s.editor.value=prompt;s.editor.dispatchEvent(new Event('input',{bubbles:true}));
    if(!s.identity()||s.editor.value!==prompt)throw Error('SEND_BOUNDARY_CHANGED');
    s.ids=ids;s.prompt=prompt;s.cards=[...s.form.querySelectorAll('[data-asset]')];
    s.observer=new MutationObserver(()=>{s.poison=true});s.observer.observe(s.form,{subtree:true,childList:true,attributes:true});
   },{prompt:expected.prompt,ids});
   // No await between the final retained-node/text/card check and the DOM action.
   await session.evaluate(s=>{
    const cards=[...s.form.querySelectorAll('[data-asset]')];
    if(s.poison||s.observer.takeRecords().length||!s.identity()||s.changes!==1||s.editor.value!==s.prompt||cards.length!==2||cards.some((e,i)=>e!==s.cards[i]||e.dataset.asset!==s.ids[i])||s.button.disabled||!s.button.getClientRects().length)throw Error('SEND_BOUNDARY_CHANGED');
    const r=s.button.getBoundingClientRect();if(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2)!==s.button)throw Error('SELECTOR_UNKNOWN');
    s.button.click();
   });clicked=true;
   await page.waitForFunction(()=>document.querySelector('[data-complete="yes"]'),{},{timeout:4000});
   await Promise.all(pending);
   const submits=traffic.filter(x=>x.path==='/submit');
   if(submits.length!==1||submits[0].status!==200)throw Error('POST_SUBMIT_UNKNOWN');
   const sent=JSON.parse(submits[0].request), exchange=JSON.parse(submits[0].response);
   if(sent.prompt!==expected.prompt||JSON.stringify(sent.refs)!==JSON.stringify(ids))throw Error('POST_SUBMIT_REFS_MISMATCH');
   if(!exchange.user_id||!exchange.assistant_id||!exchange.turn_id||exchange.parent_id!==exchange.user_id||!exchange.asset_id)throw Error('NO_CURRENT_OUTPUT');
   const output=await page.evaluateHandle(e=>{
    const turns=[...document.querySelectorAll('[data-turn]')];
    if(turns.length!==1)throw Error('NO_CURRENT_OUTPUT');
    const t=turns[0],u=t.querySelector('[data-role="user"]'),a=t.querySelector('[data-role="assistant"]'),ims=a?.querySelectorAll('img');
    if(t.dataset.turn!==e.turn_id||u?.dataset.id!==e.user_id||a?.dataset.id!==e.assistant_id||a.dataset.parent!==e.user_id||ims?.length!==1||ims[0].dataset.asset!==e.asset_id)throw Error('NO_CURRENT_OUTPUT');
    return {t,u,a,im:ims[0],e};
   },exchange);
   const downloaded=await output.evaluate(async s=>{
    const same=()=>s.t.isConnected&&s.a.isConnected&&s.im.isConnected&&s.t.dataset.turn===s.e.turn_id&&s.a.dataset.id===s.e.assistant_id&&s.a.dataset.parent===s.e.user_id&&s.im.dataset.asset===s.e.asset_id&&s.a.querySelectorAll('img').length===1;
    if(!same())throw Error('DOWNLOAD_IDENTITY');
    const url=new URL(s.im.src);if(url.origin!==location.origin||url.pathname!=='/output')throw Error('DOWNLOAD_IDENTITY');
    const response=await fetch(url,{redirect:'error'}),bytes=new Uint8Array(await response.arrayBuffer());
    return {bytes:[...bytes],valid:same()&&s.im.src===url.href,status:response.status,type:response.headers.get('content-type')};
   });
   const bytes=Buffer.from(downloaded.bytes);
   if(!downloaded.valid||downloaded.status!==200||!downloaded.type?.startsWith('image/')||!bytes.length){const e=Error('DOWNLOAD_OWNERSHIP_UNKNOWN');e.quarantine=bytes;throw e}
   return {bytes,type:downloaded.type,sha256:hash(bytes),url:origin,evidence:{file:local.files,assets:ids,exchange,posted:sent,download_sha256:hash(bytes)}};
  }finally{await session.evaluate(s=>s.observer?.disconnect()).catch(()=>{});await session.dispose()}
 }catch(e){e.clicked=clicked;e.retry=0;if(clicked)e.outcome='UNKNOWN';throw e}
 finally{clearTimeout(wall);page.off('request',requestListener);page.off('response',responseListener);await Promise.all(pending)}
}
module.exports={localFixtureProfile,assertProfile,preflight,execute};
