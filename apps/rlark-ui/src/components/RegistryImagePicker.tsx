/**
 * Registry image picker: lists images from the private registry and recent job history.
 * Uses the same CSS classes (.image-picker / .image-picker-list / .image-picker-option)
 * as the original CreateJob image input, so the dropdown is visually identical.
 */

import { useCallback, useEffect, useRef, useState, type ReactElement } from "react";
import { imageRegistriesApi } from "../backend";

type ImageUsage = {
  image: string;
  useCount: number;
  lastUsedAt: Date;
};

export type Props = {
  zh: boolean;
  value: string;
  onChange: (image: string) => void;
  disabled?: boolean;
  recentImages: ImageUsage[];
};

type RegistryImage = {
  repository: string;
  tag: string;
  ref: string;
};

const PRIVATE_REGISTRY = "192.168.31.2:5443";

function formatImageUsage(lastUsedAt: Date, useCount: number, zh: boolean): string {
  const seconds = Math.max(0, Math.floor((Date.now() - lastUsedAt.getTime()) / 1000));
  const relative =
    seconds < 60
      ? zh ? "刚刚" : "just now"
      : seconds < 3600
        ? zh ? `${Math.floor(seconds / 60)} 分钟前` : `${Math.floor(seconds / 60)}m ago`
        : seconds < 86400
          ? zh ? `${Math.floor(seconds / 3600)} 小时前` : `${Math.floor(seconds / 3600)}h ago`
          : zh ? `${Math.floor(seconds / 86400)} 天前` : `${Math.floor(seconds / 86400)}d ago`;
  return zh ? `${relative}使用 · ${useCount} 次` : `${relative} · used ${useCount} times`;
}

export function RegistryImagePicker({
  zh,
  value,
  onChange,
  disabled,
  recentImages,
}: Props): ReactElement {
  const [open, setOpen] = useState(false);
  const [registryImages, setRegistryImages] = useState<RegistryImage[]>([]);
  const [registryLoading, setRegistryLoading] = useState(false);
  const [registryLoaded, setRegistryLoaded] = useState(false);
  const [registryError, setRegistryError] = useState<string>("");
  const cancelRef = useRef(false);

  // Filtered recent images matching the current input value
  const filteredRecent = recentImages.filter((item) =>
    item.image.toLowerCase().includes(value.trim().toLowerCase()),
  );

  const loadRegistry = useCallback(async () => {
    if (registryLoaded || registryLoading) return;
    cancelRef.current = false;
    setRegistryLoading(true);
    setRegistryError("");
    try {
      const registries = await imageRegistriesApi.list();
      const reg = registries.find((r) => r.registry === PRIVATE_REGISTRY);
      if (!reg) {
        setRegistryError(zh ? "未配置私有仓库" : "Private registry not configured");
        return;
      }
      const ac = new AbortController();
      const reposData = await imageRegistriesApi.repositories(reg.id, "", ac.signal);
      const repositories: string[] = reposData.items || [];
      const images: RegistryImage[] = [];
      for (const repo of repositories.slice(0, 10)) {
        if (cancelRef.current) break;
        try {
          const tagsData = await imageRegistriesApi.tags(reg.id, repo, "", new AbortController().signal);
          for (const tag of (tagsData.items || []).slice(0, 8)) {
            images.push({ repository: repo, tag, ref: `${PRIVATE_REGISTRY}/${repo}:${tag}` });
          }
        } catch {
          // single repo failure is non-fatal
        }
      }
      if (!cancelRef.current) {
        setRegistryImages(images);
        setRegistryLoaded(true);
      }
    } catch {
      if (!cancelRef.current) {
        setRegistryError(zh ? "加载仓库镜像失败" : "Failed to load registry images");
      }
    } finally {
      if (!cancelRef.current) setRegistryLoading(false);
    }
  }, [registryLoaded, registryLoading, zh]);

  useEffect(() => () => { cancelRef.current = true; }, []);

  const visibleRegistryImages = registryImages.filter((img) =>
    value.trim() === "" || img.ref.toLowerCase().includes(value.trim().toLowerCase()),
  );

  const showDropdown = open && (filteredRecent.length > 0 || registryImages.length > 0 || registryLoading || registryError !== "");

  return (
    <div className="image-picker">
      <input
        value={value}
        onFocus={() => setOpen(true)}
        onChange={(e) => { onChange(e.target.value); setOpen(true); }}
        onBlur={() => window.setTimeout(() => setOpen(false), 150)}
        disabled={disabled}
        placeholder={
          zh
            ? "例如：docker.io/library/ubuntu:22.04（需确保集群可访问）"
            : "Example: docker.io/library/ubuntu:22.04 (must be cluster-accessible)"
        }
      />
      {/* Registry load trigger (shown below input when focused or open) */}
      {!disabled && !registryLoaded && open && (
        <div className="image-picker-list">
          {filteredRecent.map((item) => (
            <button
              key={item.image}
              type="button"
              className="image-picker-option"
              onMouseDown={(e) => { e.preventDefault(); onChange(item.image); setOpen(false); }}
            >
              <strong>{item.image}</strong>
              <small>{formatImageUsage(item.lastUsedAt, item.useCount, zh)}</small>
            </button>
          ))}
          <button
            type="button"
            className="image-picker-option"
            style={{ color: "var(--muted)", fontStyle: "italic" }}
            onMouseDown={(e) => { e.preventDefault(); void loadRegistry(); }}
          >
            <strong style={{ color: "var(--muted)" }}>
              {registryLoading
                ? (zh ? "加载私有仓库…" : "Loading private registry…")
                : (zh ? `从 ${PRIVATE_REGISTRY} 加载镜像列表` : `Load images from ${PRIVATE_REGISTRY}`)}
            </strong>
            {registryError && <small style={{ color: "#c93f5c" }}>{registryError}</small>}
          </button>
        </div>
      )}
      {/* After registry loaded, show combined list */}
      {showDropdown && registryLoaded && (
        <div className="image-picker-list">
          {filteredRecent.map((item) => (
            <button
              key={item.image}
              type="button"
              className="image-picker-option"
              onMouseDown={(e) => { e.preventDefault(); onChange(item.image); setOpen(false); }}
            >
              <strong>{item.image}</strong>
              <small>{formatImageUsage(item.lastUsedAt, item.useCount, zh)}</small>
            </button>
          ))}
          {visibleRegistryImages.map((img) => (
            <button
              key={img.ref}
              type="button"
              className="image-picker-option"
              onMouseDown={(e) => { e.preventDefault(); onChange(img.ref); setOpen(false); }}
            >
              <strong>{img.ref}</strong>
              <small>{img.repository}:{img.tag}</small>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
