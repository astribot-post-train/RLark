import assert from "node:assert/strict";
import {test} from "node:test";
import {Client} from "../dist/test/admin/container-runtime/api.js";
import {enrollmentIntent, enrollmentTargets, nodeProbe, probeState} from "../dist/test/admin/container-runtime/enrollment.js";

globalThis.sessionStorage = {getItem:()=>"admin",removeItem:()=>{}};
globalThis.window = {dispatchEvent:()=>{}};

test("enrollment cancellation pins exact operation and cluster and rejects foreign receipts", async()=>{
  const operation={name:"enroll-1",uid:"operation-instance",operationID:"operation",releaseName:"v1",releaseVersion:"v1",phase:"Waiting",targets:[],nodes:[],conditions:[],cancelRequested:true};
  let seen;
  globalThis.fetch=async(path,init)=>{seen={path,init};return new Response(JSON.stringify(operation));};
  const client=new Client();
  const cluster={namespace:"agents",name:"cloud",uid:"cluster-instance"};
  const controller=new AbortController();
  await client.cancelEnrollment(cluster,operation,controller.signal);
  assert.match(seen.path,/\/clusters\/agents\/cloud\/enrollments\/enroll-1\/cancel$/);
  assert.deepEqual(JSON.parse(seen.init.body),{uid:"operation-instance",clusterUID:"cluster-instance"});
  assert.equal(seen.init.signal,controller.signal);
  globalThis.fetch=async()=>new Response(JSON.stringify({...operation,uid:"foreign"}));
  await assert.rejects(client.cancelEnrollment(cluster,operation));
});

test("enrollment uses retained SSH topology and fresh UID-bound probes",()=>{
  const target={name:"a",role:"Gateway",address:"36.103.180.181",port:22};
  const detail={enrollments:[{targets:[target],nodes:[{name:"a",nodeUID:"current",reachability:"Reachable",probedAt:new Date().toISOString()}]}],nodes:[{name:"a",uid:"current",privateAddress:"10.1.3.214",role:"Gateway"}]};
  const targets=enrollmentTargets(detail);
  assert.equal(targets[0].address,target.address);
  assert.equal(targets[0].nodeUID,"current");
  assert.equal(probeState(nodeProbe(detail,detail.nodes[0])),"Reachable");
  assert.equal(nodeProbe(detail,{...detail.nodes[0],uid:"replacement"}),undefined);
  assert.equal(probeState({reachability:"Reachable",probedAt:new Date(Date.now()-61000).toISOString()}),"Unknown");
  const intent=enrollmentIntent("op","cluster","",targets);
  assert.deepEqual(intent.nodes,targets);
  assert.throws(()=>enrollmentIntent("op","cluster","",[target,{...target,name:"b"}]));
});
