// G sending gate: card-bound source/ROI and exact ID; shared cross-batch counters.
// Called only by run/gen. Old markers/consumed IDs/quarantine are read, never rewritten.
const fs=require('fs'),path=require('path'),crypto=require('crypto');
const O='/Volumes/data/OpenSite Commerce/work/apparel-b2b/fleet-20261104/detail-20261110/sender-g', STATE=path.join(O,'send-reservations.json');
const hash=p=>crypto.createHash('sha256').update(fs.readFileSync(p)).digest('hex');
const slot=j=>{const m=/^detail\/([a-z0-9-]+)\/(structure|fabric)(?:-supplement[\w-]*)?$/.exec(j.id||'');if(!m)throw Error('INVALID_SLOT');return m[1]+'/'+m[2]};
function makeGate(auth, options={}){
 if(!options.stateFile && __dirname!==O)throw Error('NONCANONICAL_SENDER');
 const stateFile=options.stateFile||STATE; // production entry never supplies override
 if(!Number.isInteger(auth.max_calls)||auth.max_calls<1||!auth.card_id||!Number.isFinite(auth.deadline_epoch)||!Array.isArray(auth.jobs))throw Error('INVALID_CARD');
 const consumed=JSON.parse(fs.readFileSync(options.consumed||path.join(O,'../../wash-consumed-exact-ids.json'),'utf8'));
 if(!Array.isArray(consumed.exact_ids))throw Error('INVALID_CONSUMED');
 const old=JSON.parse(fs.readFileSync(options.history||path.join(O,'../../w3-b7-20261108/historical-attempts.json'),'utf8'));
 const load=()=>{if(options.requireExisting&&!fs.existsSync(stateFile))throw Error('STATE_UNKNOWN');return fs.existsSync(stateFile)?JSON.parse(fs.readFileSync(stateFile,'utf8')):{counts:{...old.counts},cards:{},exact_ids:[]}};
 function check(j,s){
  const k=slot(j);if(k.startsWith('apparel-7203/'))throw Error('7203_QUARANTINE');
  if(options.requireExisting&&(!Number.isInteger(s.counts[k])||!Number.isInteger(old.counts[k])||s.counts[k]<old.counts[k]))throw Error('HISTORY_INCONSISTENT');
  if(Date.now()>auth.deadline_epoch*1000-250000)throw Error('DEADLINE');
  const a=auth.jobs.find(x=>x.id===j.id);
  if(!a||a.slot!==k||a.style_slug!==k.split('/')[0]||a.status!=='READY'||j.style_slug!==a.style_slug||j.source_sha256!==a.source_sha256||j.roi_sha256!==a.roi_sha256||!Array.isArray(j.refs)||j.refs.length!==2||JSON.stringify(j.refs)!==JSON.stringify(a.refs)||hash(j.refs[0])!==a.source_sha256||hash(j.refs[1])!==a.roi_sha256||hashPrompt(j.prompt)!==a.prompt_sha256)throw Error('SOURCE_STYLE_BINDING');
  if(consumed.exact_ids.includes(j.id)||s.exact_ids.includes(j.id))throw Error('EXACT_ID_CONSUMED');
  if((s.counts[k]||0)>=2)throw Error('CROSS_BATCH_ATTEMPT_LIMIT');
  if((s.cards[auth.card_id]||0)>=auth.max_calls)throw Error('CARD_BUDGET');
 }
 function reserve(j){const lock=stateFile+'.lock';fs.mkdirSync(lock);try{const s=load();check(j,s);const k=slot(j);s.counts[k]=(s.counts[k]||0)+1;s.cards[auth.card_id]=(s.cards[auth.card_id]||0)+1;s.exact_ids.push(j.id);s.last_id=j.id;s.time=new Date().toISOString();const tmp=stateFile+'.tmp';const fd=fs.openSync(tmp,'w');try{fs.writeSync(fd,JSON.stringify(s,null,2));fs.fsyncSync(fd)}finally{fs.closeSync(fd)}fs.renameSync(tmp,stateFile);const parent=fs.openSync(path.dirname(stateFile),'r');try{fs.fsyncSync(parent)}finally{fs.closeSync(parent)}}finally{fs.rmdirSync(lock)}}
 return {checkSlot:j=>check(j,load()),reserve};
}
const hashPrompt=s=>crypto.createHash('sha256').update(s||'').digest('hex');
module.exports={slot,makeGate};
