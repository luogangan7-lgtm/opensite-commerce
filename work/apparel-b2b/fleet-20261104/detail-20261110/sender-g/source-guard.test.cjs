// Guard boundary regression; expanded below with owned HTTP/browser controls.
const {test}=require('node:test');
const assert=require('node:assert/strict');
const {one}=require('./gen.cjs');
test('default profile refuses before browser',async()=>{
 await assert.rejects(one({}, {prompt_send:'Reviewed prompt'}),/PROFILE_UNKNOWN/);
});
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
const {fixture,image}=require('./local-fixture.cjs');
const {localFixtureProfile}=require('./source-guard.cjs');
const hash=b=>crypto.createHash('sha256').update(b).digest('hex');
const prompt='Generate a square fabric detail from the second reference.';
// Installed test-only runtime; CLI/card cannot supply a browser connection.
const {chromium}=require(process.env.GUARD_PLAYWRIGHT||'@playwright/test');
const owned=path.join(__dirname,'owned-test');
let browser;
test.before(async()=>{fs.mkdirSync(owned,{recursive:true});fs.writeFileSync(path.join(owned,'whole.png'),image);fs.writeFileSync(path.join(owned,'roi.png'),Buffer.concat([image,Buffer.from('roi')]));browser=await chromium.launch({headless:true,...(process.env.GUARD_BROWSER_CHANNEL?{channel:process.env.GUARD_BROWSER_CHANNEL}:{})})});
test.after(async()=>{await browser?.close();fs.rmSync(owned,{recursive:true,force:true})});
function job(){return {id:'detail/test/fabric',refs:['whole.png','roi.png'].map(x=>path.join(owned,x)),prompt_send:prompt,prompt:'ignored HOLD',reviewed_prompt_sha256:hash(prompt),source_sha256:hash(image),roi_sha256:hash(Buffer.concat([image,Buffer.from('roi')]))}}
for(const [mode,pattern,clicks] of [['G01',null,1],['A01',/SOURCE_ACK_MISMATCH/,0],['A02',/SOURCE_ACK_MISMATCH|UPLOAD_ORDER/,0],['A03',/STALE_ATTACHMENT_ACK/,0],['A04',/UPLOAD_SOURCE_ACK_UNKNOWN|UPLOAD_ORDER/,0],['A05',/UNKNOWN_COMPOSER_TEXT/,0],['A06',/SEND_BOUNDARY_CHANGED/,0],['A06-late',/SEND_BOUNDARY_CHANGED/,0],['A09',/SELECTOR_UNKNOWN/,0],['P01',/NO_CURRENT_OUTPUT/,1],['P02',/POST_SUBMIT_REFS_MISMATCH/,1],['P03',/POST_SUBMIT_REFS_MISMATCH/,1],['P04',/fetch|Failed|DOWNLOAD/i,1],['P04-identity',/DOWNLOAD_OWNERSHIP_UNKNOWN/,1]]){
 test(mode+' real File HTTP DOM download',async()=>{
  const app=await fixture(mode),page=await browser.newPage();
  try{
   await page.goto(app.origin);const options={profile:localFixtureProfile(app.origin)};
   let result,error;try{result=await one(page,job(),options)}catch(e){error=e}
   if(mode==='P04-identity')assert.deepEqual(error?.quarantine,app.image);
   if(pattern){assert.match(error?.message||'',pattern);if(clicks){assert.equal(error.outcome,'UNKNOWN');assert.equal(error.retry,0)}}
   else{assert.ifError(error);assert.deepEqual(result.bytes,app.image);assert.equal(result.sha256,hash(app.image));assert.deepEqual(result.evidence.posted.refs,app.ids);assert.equal(result.evidence.posted.prompt,prompt)}
   assert.equal(await page.evaluate(()=>window.clicks),clicks);
   assert.equal(app.raw.filter(r=>r.route==='/submit').length,clicks);
   if(mode==='G01'){
    const uploads=app.raw.filter(r=>r.route==='/upload');assert.equal(uploads.length,2);
    assert.deepEqual(uploads.map(r=>hash(Buffer.from(r.body,'base64'))),[job().source_sha256,job().roi_sha256]);
   }
   console.log(JSON.stringify({mode,raw:app.raw,events:await page.evaluate(()=>window.events),clicks,error:error?.message,download_sha256:result?.sha256}));
  }finally{await page.close();await app.close()}
 });
}
test('A07 canonical reviewed hash; A08 job.prompt bypass and JSON profile rejected before browser',async()=>{
 const {prepareJobs,run}=require('./gen.cjs');const table=path.join(owned,'slots.tsv');fs.writeFileSync(table,'slot_id\tprompt_send\ndetail/test/fabric\t'+prompt+'\n');
 const j=prepareJobs([job()],table)[0];assert.equal(j.prompt,prompt);
 let opened=0,reserved=0;
 const profile=localFixtureProfile('http://127.0.0.1:32101');
 await assert.rejects(run([{...j,reviewed_prompt_sha256:hash('another legal prompt')}],{slotsFile:table,profile,reserve:()=>reserved++,newPage:()=>opened++}),/REVIEWED_PROMPT_MISMATCH/);
 assert.equal(opened,0);assert.equal(reserved,0);
 await assert.rejects(one({},j,{profile:{passed:true}}),/PROFILE_UNKNOWN/);
 await assert.rejects(one({}, {...j,beforeSend:()=>{}},{profile}),/CARD_FIELD_FORBIDDEN/);
});
test('G01 run keeps canonical ledger and reservation consumed on late-click UNKNOWN',async()=>{
 const {run}=require('./gen.cjs');const app=await fixture('P03'),dir=path.join(owned,'run');fs.mkdirSync(dir);
 const consumed=path.join(dir,'consumed.json'),table=path.join(dir,'slots.tsv');fs.writeFileSync(consumed,'{"exact_ids":[]}');fs.writeFileSync(table,'slot_id\tprompt_send\ndetail/test/fabric\t'+prompt+'\n');
 let reservations=0;
 try{
 const result=await run([job()],{slotsFile:table,dir,consumed,profile:localFixtureProfile(app.origin),checkSlot:()=>{},reserve:()=>reservations++,newPage:async()=>{const p=await browser.newPage();await p.goto(app.origin);return p}});
 assert.equal(result,2);assert.equal(reservations,1);
 const rows=fs.readFileSync(path.join(dir,'ledger.jsonl'),'utf8').trim().split('\n').map(JSON.parse);assert.equal(rows[0].status,'started');assert.equal(rows[1].status,'UNKNOWN');
 assert.equal(app.raw.filter(r=>r.route==='/submit').length,1);
 // The actual ledger suppresses another attempt; no callback is invoked.
 assert.equal(await run([job()],{slotsFile:table,dir,consumed,profile:localFixtureProfile(app.origin),reserve:()=>{throw Error('RETRY')},newPage:()=>{throw Error('RETRY')}}),0);
 }finally{await app.close()}
});
