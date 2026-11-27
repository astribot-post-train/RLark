import { useEffect, useRef, useState } from "react";
import { ApiError } from "../../api";
import { nodeJoinsApi } from "../api";
import { stationRejoinFailure, stationRejoinProgress, type StationRejoinState } from "../lib/stationRejoin";

// An automatic rejoin is a retry of one existing NodeJoin, followed by a bounded series of reads of that join record.
// It never writes a second request while one is in flight, never sends a password, and never re-submits after a
// failure: the card says why it stopped and leaves the next step to the operator.
export const REJOIN_POLL_INTERVAL_MS = 5000;
// A retry can start at any management-plane stage; node-ready alone waits up to five minutes and station up to three.
// The window is a display bound, not an abort: a join still running at its end is reported as unfinished, not failed.
export const REJOIN_WINDOW_MS = 12 * 60 * 1000;
const LINGER_MS = 60_000;

export interface StationRejoinEntry extends StationRejoinState {
  key: string;
  startedAt: number;
}

export interface StationRejoinTarget {
  key: string;
  clusterID: string;
  nodeName: string;
  // The NodeJoin's name, <cluster>.<node>; the retry endpoint addresses an existing record by this name only.
  name: string;
  stage: string;
}

export function useStationRejoin(zh: boolean, onChanged?: () => unknown) {
  const [entries, setEntries] = useState<Record<string, StationRejoinEntry>>({});
  // A synchronous in-flight set: state alone cannot stop a second click in the same frame from submitting twice.
  const busy = useRef(new Set<string>());
  const mounted = useRef(true);
  const timers = useRef<number[]>([]);
  // A key's generation: an older poll stops touching a key a newer attempt took over.
  const generations = useRef(new Map<string, number>());

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      timers.current.forEach((timer) => window.clearTimeout(timer));
      timers.current = [];
      busy.current.clear();
    };
  }, []);

  const schedule = (callback: () => void, delay: number) => {
    const timer = window.setTimeout(callback, delay);
    timers.current.push(timer);
  };

  const update = (key: string, generation: number, patch: Partial<StationRejoinEntry>) => {
    if (!mounted.current || generations.current.get(key) !== generation) return;
    setEntries((current) => (current[key] ? { ...current, [key]: { ...current[key], ...patch } } : current));
  };

  const linger = (key: string, generation: number) => {
    schedule(() => {
      if (!mounted.current || generations.current.get(key) !== generation) return;
      generations.current.delete(key);
      setEntries((current) => {
        if (!current[key]) return current;
        const next = { ...current };
        delete next[key];
        return next;
      });
    }, LINGER_MS);
  };

  const poll = (target: StationRejoinTarget, generation: number) => {
    const deadline = performance.now() + REJOIN_WINDOW_MS;
    const step = async () => {
      if (!mounted.current || generations.current.get(target.key) !== generation) return;
      let join: Awaited<ReturnType<typeof nodeJoinsApi.list>>[number] | undefined;
      try {
        // The list is the cluster-scoped reader the page's own data uses; the Gateway filters it server-side by the
        // cluster label. Reading by name without a cluster scope is avoided on purpose.
        const joins = await nodeJoinsApi.list(target.clusterID);
        join = joins.find((item) => item.metadata?.name === target.name
          || (item.spec?.nodeName === target.nodeName && item.spec?.clusterID === target.clusterID));
      } catch {
        // A momentary read failure is not a failed join; the bounded series reads again.
      }
      if (!mounted.current || generations.current.get(target.key) !== generation) return;
      if (join) {
        const phase = join.status?.phase;
        if (phase === "Succeeded") {
          update(target.key, generation, { phase: "succeeded", message: stationRejoinProgress(join, zh), stage: undefined });
          linger(target.key, generation);
          void onChanged?.();
          return;
        }
        if (phase === "Failed") {
          update(target.key, generation, {
            phase: "failed",
            message: stationRejoinFailure(join, zh),
            stage: join.status?.stage,
          });
          linger(target.key, generation);
          // The admission's join_phase/join_stage follow the record; refreshing makes the next click re-plan on it.
          void onChanged?.();
          return;
        }
        update(target.key, generation, { phase: "running", message: stationRejoinProgress(join, zh), stage: join.status?.stage });
      }
      if (performance.now() >= deadline) {
        update(target.key, generation, {
          phase: "timed-out",
          message: zh
            ? "接入在后台仍未结束，等待已超过显示时限。记录可能仍在推进；可稍后点“重新检测”，或手动重新接入。"
            : "The rejoin is still running in the background after the display window. Its record may still advance; re-check later, or rejoin manually.",
        });
        linger(target.key, generation);
        return;
      }
      schedule(() => void step(), REJOIN_POLL_INTERVAL_MS);
    };
    schedule(() => void step(), REJOIN_POLL_INTERVAL_MS);
  };

  const start = async (target: StationRejoinTarget) => {
    if (busy.current.has(target.key)) return;
    busy.current.add(target.key);
    const generation = (generations.current.get(target.key) ?? 0) + 1;
    generations.current.set(target.key, generation);
    const at = performance.now();
    setEntries((current) => ({
      ...current,
      [target.key]: { key: target.key, startedAt: at, at, phase: "retrying", message: zh ? "正在提交自动接入…" : "Submitting the automatic rejoin…" },
    }));
    try {
      await nodeJoinsApi.retryStored(target.name);
    } catch (reason) {
      busy.current.delete(target.key);
      if (!mounted.current || generations.current.get(target.key) !== generation) return;
      const status = reason instanceof ApiError ? reason.status : 0;
      const detail = (reason as Error).message;
      // A join that is already running is not a failure: the Gateway refused a second run, and the record is followed.
      if (status === 409 && /running/i.test(detail)) {
        update(target.key, generation, {
          phase: "running",
          message: zh ? "这个节点的接入已经在运行，控制台将跟随它的结果。" : "This node's join is already running; the console will follow its result.",
        });
        poll(target, generation);
        return;
      }
      // The plan proved this stage needs no password; a 400 here means the phase/stage the page read was stale.
      if (status === 400 && /password|口令/i.test(detail)) {
        update(target.key, generation, {
          phase: "failed",
          message: zh
            ? "接入停下的步骤需要登录机器，而 SSH 口令不会保存，自动重试无法继续。请手动重新接入并输入口令。"
            : "The stage it stopped at logs in to the machine, and its SSH password is never stored, so the retry cannot continue automatically. Rejoin manually and enter it.",
        });
        linger(target.key, generation);
        return;
      }
      update(target.key, generation, {
        phase: "failed",
        message: zh
          ? `自动接入没有提交成功：${detail}。请重新检测，或手动重新接入。`
          : `The automatic rejoin was not submitted: ${detail}. Re-check, or rejoin manually.`,
      });
      linger(target.key, generation);
      return;
    }
    busy.current.delete(target.key);
    if (!mounted.current || generations.current.get(target.key) !== generation) return;
    update(target.key, generation, {
      phase: "running",
      message: zh ? "已提交，正在跟随这条接入记录…" : "Submitted; following the join record…",
    });
    poll(target, generation);
  };

  // A click that the plan cannot turn into a retry says why on the card instead of opening the join page.
  const explain = (key: string, message: string) => {
    const generation = (generations.current.get(key) ?? 0) + 1;
    generations.current.set(key, generation);
    const at = performance.now();
    setEntries((current) => ({ ...current, [key]: { key, startedAt: at, at, phase: "unavailable", message } }));
    linger(key, generation);
  };

  return { entries, start, explain };
}

export type StationRejoin = ReturnType<typeof useStationRejoin>;
