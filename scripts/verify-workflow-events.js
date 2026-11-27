// Real JetStream + Temporal + factory processes. Only the paid model provider is a deterministic HTTP fixture.
import assert from 'node:assert/strict';
import {spawn, execFileSync} from 'node:child_process';
import {mkdtemp, mkdir, writeFile, readFile, rm, access} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import net from 'node:net';
import http from 'node:http';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const dir=await mkdtemp(path.join(tmpdir(),'factory-events-'));
const children=[];let provider;
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
async function freePort(){const s=net.createServer();await new Promise((r,j)=>{s.once('error',j);s.listen(0,'127.0.0.1',r);});const p=s.address().port;await new Promise(r=>s.close(r));return p;}
async function api(base,method,route,body,expected=200,headers={}){
 const response=await fetch(base+route,{method,headers:{'Content-Type':'application/json','X-Creator':'integration-test',...headers},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(15000)});
 const text=await response.text();assert.equal(response.status,expected,`${method} ${route}: ${text}`);return text?JSON.parse(text):{};
}
function launch(name,binary,args,env={}){
 const child=spawn(binary,args,{env:{...process.env,OTEL_EXPORTER_OTLP_TRACES_ENDPOINT:'',...env},stdio:['ignore','pipe','pipe']});
 child.output='';child.label=name;for(const stream of [child.stdout,child.stderr])stream.on('data',b=>{child.output=(child.output+b).slice(-12000);});children.push(child);return child;
}
async function ready(child,url){
 for(let i=0;i<300;i++){if(child.exitCode!==null||child.signalCode)throw new Error(`${child.label} exited: ${child.output}`);try{if((await fetch(url,{signal:AbortSignal.timeout(1000)})).ok)return;}catch{}await sleep(200);}
 throw new Error(`${child.label} startup timeout: ${child.output}`);
}
async function assertPublishDenied(port,user,password,subject){
 await new Promise((resolve,reject)=>{
  const socket=net.connect(port,'127.0.0.1');let response='';
  const timer=setTimeout(()=>{socket.destroy();reject(new Error('consumer event publication was not denied'));},3000);
  socket.on('connect',()=>socket.write('CONNECT '+JSON.stringify({user,pass:password,verbose:false})+'\r\nPUB '+subject+' 2\r\n{}\r\nPING\r\n'));
  socket.on('data',data=>{response+=data;if(/Permissions Violation/i.test(response)){clearTimeout(timer);socket.destroy();resolve();}});
  socket.on('error',error=>{clearTimeout(timer);reject(error);});
 });
}
async function until(fn,description){let last;for(let i=0;i<180;i++){try{const result=await fn();if(result)return result;}catch(e){last=e;}await sleep(500);}throw new Error(`${description}: ${last?.message||'timed out'}`);}
try{
 const nats=process.env.NATS_SERVER_BIN||path.join(root,'scratch/measurements/bin/nats-server');await access(nats);
 let temporal=process.env.TEMPORAL_BIN;
 if(!temporal){const location=execFileSync('mise',['where','aqua:temporalio/cli@1.5.1'],{encoding:'utf8'}).trim();temporal=path.join(location,'temporal');}
 await access(temporal);
 for(const service of ['blobstore','evaluation','registry','llm-gateway','orchestrator','agent','connectors'])execFileSync('go',['build','-o',path.join(dir,service),'.'],{cwd:path.join(root,'services',service),stdio:'inherit'});
 const ports={};for(const name of ['nats','monitor','temporal','temporalHTTP','blobstore','evaluation','registry','llm-gateway','orchestrator','agent','connectors','provider'])ports[name]=await freePort();
 const url=name=>`http://127.0.0.1:${ports[name]}`;
 let calls=0, omitUsage=false;
 provider=http.createServer(async(req,res)=>{for await(const _ of req){}res.setHeader('Content-Type','application/json');if(req.method==='GET'){res.end(JSON.stringify(req.url.endsWith('/models')?{object:'list',data:[{id:'fixture-model',object:'model'}]}:{id:'fixture-model',object:'model'}));return;}calls++;res.end(JSON.stringify({id:`completion-${calls}`,object:'chat.completion',model:'fixture-model',choices:[{index:0,message:{role:'assistant',content:'{"quality":0,"finding":"Evidence retained"}'},finish_reason:'stop'}],...(omitUsage?{}:{usage:{prompt_tokens:20,completion_tokens:10,total_tokens:30,prompt_tokens_details:{cached_tokens:4}}})}));});
 await new Promise(r=>provider.listen(ports.provider,'127.0.0.1',r));
 const natsConfig=(await readFile(path.join(root,'infra/kubernetes/base/jetstream/nats.conf'),'utf8')).replace('port: 4222',`port: ${ports.nats}`).replace('http_port: 8222',`http_port: ${ports.monitor}`).replace('store_dir: /data',`store_dir: ${JSON.stringify(path.join(dir,'nats-data'))}`)+'\nhost: 127.0.0.1\n';
 await writeFile(path.join(dir,'nats.conf'),natsConfig);
 const broker=launch('nats',nats,['-c',path.join(dir,'nats.conf')],{NATS_PUBLISHER_USER:'fixture-publisher',NATS_PUBLISHER_PASSWORD:'fixture-publisher',NATS_CONSUMER_USER:'fixture-consumer',NATS_CONSUMER_PASSWORD:'fixture-consumer'});await ready(broker,url('monitor')+'/healthz');await assertPublishDenied(ports.nats,'fixture-consumer','fixture-consumer','factory.workflow.finished');await assertPublishDenied(ports.nats,'fixture-publisher','fixture-publisher','$JS.API.CONSUMER.INFO.FACTORY_WORKFLOW_EVENTS.test');
 const temporalProcess=launch('temporal',temporal,['server','start-dev','--ip','127.0.0.1','--port',String(ports.temporal),'--http-port',String(ports.temporalHTTP),'--headless','--db-filename',path.join(dir,'temporal.db')]);
 await until(async()=>{if(temporalProcess.exitCode!==null)throw new Error(temporalProcess.output);return new Promise(resolve=>{const socket=net.connect(ports.temporal,'127.0.0.1');socket.on('connect',()=>{socket.destroy();resolve(true);});socket.on('error',()=>resolve(false));});},'Temporal startup');
 const shared={BLOBSTORE_URL:url('blobstore'),BLOB_URL:url('blobstore')+'/blobs',EVALUATION_URL:url('evaluation'),ORCHESTRATOR_URL:url('orchestrator'),AGENT_REGISTRY_URL:url('registry'),POLICY_REGISTRY_URL:'http://127.0.0.1:1',MCP_GATEWAY_URL:'http://127.0.0.1:1',TEMPORAL_ADDRESS:`127.0.0.1:${ports.temporal}`,TEMPORAL_NAMESPACE:'default',NATS_URL:`nats://127.0.0.1:${ports.nats}`,NATS_USER:'fixture-publisher',NATS_PASSWORD:'fixture-publisher'};
 await ready(launch('blobstore',path.join(dir,'blobstore'),['-data',path.join(dir,'blobs'),'-addr',`127.0.0.1:${ports.blobstore}`],shared),url('blobstore')+'/healthz');
 await mkdir(path.join(dir,'state'));
 await ready(launch('evaluation',path.join(dir,'evaluation'),[],{...shared,EVALUATION_ADDR:`127.0.0.1:${ports.evaluation}`,EVALUATION_LOCK_PATH:path.join(dir,'state/writer.lock'),OPENOBSERVE_CONFIG:path.join(dir,'absent.env'),OPENOBSERVE_URL:'http://127.0.0.1:1'}),url('evaluation')+'/healthz');
 await ready(launch('registry',path.join(dir,'registry'),['-addr',`127.0.0.1:${ports.registry}`],shared),url('registry')+'/healthz');
 const config={log_level:'ERROR',blobstore_url:url('blobstore'),evaluation_url:url('evaluation'),providers:[{name:'fixture',base_url:url('provider')+'/v1',auth:{type:'none'},streaming:false,models:{fixture:'fixture-model'}}]};
 await writeFile(path.join(dir,'gateway.yaml'),JSON.stringify(config));
 await ready(launch('gateway',path.join(dir,'llm-gateway'),['-config',path.join(dir,'gateway.yaml'),'-logs',path.join(dir,'gateway-logs')],{...shared,GATEWAY_ADDR:`127.0.0.1:${ports['llm-gateway']}`}),url('llm-gateway')+'/v1/models');
 await ready(launch('orchestrator',path.join(dir,'orchestrator'),['-addr',`127.0.0.1:${ports.orchestrator}`],shared),url('orchestrator')+'/healthz');
 await ready(launch('agent',path.join(dir,'agent'),[],{...shared,LLM_URL:url('llm-gateway')+'/v1',CHAT_ADDR:`127.0.0.1:${ports.agent}`}),url('agent')+'/healthz');
 await ready(launch('connectors',path.join(dir,'connectors'),[],{...shared,NATS_USER:'fixture-consumer',NATS_PASSWORD:'fixture-consumer',CONNECTORS_ADDR:`127.0.0.1:${ports.connectors}`,CONNECTOR_SEED_FILE:path.join(dir,'no-seeds')}),url('connectors')+'/healthz');
 for(const name of ['worker','judge']){
  const definition={schema_version:1,name:`fixture/${name}`,description:'Temporary integration fixture',models:['fixture'],effort:'low',tools:[],max_turns:1,unpack_inputs:true};
  if(name==='judge')definition.output_contract={format:'json',mappings:[{path:'/quality',name:'quality',type:'number'},{path:'/finding',name:'finding',type:'string'}]};
  await api(url('registry'),'POST',`/agents/fixture/${name}`,{files:{'agent.json':definition,'systemprompt.md':'Return JSON with quality number and finding text.'}},201);
 }
 for(const [name,agent]of [['child','worker'],['scoring','judge']])await api(url('orchestrator'),'POST',`/api/workflows/${encodeURIComponent('fixture/'+name)}/versions`,{steps:[{id:'run',service:'agent',agent:`fixture/${agent}@v1`}]},201);
 await api(url('orchestrator'),'POST',`/api/workflows/${encodeURIComponent('fixture/source')}/versions`,{steps:[{id:'nested',service:'workflow',workflow:'fixture/child@v1'}]},201);
 await api(url('connectors'),'PUT','/api/intake/bindings/integration',{id:'integration',name:'Integration',enabled:true,source:'factory',source_workflows:['fixture/source'],outcomes:['completed'],workflow:'fixture/scoring',workflow_version:1},200,{'If-None-Match':'*'});
 const source=await api(url('orchestrator'),'POST',`/api/workflows/${encodeURIComponent('fixture/source')}/runs?version=1`,{task:'Run the integration fixture'},202);
 await until(async()=>{const state=await api(url('orchestrator'),'GET',`/api/runs/${source.trace_id}`);if(state.status==='Failed')throw new Error(JSON.stringify(state));return state.status==='Completed';},'source completion');
 const result=await until(async()=>{const data=await api(url('evaluation'),'GET','/api/measurements?kind=judge');const rows=data.items||data.measurements||[];return rows.length===2?rows:null;},'event-triggered judge metrics');
 assert.ok(result.every(m=>m.target_run_id===source.trace_id));assert.ok(result.some(m=>m.metric_name==='quality'&&Number(m.number_value)===0));assert.equal(new Set(result.map(m=>m.producer_run_id)).size,1);assert.equal(calls,2);
 const durations=await until(async()=>{const data=await api(url('evaluation'),'GET',`/api/measurements?kind=execution&run_id=${source.trace_id}`);const rows=data.items||data.measurements||[];return rows.length>=2?rows:null;},'root and child execution measurements');
 assert.ok(durations.every(m=>typeof m.duration_ms==='number'&&m.duration_ms>=0));
 assert.ok(durations.some(m=>m.parent_execution_id));
 await new Promise(resolve=>{broker.once('exit',resolve);broker.kill('SIGTERM');});
 const restartedBroker=launch('nats-restarted',nats,['-c',path.join(dir,'nats.conf')],{NATS_PUBLISHER_USER:'fixture-publisher',NATS_PUBLISHER_PASSWORD:'fixture-publisher',NATS_CONSUMER_USER:'fixture-consumer',NATS_CONSUMER_PASSWORD:'fixture-consumer'});
 await ready(restartedBroker,url('monitor')+'/healthz');
 await sleep(3000);
 assert.equal(calls,2,'broker restart must not rerun paid business work');
 const view=await api(url('connectors'),'POST','/api/events/sessions',{start_sequence:1},201);
 const events=await api(url('connectors'),'GET',`/api/events/sessions/${view.id||view.session_id}?limit=1000`);
 assert.ok(events.events.some(e=>e.run_id===source.trace_id&&e.type==='workflow.finished'));
 assert.ok(events.events.some(e=>e.workflow==='fixture/child'&&e.type==='workflow.finished'&&e.parent_execution_id));
 await api(url('connectors'),'DELETE',`/api/events/sessions/${view.id||view.session_id}`,undefined,204);
 omitUsage=true;
 await api(url('llm-gateway'),'POST','/v1/chat/completions',{model:'fixture',messages:[{role:'user',content:'explicit missing-usage probe'}],stream:false});
 const unknown=await until(async()=>{const data=await api(url('evaluation'),'GET','/api/measurements?kind=usage');return (data.items||data.measurements||[]).find(m=>!m.run_id&&!m.execution_id);},'unattributed missing provider evidence');
 assert.equal(unknown.input_tokens,undefined);assert.equal(unknown.effort,undefined);
 assert.ok(unknown.missing.some(reason=>reason.startsWith('raw_usage/')));
 console.log('PASS: real JetStream/Temporal and factory processes: source completion -> durable event -> intake -> ordinary judge -> JSON metrics; nested lifecycle and duration measurements; broker restart and viewer replay; two workflow provider calls; omitted-usage/effort/attribution probe retained as unknown.');
 console.log('NOT VERIFIED here: actual OpenObserve projection, browser rendering, or shared cluster deployment.');
}catch(error){for(const c of children)console.error(`--- ${c.label} ---\n${c.output}`);throw error;}
finally{for(const c of children.reverse()){if(c.exitCode!==null||c.signalCode)continue;c.kill('SIGTERM');await Promise.race([new Promise(r=>c.once('exit',r)),sleep(5000)]);if(c.exitCode===null&&!c.signalCode){c.kill('SIGKILL');await new Promise(r=>c.once('exit',r));}}if(provider)await new Promise(r=>provider.close(r));await rm(dir,{recursive:true,force:true});}
