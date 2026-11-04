import {useEffect, useRef, useState} from "react";
import type {Client, Detail, Enrollment, EnrollmentRequest, EnrollmentTarget} from "./api";
import {enrollmentIntent, enrollmentTargets} from "./enrollment";

export function EnrollmentPanel({detail,client,lang,onChanged,blocked}: {detail:Detail;client:Client;lang:"zh"|"en";onChanged:()=>void;blocked:boolean}) {
  const zh = lang === "zh";
  const key = `rlark.enrollment.${detail.cluster.uid}`;
  const [retained,setRetained] = useState<EnrollmentRequest | undefined>(()=>{
    try {const value:unknown=JSON.parse(sessionStorage.getItem(key)||"null");
      if(value && typeof value==="object" && "clusterUID" in value && value.clusterUID===detail.cluster.uid && "nodes" in value && Array.isArray(value.nodes) && "operationID" in value && typeof value.operationID==="string" && "release" in value && typeof value.release==="string") return enrollmentIntent(value.operationID,detail.cluster.uid,value.release,value.nodes as EnrollmentTarget[]);
    } catch { /* Ignore malformed retained input; never submit it automatically. */ }
    return undefined;
  });
  const [nodes,setNodes]=useState(()=>retained?.nodes ?? enrollmentTargets(detail));
  const [release,setRelease]=useState(retained?.release ?? "");
  const [busy,setBusy]=useState(false);
  const [error,setError]=useState("");
  const controller=useRef<AbortController | undefined>(undefined);
  useEffect(()=>{
    if(retained) return;
    const known=enrollmentTargets(detail);
    setNodes(current=>current.map(node=>{
      const observed=known.find(n=>n.name===node.name && n.address===node.address);
      return observed?.nodeUID && !node.nodeUID ? {...node,nodeUID:observed.nodeUID} : node;
    }));
  },[detail,retained]);
  useEffect(()=>()=>controller.current?.abort(),[]);
  function update(index:number, patch:Partial<EnrollmentTarget>) {setNodes(current=>current.map((n,i)=>i===index?{...n,...patch}:n));}
  async function submit() {
    let request:EnrollmentRequest;
    try {request=retained ?? enrollmentIntent(crypto.randomUUID(),detail.cluster.uid,release,nodes);} catch(e) {setError(String(e));return;}
    setRetained(request);
    try {sessionStorage.setItem(key,JSON.stringify(request));} catch { /* Same mounted form retains the request. */ }
    setBusy(true);setError("");
    const active=new AbortController();controller.current=active;
    try {
      await client.startNodes(detail.cluster,request,active.signal);
      if(active.signal.aborted) return;
      sessionStorage.removeItem(key);setRetained(undefined);onChanged();
    } catch(e) {if(!active.signal.aborted) setError(e instanceof Error?e.message:String(e));}
    finally {if(!active.signal.aborted) setBusy(false);}
  }
  const disabled=busy || !!retained;
  async function cancel(operation:Enrollment) {
    setBusy(true);setError("");
    const active=new AbortController();controller.current=active;
    try {
      await client.cancelEnrollment(detail.cluster,operation,active.signal);
      if(!active.signal.aborted) onChanged();
    } catch(e) {if(!active.signal.aborted) setError(e instanceof Error?e.message:String(e));}
    finally {if(!active.signal.aborted) setBusy(false);}
  }
  return <section className="panel enrollment-panel">
    <h3>{zh?"Notebook 启动接入":"Notebook start / join"}</h3>
    <p className="muted">{zh?"A 为网关兼计算节点，B/C 经 A 的 SSH 接入。预配置私钥不经页面传输；保存后安装并启动所需服务。":"A is the gateway and compute node; workers join through A over SSH. Preconfigured private keys never pass through this page. Save to install and start required services."}</p>
    {!detail.enrollmentEnabled && <p className="notice">{zh?"管理员尚未启用 SSH 接入控制器和 Gateway 接入入口。":"SSH enrollment controller and Gateway capability must be enabled by an administrator."}</p>}
    <fieldset disabled={disabled || blocked || !detail.enrollmentEnabled}>
      <label>{zh?"Agent 版本":"Agent version"}<select value={release} onChange={e=>setRelease(e.target.value)}><option value="">{zh?"最新可用版本（提交时固定）":"Latest available (pinned on submit)"}</option>{detail.releases.filter(r=>r.available && r.os==="linux" && r.architecture==="amd64").map(r=><option key={r.uid} value={r.name}>{r.version} · {r.name}</option>)}</select></label>
      <div className="table-scroll"><table><thead><tr>{(zh?["节点名","SSH IP 地址","SSH 端口","角色","操作"]:["Node","SSH IP address","SSH port","Role","Actions"]).map(s=><th key={s}>{s}</th>)}</tr></thead><tbody>{nodes.map((node,i)=><tr key={i}>
        <td><input aria-label={zh?"节点名":"Node name"} value={node.name} readOnly={!!node.nodeUID} onChange={e=>update(i,{name:e.target.value})}/></td>
        <td><input aria-label={zh?"SSH IP 地址":"SSH IP address"} value={node.address} onChange={e=>update(i,{address:e.target.value})}/></td>
        <td><input aria-label={zh?"SSH 端口":"SSH port"} type="number" min="1" max="65535" value={node.port} onChange={e=>update(i,{port:Number(e.target.value)})}/></td>
        <td><select aria-label={zh?"角色":"Role"} value={node.role} disabled={!!node.nodeUID} onChange={e=>update(i,{role:e.target.value as EnrollmentTarget["role"]})}><option value="Gateway">{zh?"网关 + 计算":"Gateway + compute"}</option><option value="Worker">Worker</option></select></td>
        <td><button type="button" onClick={()=>setNodes(current=>current.filter((_,index)=>i!==index))}>{zh?"移除此行":"Remove row"}</button></td>
      </tr>)}</tbody></table></div>
      <button type="button" onClick={()=>setNodes(current=>[...current,{name:`node-${current.length+1}`,address:"",role:"Worker",port:22}])}>{zh?"增加 Notebook 节点":"Add Notebook node"}</button>
    </fieldset>
    <p className="muted">{zh?"主机停机时请先在金山云启动。更换实例/IP 不会自动接管旧节点。":"Start stopped Notebooks in Kingsoft Cloud first. Replaced instances/IPs are never silently adopted."}</p>
    {retained && <p className="notice">{zh?"结果待确认，请保留原请求并重试：":"Unconfirmed result; retry the retained request: "}{retained.operationID}</p>}
    {error && <p role="alert">{error}</p>}
    <button className="primary" disabled={busy || blocked || !detail.enrollmentEnabled} onClick={()=>void submit()}>{busy?(zh?"提交中…":"Submitting…"):retained?(zh?"重试同一接入操作":"Retry same operation"):(zh?"保存并启动接入（全部服务）":"Save and start / join (all services)")}</button>
    {detail.enrollments.map(op=><div className="notice" key={op.uid}><strong>{op.cancelRequested?(zh?"取消中/已取消":"Cancellation requested"):op.phase || (zh?"待处理":"Pending")} · {op.releaseVersion}</strong><div><code>{op.operationID}</code></div>{op.targets.map(target=>{const status=op.nodes?.find(n=>n.name===target.name);return <div key={target.name}>{target.name} · {target.role} · {target.address} · {status?.reachability || "Unknown"} · {status?.phase || "Pending"} · {status?.reason} · {status?.services?.join(", ")}</div>;})}{op.conditions?.map(c=><div key={c.type}>{c.reason}: {c.message}</div>)}{!["Succeeded","Failed","Cancelled"].includes(op.phase) && !op.cancelRequested && <button type="button" disabled={busy || blocked || !detail.enrollmentEnabled} onClick={()=>void cancel(op)}>{zh?"取消本次接入（保留已接入节点）":"Cancel enrollment (retain joined nodes)"}</button>}</div>)}
  </section>;
}
