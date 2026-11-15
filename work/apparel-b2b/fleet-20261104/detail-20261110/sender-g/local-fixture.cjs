// Owned loopback application: raw request oracle and server-generated IDs.
// Depends on Node http/crypto; called only by source-guard tests, never production.
const http=require('node:http'),crypto=require('node:crypto');
const image=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=','base64');
async function fixture(mode='good'){
 const raw=[],ids=[];let downloads=0;
 const server=http.createServer(async(req,res)=>{
  const chunks=[];for await(const c of req)chunks.push(c);const body=Buffer.concat(chunks);const route=new URL(req.url,'http://local').pathname;
  raw.push({route,method:req.method,body:body.toString('base64')});
  const json=v=>{raw[raw.length-1].response=v;res.setHeader('content-type','application/json');res.end(JSON.stringify(v))};
  if(route==='/upload'){
   const ordinal=ids.length,id=crypto.randomUUID();ids.push(id);
   return json(mode==='A04'?{converted:true}:{asset_id:id,ordinal});
  }
  if(route==='/submit')return json({user_id:crypto.randomUUID(),assistant_id:crypto.randomUUID(),turn_id:mode==='P01'?'old':crypto.randomUUID(),parent_id:undefined,...(()=>{const u=crypto.randomUUID();return {user_id:u,parent_id:u}})(),asset_id:crypto.randomUUID()});
  if(route==='/output'){
   downloads++;if(mode==='P04'){res.writeHead(302,{location:'/other'});return res.end()}
   res.setHeader('content-type','image/png');return res.end(image);
  }
  res.setHeader('content-type','text/html');res.end(`<!doctype html><html lang="${mode==='A09'?'xx':'en'}"><body data-profile="owned-loopback-v1">
  <form id="composer"><input id="refs" type="file" multiple><textarea id="prompt"></textarea><button id="send" type="button">Send</button></form><main></main>
  <script>
  const mode=${JSON.stringify(mode)},form=document.querySelector('form'),input=document.querySelector('#refs'),editor=document.querySelector('#prompt'),button=document.querySelector('#send');
  window.events=[];window.clicks=0;
  if(mode==='A03')form.insertAdjacentHTML('beforeend','<img data-asset="old">');
  if(mode==='A05')editor.onfocus=()=>{editor.value='late draft';events.push('focus-tamper')};
  if(mode==='A06-late')editor.oninput=()=>queueMicrotask(()=>{form.querySelector('[data-asset]').dataset.asset='changed';events.push('late-card-tamper')});
  if(mode==='P04-identity'){const original=window.fetch;window.fetch=async(...args)=>{const r=await original(...args);if(String(args[0]).includes('/output'))document.querySelector('[data-role=assistant] img').dataset.asset='swapped-during-fetch';return r}};
  if(mode==='A06')editor.oninput=()=>{editor.value='changed';events.push('input-tamper')};
  input.onchange=async()=>{
   events.push('change');let files=[...input.files];if(mode==='A02')files.reverse();
   for(const f of files){let b=await f.arrayBuffer();if(mode==='A01'&&form.querySelector('[data-asset]'))b=new TextEncoder().encode('wrong same name').buffer;
    const ack=await(await fetch('/upload',{method:'POST',body:b})).json();
    const im=document.createElement('img');im.dataset.asset=ack.asset_id;form.append(im);
   }form.dataset.ready='yes';
  };
  button.onclick=async()=>{
   clicks++;events.push('click');const refs=[...form.querySelectorAll('[data-asset]')].map(e=>e.dataset.asset);
   if(mode==='P02')refs.reverse();
   const e=await(await fetch('/submit',{method:'POST',body:JSON.stringify({prompt:mode==='P03'?'late click tamper':editor.value,refs})})).json();
   document.querySelector('main').innerHTML='<section data-turn="'+e.turn_id+'"><div data-role="user" data-id="'+e.user_id+'"></div><div data-role="assistant" data-id="'+e.assistant_id+'" data-parent="'+(mode==='P01'?'old':e.parent_id)+'"><img data-asset="'+e.asset_id+'" src="/output"></div></section>';
   document.querySelector('section').dataset.complete='yes';
  };
  </script></body></html>`);
 });
 await new Promise(r=>server.listen(0,'127.0.0.1',r));
 return {origin:`http://127.0.0.1:${server.address().port}`,raw,image,ids,get downloads(){return downloads},close:()=>new Promise(r=>{server.closeAllConnections();server.close(r)})};
}
module.exports={fixture,image};
