import assert from "node:assert/strict";
import test from "node:test";
import { generateJobCRD } from "../dist/test/utils/job.js";
import { crdToJob } from "../dist/test/utils/crd.js";

test("Ray Job ports survive generation and reading, including automatic allocation", () => {
  const ports = [
    { name: "api", protocol: "TCP", targetPort: 8080 },
    { name: "telemetry", protocol: "UDP", targetPort: 9000, nodePort: 31000 },
  ];
  const job = generateJobCRD({
    name: "jo-0123456789abcdef",
    type: "Custom",
    headerRole: "actor",
    roles: ["actor"],
    domain: "",
    runScript: "echo ok",
    roleResources: {
      actor: {
        role: "actor",
        cluster: "cluster",
        nodeSelector: "",
        replicas: 1,
        cpu: "1",
        memory: "1Gi",
        gpu: "0",
        devices: [],
        image: "busybox",
        prepareScript: "",
        envs: [],
        mounts: [],
        exposedPorts: ports,
      },
    },
  });
  assert.deepEqual(job.spec.tasks[0].workload.parameters.exposedPorts, ports);
  assert.equal(
    job.spec.tasks[0].workload.parameters.exposedPortsConfigured,
    true,
  );
  assert.deepEqual(crdToJob(job).resources[0].exposedPorts, ports);
});

test("Ray Job explicitly cleared ports remain configured after reading", () => {
  const job = generateJobCRD({
    name: "jo-0123456789abcdef",
    type: "Custom",
    headerRole: "actor",
    roles: ["actor"],
    domain: "",
    runScript: "echo ok",
    roleResources: {
      actor: {
        role: "actor",
        cluster: "cluster",
        nodeSelector: "",
        replicas: 1,
        cpu: "1",
        memory: "1Gi",
        gpu: "0",
        devices: [],
        image: "busybox",
        prepareScript: "",
        envs: [],
        mounts: [],
        exposedPorts: [],
      },
    },
  });
  assert.equal(job.spec.tasks[0].workload.parameters.exposedPortsConfigured, true);
  assert.deepEqual(crdToJob(job).resources[0].exposedPorts, []);
});
