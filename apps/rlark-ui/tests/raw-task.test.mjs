import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { rawTaskFromRole } from "../dist/test/utils/rawTask.js";

describe("Raw task generation", () => {
  it("generates a valid Raw task", () => {
    const config = {
      environment: "python311",
      argv: ["/usr/bin/python3", "-u", "train.py"],
      env: ["PYTHONPATH=/workspace", "CUDA_VISIBLE_DEVICES=0"],
      directory: "/workspace",
      nodeSelector: "kubernetes.io/hostname=ksyun-a",
      replicas: 2,
    };
    const result = rawTaskFromRole("worker", config);
    assert.ok(result.task);
    assert.equal(result.error, undefined);
    const task = result.task;
    assert.equal(task.name, "worker");
    assert.equal(task.role, "Actor");
    assert.equal(task.runtime.kind, "Raw");
    assert.equal(task.placement.environment, "python311");
    assert.deepEqual(task.placement.nodeSelector, { "kubernetes.io/hostname": "ksyun-a" });
    assert.equal(task.workload.kind, "raw.rlark.io/process");
    assert.equal(task.workload.version, "v1");
    assert.deepEqual(task.workload.parameters.argv, ["/usr/bin/python3", "-u", "train.py"]);
    assert.deepEqual(task.workload.parameters.env, ["PYTHONPATH=/workspace", "CUDA_VISIBLE_DEVICES=0"]);
    assert.equal(task.workload.parameters.directory, "/workspace");
    assert.equal(task.replicas, 2);
  });

  it("rejects empty environment", () => {
    const config = {
      environment: "",
      argv: ["/usr/bin/python3"],
      env: [],
      directory: "/workspace",
      nodeSelector: "",
      replicas: 1,
    };
    const result = rawTaskFromRole("worker", config);
    assert.equal(result.task, null);
    assert.ok(result.error.includes("Environment"));
  });

  it("rejects empty argv", () => {
    const config = {
      environment: "python311",
      argv: [],
      env: [],
      directory: "/workspace",
      nodeSelector: "",
      replicas: 1,
    };
    const result = rawTaskFromRole("worker", config);
    assert.equal(result.task, null);
    assert.ok(result.error.includes("argv"));
  });

  it("rejects non-absolute argv[0]", () => {
    const config = {
      environment: "python311",
      argv: ["python3", "train.py"],
      env: [],
      directory: "/workspace",
      nodeSelector: "",
      replicas: 1,
    };
    const result = rawTaskFromRole("worker", config);
    assert.equal(result.task, null);
    assert.ok(result.error.includes("absolute canonical path"));
  });

  it("rejects non-absolute directory", () => {
    const config = {
      environment: "python311",
      argv: ["/usr/bin/python3"],
      env: [],
      directory: "workspace",
      nodeSelector: "",
      replicas: 1,
    };
    const result = rawTaskFromRole("worker", config);
    assert.equal(result.task, null);
    assert.ok(result.error.includes("absolute canonical path"));
  });
});
