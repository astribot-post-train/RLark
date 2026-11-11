import assert from "node:assert/strict";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  nodeNetworkKey,
  nodeNetworkModel,
  nodeNetworkSearchText,
} from "../dist/test/utils/nodeNetwork.js";
import { NodeNetworkAddresses } from "../dist/test/components/NodeNetworkAddresses.js";

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
  accessMode: "direct",
  wireguard: {
    state: "current",
    reportedAt: "2026-11-11T04:00:00Z",
    interfaces: [
      {
        name: "wg-node",
        addresses: ["10.254.240.10/32", "fd00::10/128", "10.254.240.10/32", ""],
      },
      { name: "wg-site", addresses: ["10.254.241.1/24"] },
    ],
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

test("only explicit gateway mode renders via gateway and never copies a gateway address", () => {
  assert.equal(
    nodeNetworkModel(node, { accessMode: "gateway" }).message,
    "gateway",
  );
  const html = render({ accessMode: "gateway" });
  assert.match(html, /经站点网关/);
  assert.equal((html.match(/<button/g) ?? []).length, 1);
  assert.equal(
    nodeNetworkModel(node, { accessMode: "unknown" }).message,
    "unreported",
  );
});

test("multiple WG interfaces and IPv6 remain separate from the Kubernetes address", () => {
  const model = nodeNetworkModel(node, current);
  assert.equal(model.message, null);
  assert.deepEqual(
    model.addresses.map((a) => a.address),
    ["10.11.14.128", "10.254.240.10", "fd00::10", "10.254.241.1"],
  );
  const html = render(current);
  assert.match(html, /wg-node · fd00::10\/128/);
  assert.match(html, /复制 WG fd00::10/);
  assert.equal((html.match(/<button/g) ?? []).length, 4);
});

test("stale observations retain historical addresses and explicitly show their receipt time", () => {
  const stale = {
    ...current,
    wireguard: { ...current.wireguard, state: "stale" },
  };
  assert.equal(nodeNetworkModel(node, stale).message, "stale");
  assert.match(render(stale), /上次上报 · 2026-11-11 12:00:00/);
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
