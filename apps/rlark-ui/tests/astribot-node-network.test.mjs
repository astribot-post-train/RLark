import assert from "node:assert/strict";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  nodeLinkHealth,
  nodeNetworkKey,
  nodeNetworkModel,
  nodeNetworkSearchText,
  nodeSelfLine,
  nodeSelfLink,
} from "../dist/test/astribot/lib/nodeNetwork.js";
import { NodeNetworkAddresses } from "../dist/test/astribot/components/NodeNetworkAddresses.js";
import { NodeLinks } from "../dist/test/astribot/components/NodeLinks.js";

const node = {
  metadata: { name: "worker", namespace: "cluster-a" },
  spec: {},
  status: {
    addresses: [
      { type: "Hostname", address: "worker" },
      { type: "InternalIP", address: "10.11.14.128" },
    ],
  },
};
const current = {
  wireguard: {
    state: "current",
    reportedAt: "2026-11-11T04:00:00Z",
    interface: "wg-node",
    addresses: ["10.254.240.10/32", "fd00::10/128", "10.254.240.10/32", ""],
  },
};
const render = (network, zh = true) =>
  renderToStaticMarkup(
    React.createElement(NodeNetworkAddresses, { node, network, zh }),
  );

test("missing WG telemetry retains the node IP without inventing a peer or gateway", () => {
  const model = nodeNetworkModel(node);
  assert.deepEqual(model.addresses, [
    { kind: "node", address: "10.11.14.128" },
  ]);
  assert.equal(model.message, "unreported");
  assert.match(render(), /未上报/);
  assert.match(render(undefined, false), /Not reported/);
});

test("WG addresses, IPv6 too, are listed once each beside the Kubernetes address", () => {
  const model = nodeNetworkModel(node, current);
  assert.equal(model.message, null);
  assert.deepEqual(
    model.addresses.map((a) => a.address),
    ["10.11.14.128", "10.254.240.10", "fd00::10"],
  );
  const html = render(current);
  assert.match(html, /wg-node · fd00::10\/128/);
  assert.match(html, /复制 WG fd00::10/);
  assert.equal((html.match(/<button/g) ?? []).length, 3);
});

test("stale observations retain historical addresses and explicitly show their receipt time", () => {
  const stale = {
    ...current,
    wireguard: { ...current.wireguard, state: "stale" },
  };
  assert.equal(nodeNetworkModel(node, stale).message, "stale");
  assert.match(render(stale), /上次上报 · 2026-11-11 12:00:00/);
  assert.equal((render(stale).match(/<button/g) ?? []).length, 1);
});

test("unavailable telemetry does not present cached addresses as current or searchable", () => {
  const unavailable = {
    ...current,
    wireguard: { ...current.wireguard, state: "unavailable" },
  };
  assert.equal(nodeNetworkModel(node, unavailable).message, "unavailable");
  assert.equal(nodeNetworkSearchText(node, unavailable).trim(), "10.11.14.128");
  assert.match(render(unavailable), /数据不可用/);
  assert.doesNotMatch(render(unavailable), /10\.254\.240\.10/);
});

test("address search includes WG IPv4 and IPv6; keys separate identically named nodes", () => {
  const searchable = nodeNetworkSearchText(node, current);
  assert.match(searchable, /10\.11\.14\.128/);
  assert.match(searchable, /10\.254\.240\.10/);
  assert.match(searchable, /fd00::10/);
  assert.notEqual(
    nodeNetworkKey(node),
    nodeNetworkKey({
      ...node,
      metadata: { ...node.metadata, namespace: "cluster-b" },
    }),
  );
});

test("empty node and WG address lists do not create copy controls", () => {
  const empty = { metadata: { name: "empty" }, spec: {} };
  const html = renderToStaticMarkup(
    React.createElement(NodeNetworkAddresses, { node: empty, zh: true }),
  );
  assert.match(html, /—/);
  assert.match(html, /未上报/);
  assert.doesNotMatch(html, /<button/);
});

// Shapes of GET /api/v1/site-nodes items. `connectedLeaf` is s1u-01-x86 as
// the management plane recorded it on 11-14 (all five hubs Connected).
const hub = (name, state, since = "2026-11-13T18:27:37Z", extra = {}) => ({ hub: name, state, since, ...extra });
const connectedLeaf = {
  online: true,
  tunnel: "verified",
  role: "Leaf",
  summary: "",
  links: [
    hub("rlark-local-dev-gpu/cailiang", "Connected", "2026-11-13T18:49:38Z"),
    hub("rlark-ksyun-gpu/10-0-2-57", "Connected"),
    hub("rlark-ksyun-gpu/10-0-2-196", "Connected"),
    hub("rlark-robot-s1u-01/alexfu", "Connected", "2026-11-13T18:52:38Z"),
    hub("rlark-local-dev-gpu/wendychen", "Connected", "2026-11-13T18:49:08Z"),
  ],
};
const silentLeaf = {
  ...connectedLeaf,
  summary: "nothing arrives from this node at rlark-robot-s1u-01/alexfu",
  links: connectedLeaf.links.map((link) =>
    link.hub.endsWith("/alexfu") ? { ...link, state: "Silent", latest_handshake: "2026-11-13T18:50:00Z" } : link,
  ),
};
const unansweredLeaf = {
  ...connectedLeaf,
  summary: "this node's requests arrive at rlark-local-dev-gpu/cailiang and no session forms: the answers do not get back to it",
  links: connectedLeaf.links.map((link) => (link.hub.endsWith("/cailiang") ? { ...link, state: "Unanswered" } : link)),
};

test("a node whose hubs all hear it is connected and shows no summary", () => {
  assert.equal(nodeLinkHealth(connectedLeaf), "connected");
  assert.match(render({ site: connectedLeaf }), /node-link connected.*5 个 Hub 都已连通/);
  assert.match(render({ site: { online: true, tunnel: "verified", role: "Hub", links: [], summary: "" } }), /在线/);
});

test("a silent or unanswered hub marks the node down and names the hub", () => {
  assert.equal(nodeLinkHealth(silentLeaf), "down");
  assert.match(render({ site: silentLeaf }), /node-link down/);
  assert.match(render({ site: silentLeaf }), /alexfu 收不到它的包/);
  assert.match(render({ site: silentLeaf }), /title="nothing arrives from this node at rlark-robot-s1u-01\/alexfu"/);
  assert.match(render({ site: unansweredLeaf }), /请求到了 cailiang，回包回不来/);
  assert.match(render({ site: { ...connectedLeaf, online: false } }), /心跳中断/);
  assert.equal(nodeLinkHealth({ ...connectedLeaf, links: [hub("c/h", "Unknown")] }), "unknown");
});

test("the node page lists every hub with its state, since and last handshake", () => {
  const html = renderToStaticMarkup(React.createElement(NodeLinks, { network: { site: silentLeaf }, zh: true }));
  assert.match(html, /rlark-robot-s1u-01\/alexfu<\/dt><dd><span class="node-link down">收不到/);
  assert.match(html, /最后握手 2026-11-14 02:50:00/);
  assert.match(html, /nothing arrives from this node/);
  assert.match(renderToStaticMarkup(React.createElement(NodeLinks, { network: {}, zh: true })), /不在站点网络中/);
  assert.match(renderToStaticMarkup(React.createElement(NodeLinks, { zh: true })), /读取中/);
});

// The node's own record (site-nodes link): a GPU node that nothing restarts, down 12 min on 11-28 (UTC 02:00).
const notAsked = "not asked: nothing on this machine owns its restart (restart-owner); a person restarts it if it needs one";
const gpuOutage = {
  state: "up",
  since: 1795831920,
  restarts: 0,
  actions: [
    { at: 1795831200, step: "down", result: "no peer this node dials has answered for 3m0s" },
    { at: 1795831200, step: "uplink", result: "skipped: eno1 has its address and a route to 10.11.14.128" },
    { at: 1795831260, step: "wireguard", result: "st-wg0 removed, made anew by this pass" },
    { at: 1795831380, step: "restart", result: notAsked },
    { at: 1795831920, step: "recovered", result: "the tunnel answers again after 12m0s" },
  ],
};

test("a node's own record is read only when it holds something to show", () => {
  assert.equal(nodeSelfLink(undefined), undefined);
  assert.equal(nodeSelfLink(""), undefined);
  assert.equal(nodeSelfLink("{not json"), undefined);
  assert.equal(nodeSelfLink(JSON.stringify({ state: "up", since: 1, restarts: 0, actions: [] })), undefined);
  assert.deepEqual(nodeSelfLink(JSON.stringify(gpuOutage)), gpuOutage);
});

test("the self-heal line says when the node recovered, after how long, and what nobody restarted", () => {
  assert.equal(nodeSelfLine(gpuOutage, true), "11-28 10:12 恢复（断 12 分钟） · 未自动重启");
  assert.equal(nodeSelfLine(gpuOutage, false), "Recovered 11-28 10:12 after 12 min · not restarted automatically");
  const down = { ...gpuOutage, state: "down", since: 1795831200, actions: gpuOutage.actions.slice(0, 4) };
  assert.equal(nodeSelfLine(down, true), "断网自 11-28 10:00 · 最近：重启 · 需人工重启");
  assert.equal(nodeSelfLine(down, false), "Down since 11-28 10:00 · last: restart · needs a person to restart");
  // A robot: its restart was asked of its owner, so nothing is marked.
  const robot = { ...gpuOutage, actions: gpuOutage.actions.map((action) => (action.step === "restart" ? { ...action, result: "restart 1 asked of s1u-robot" } : action)) };
  assert.equal(nodeSelfLine(robot, false), "Recovered 11-28 10:12 after 12 min");
  const html = render({ site: { ...connectedLeaf, self: gpuOutage } });
  assert.match(html, /自愈.*11-28 10:12 恢复/);
  assert.match(html, /title="the tunnel answers again after 12m0s"/);
  const page = renderToStaticMarkup(React.createElement(NodeLinks, { network: { site: { ...connectedLeaf, self: gpuOutage } }, zh: false }));
  assert.match(page, /Self-heal: Recovered 11-28 10:12 after 12 min/);
  assert.match(page, /11-28 10:12<\/small> recovered — the tunnel answers again after 12m0s.*11-28 10:03<\/small> restart — not asked/);
});
