// Offline fault injection only: no browser, no generated images.
const fs=require('fs'),p=require('path'),c=require('crypto'),assert=require('assert'),{makeGate,slot}=require('./attempt-gate.cjs');
const O=p.join(__dirname,'offline-fixture');fs.mkdirSync(O,{recursive:true});const h=b=>c.createHash('sha256').update(b).digest('hex');
fs.writeFileSync(p.join(O,'source'),'whole');fs.writeFileSync(p.join(O,'roi'),'native ROI');fs.writeFileSync(p.join(O,'consumed.json'),JSON.stringify({exact_ids:['detail/old/fabric']}));fs.writeFileSync(p.join(O,'history.json'),JSON.stringify({counts:{'old/fabric':1}}));
let seq=0;const options=()=>({stateFile:p.join(O,'state-'+(++seq)+'.json'),consumed:p.join(O,'consumed.json'),history:p.join(O,'history.json')});
const job=id=>({id,style_slug:id.split('/')[1],refs:[p.join(O,'source'),p.join(O,'roi')],source_sha256:h('whole'),roi_sha256:h('native ROI'),prompt:'detail'});
const card=(id,jobs,max=10)=>({card_id:id,max_calls:max,deadline_epoch:Date.now()/1000+3600,jobs:jobs.map(j=>({...j,slot:slot(j),status:'READY',prompt_sha256:h(j.prompt)}))});
const results=[];function red(name,fn,pattern){assert.throws(fn,pattern);results.push({name,status:'RED',reason:String(pattern)})}
// clean fixture files make repeat executions deterministic; real state never touched.
for(const f of fs.readdirSync(O))if(/^state-\d+\.json/.test(f))fs.rmSync(p.join(O,f),{recursive:true,force:true});
for(const type of ['structure','fabric']){
 const a=job('detail/test/'+type),b=job('detail/test/'+type+'-supplement-2'),d=job('detail/test/'+type+'-supplement-3'),o=options();
 makeGate(card('A',[a]),o).reserve(a);makeGate(card('B',[b]),o).reserve(b);
 red('third_cross_batch_'+type,()=>makeGate(card('C',[d]),o).reserve(d),/CROSS_BATCH_ATTEMPT_LIMIT/);
 red('exact_id_'+type,()=>makeGate(card('A',[a]),o).reserve(a),/EXACT_ID_CONSUMED/);
}
const a=job('detail/test/structure'),b=job('detail/other/fabric'),o=options();let g=makeGate(card('budget',[a,b],1),o);g.reserve(a);red('card_budget',()=>g.reserve(b),/CARD_BUDGET/);
const repaired=makeGate(card('fresh',[b],1),o);repaired.reserve(b);results.push({name:'fresh_card_repaired',status:'GREEN'});
let j=job('detail/test/fabric');g=makeGate(card('binding',[j]),options());red('wrong_style',()=>g.reserve({...j,style_slug:'wrong'}),/SOURCE_STYLE_BINDING/);red('wrong_source',()=>g.reserve({...j,source_sha256:'bad'}),/SOURCE_STYLE_BINDING/);red('wrong_attachments',()=>g.reserve({...j,refs:[...j.refs,'pose']}),/SOURCE_STYLE_BINDING/);g.reserve(j);results.push({name:'binding_repaired',status:'GREEN'});
j=job('detail/apparel-7203/fabric');red('7203',()=>makeGate(card('q',[j]),options()).reserve(j),/7203_QUARANTINE/);
const pins={};for(const f of ['gen.cjs','run.cjs','attempt-gate.cjs','test-offline.cjs'])pins[f]=h(fs.readFileSync(p.join(__dirname,f)));
fs.writeFileSync(p.join(__dirname,'offline-results.json'),JSON.stringify({assertions_pass:true,generation_calls:0,pins,results},null,2));console.log(JSON.stringify({pass:true,tests:results.length}));
