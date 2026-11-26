import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("../../src/astribot/pages/JoinNode.tsx", import.meta.url), "utf8");
const styles = await readFile(new URL("../../src/astribot/pages/JoinNode.css", import.meta.url), "utf8");
const dialog = await readFile(new URL("../../src/astribot/pages/JoinMachinesDialog.tsx", import.meta.url), "utf8");
const dialogStyles = await readFile(new URL("../../src/astribot/pages/JoinMachinesDialog.css", import.meta.url), "utf8");

test("the join page is a compact toolbar and one full-width workspace", () => {
  assert.match(page, /className="join-page-head"/);
  assert.match(page, /className="join-toolbar"/);
  assert.match(page, /className="join-method-tabs"/);
  assert.match(page, /<small>\{entry.note\}<\/small>/);
  assert.match(page, /className="join-machine-filters"/);
  assert.match(page, /className="join-machine-blocker"/);
  assert.match(page, /const scanned = scanJoinMachines\(list\?\.items \?\? \[\], clusterID, now, scanID\)/);
  assert.match(page, /<DiscoveryScanControl/);
  assert.doesNotMatch(page, /siteDiscovery\.intervalSeconds/);
  assert.match(page, /Unknown: zh \? "类型待确认" : "Unclassified"/);
  assert.match(page, /open=\{accessOpen \?\? \(scanClasses.length > 0 && !allClassesConfigured\)\}/);
  assert.match(page, /className="join-body"/);
  assert.match(page, /className="join-main"/);
  assert.match(page, /className="join-rail-wrap"/);
  assert.match(page, /className="join-machine-grid"/);
  assert.match(page, /className="join-machine-identity"/);
  assert.match(page, /className="join-machine-location"/);
  assert.match(page, /className="join-machine-reach"/);
  assert.match(page, /hasRegisteredRobotClass\(machine, list\?\.classes \?\? \{\}\)/);
  assert.match(page, /reach \? <>[\s\S]*Orin \{zh \? "上联" : "entry"\}[\s\S]*SSH ·/);
  assert.match(page, /className="join-machine-action"/);
  assert.match(page, /className={`join-selection-bar/);
  assert.doesNotMatch(page, /className="join-hero"/);
  assert.doesNotMatch(page, /className="join-stepper"/);
  assert.doesNotMatch(page, /join-step is-current/);
  assert.doesNotMatch(page, /join-machine-table/, "the old wide machine table is gone");
  assert.match(page, /<JoinMachinesDialog/);
});

test("the join page has real scanned and manual sources, and opens SSH records", () => {
  assert.match(page, /nodeJoinsApi\.list\(clusterID\)/);
  assert.match(page, /join-machine-filter/);
  assert.match(page, /添加普通电脑\/GPU/);
  assert.match(page, /onOpenSSHJoin/);
  assert.match(page, /打开 SSH 记录/);
});

test("the join page CSS is full width at desktop and rows become cards on phones", () => {
  for (const token of ["--accent", "--accent-bg", "--accent-ink", "--panel-soft", "--success", "--text-muted"]) {
    assert.ok(!styles.includes(`var(${token}`), `${token} is not defined in the production tokens`);
  }
  assert.match(styles, /\.join-node-page \.join-body\s*\{[^}]*grid-template-columns: minmax\(0, 1fr\)/);
  assert.match(styles, /@media \(min-width: 1600px\)[\s\S]*\.join-node-page \.join-body\s*\{[^}]*380px/);
  assert.match(styles, /\.join-node-page \.join-machine-card\s*\{[^}]*grid-template-columns:/);
  assert.match(styles, /\.join-node-page \.join-selection-bar\s*\{/);
  assert.match(styles, /\.join-method-layout\s*\{[^}]*minmax\(0, 1fr\) 320px/);
  assert.match(styles, /\.join-node-page \.join-machine-card\s*\{[^}]*border-bottom/);
});

test("the password dialog writes class credentials before admitting", () => {
  assert.match(dialog, /role="dialog"/);
  assert.match(dialog, /aria-modal="true"/);
  assert.match(dialog, /joinCredentialGroups\(machines, classes\)/);
  assert.match(dialog, /joinCredentialProblem\(groups, credentials, zh\)/);
  assert.ok(dialog.indexOf("!canSubmitMachines()") < dialog.indexOf("machinesApi.setClassAccess"), "stale selection is rejected before writing credentials");
  assert.ok(dialog.indexOf("machinesApi.setClassAccess") < dialog.indexOf("machinesApi.admit"), "credentials are written before admit");
  assert.match(dialog, /node_password: typed\.password/);
  assert.match(dialog, /cluster_id: clusterID/);
  assert.match(dialog, /type="password"/);
  assert.doesNotMatch(dialog, /window\.location|URLSearchParams|new URL\(/);
  assert.match(dialogStyles, /\.modal\.join-machines-modal/);
  assert.match(dialogStyles, /@media \(max-width: 620px\)/);
});

test("SSH advanced settings use labeled controls with visible help and a stacked narrow layout", () => {
  const advanced = page.slice(page.indexOf("function SSHJoin(")).match(/<details className="join-node-advanced join-form-advanced">([\s\S]*?)<\/details>/)?.[1];
  assert.ok(advanced);
  assert.match(advanced, /<JoinAdvancedSummary/);
  for (const field of ["name", "port", "endpoint"]) {
    const label = 'htmlFor={`\${advancedID}-' + field + '`}';
    const input = 'id={`\${advancedID}-' + field + '`}';
    const help = 'id={`\${advancedID}-' + field + '-help`}';
    assert.ok(advanced.includes(label) && advanced.includes(input) && advanced.includes(help));
  }
  assert.match(advanced, /aria-invalid=\{!!endpointProblem\}/);
  assert.match(advanced, /role="alert">\{endpointProblem\}/);
  assert.match(styles, /\.join-form-advanced \.join-ssh-advanced-fields\s*\{[^}]*2fr[^}]*1fr/);
  assert.match(styles, /@media \(max-width: 760px\)\s*\{[\s\S]*\.join-form-advanced \.join-ssh-advanced-fields[^\n]*\{ grid-template-columns: minmax\(0, 1fr\)/);
});

test("join code advanced settings group robot and scheduling fields and show validation in place", () => {
  const code = page.slice(page.indexOf("function JoinCode("));
  const advanced = code.match(/<details className="join-node-advanced join-form-advanced" ref=\{advanced\}>([\s\S]*?)<\/details>/)?.[1];
  assert.ok(advanced);
  assert.match(advanced, /<JoinAdvancedSummary zh=\{zh\} hint=\{advancedHint\}/);
  assert.match(advanced, /robot && <fieldset[\s\S]*<legend>[\s\S]*机器人配置/);
  assert.match(advanced, /<legend>[\s\S]*网络[\s\S]*<legend>[\s\S]*节点调度/);
  assert.match(code, /schedulingInvalid && advanced.current\) advanced.current.open = true/);
  assert.match(code, /发码前请选择机器人型号和配置/);
  for (const field of ["labels", "taints"]) {
    assert.ok(advanced.includes("aria-invalid={!!" + field + ".error}"));
    assert.ok(advanced.includes('role="alert"'));
    assert.ok(advanced.includes(field + "-error"));
  }
  assert.match(styles, /\.join-code-advanced-fields textarea\s*\{[^}]*resize: vertical/);
  assert.match(styles, /@media \(max-width: 760px\)\s*\{[\s\S]*\.join-form-advanced \.join-code-advanced-fields\s*\{ grid-template-columns: minmax\(0, 1fr\)/);
});
