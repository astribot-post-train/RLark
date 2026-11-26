import { useEffect, useRef, useState } from "react";
import { ApiError } from "../../api";
import { useAutoRefresh } from "../../hooks";
import { siteNetworkApi, type SiteDiscoveryScan } from "../api";

function newScanID() {
  // The management console may be served over HTTP, where randomUUID is unavailable.
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 15) | 64;
  bytes[8] = (bytes[8] & 63) | 128;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join("-");
}

// useDiscoveryScan tracks one site. Callers key the containing component by site.
export function useDiscoveryScan(site: string, zh: boolean) {
  const [scan, setScan] = useState<SiteDiscoveryScan | null>();
  const [starting, setStarting] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [startError, setStartError] = useState("");
  const read = useRef<AbortController | null>(null);
  const writing = useRef(false);
  const requestID = useRef<string | undefined>(undefined);

  useEffect(() => () => {
    read.current?.abort();
    read.current = null;
  }, []);
  useAutoRefresh(async () => {
    if (!site || writing.current || read.current) return;
    const controller = new AbortController();
    read.current = controller;
    try {
      const current = await siteNetworkApi.getScan(site, controller.signal);
      if (!controller.signal.aborted) {
        setScan(current);
        setLoadError("");
        if (current?.id === requestID.current) {
          requestID.current = undefined;
          setStartError("");
        }
      }
    } catch {
      if (!controller.signal.aborted) setLoadError(zh ? "读取扫描状态失败。" : "Could not load scan status.");
    } finally {
      if (read.current === controller) read.current = null;
    }
  }, scan?.phase === "Running" ? 3000 : 10000, [site]);

  const start = async () => {
    if (!site || writing.current || scan?.phase === "Running") return;
    writing.current = true;
    read.current?.abort();
    read.current = null;
    setStarting(true);
    setStartError("");
    const id = requestID.current ?? newScanID();
    requestID.current = id;
    try {
      setScan(await siteNetworkApi.startScan(site, id));
      requestID.current = undefined;
      setLoadError("");
    } catch (reason) {
      if (reason instanceof ApiError && reason.status === 409) {
        setStartError(zh ? "站点还没有可执行手动扫描的 Hub。请确认 Hub 在线、支持手动扫描并已收到扫描配置。" : "No Hub is ready for a manual scan. Check that a Hub is online, supports manual scans, and has received the scan settings.");
      } else {
        setStartError(zh ? "发起扫描失败，请重试。" : "Could not start the scan. Try again.");
      }
    } finally {
      writing.current = false;
      setStarting(false);
    }
  };

  return { scan, starting, start, error: startError || loadError };
}
