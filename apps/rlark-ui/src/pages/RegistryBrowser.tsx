import { Fragment, useCallback, useEffect, useState } from "react";
import { imageRegistriesApi, type ImageRegistryItem } from "../backend";
import { ApiError } from "../api";
import { PageToolbar, RefreshOverlay } from "../components/shared";
import type { Copy } from "../i18n";
import { copyText } from "../utils/clipboard";
import { pullCommand, registryCommands } from "../utils/registryCommands";
import {
  loadRegistryTagPreviews,
  type RegistryTagPreview,
} from "../utils/registryTagPreviews";
import "./RegistryBrowser.css";

type Page = { items: string[]; next: string };

function registryError(error: unknown, zh: boolean): string {
  if (error instanceof ApiError && error.status === 404) {
    return zh
      ? "浏览接口或仓库不存在，请检查 Gateway 版本和仓库配置。"
      : "The browse endpoint or registry was not found. Check the Gateway version and registry configuration.";
  }
  if (error instanceof ApiError && error.status === 403) {
    return zh
      ? "没有权限浏览仓库，请使用管理员账号。"
      : "Administrator access is required to browse the registry.";
  }
  if (error instanceof ApiError && error.status === 502) {
    return zh
      ? "无法读取仓库，请检查 Gateway 白名单、网络、证书、仓库认证和接口兼容性。"
      : "Cannot read the registry. Check the Gateway allowlist, network, certificate, registry credentials and API compatibility.";
  }
  return zh
    ? "读取失败，请检查连接后重试。"
    : "The request failed. Check connectivity and retry.";
}

function CopyButton({
  value,
  label,
  zh,
  compact = false,
}: {
  value: string;
  label: string;
  zh: boolean;
  compact?: boolean;
}) {
  const [copied, setCopied] = useState<boolean | null>(null);
  return (
    <span className={compact ? "registry-copy-action" : undefined}>
      <button
        className={compact ? "table-action-button" : "secondary-button"}
        type="button"
        onClick={async () => setCopied(await copyText(value))}
      >
        {label}
      </button>
      <span role="status">
        {copied !== null && (
          <>
            <span aria-hidden="true">{copied ? "✓" : "✗"}</span>
            <span className="registry-copy-status-text">
              {copied
                ? zh
                  ? "复制成功"
                  : "Copied"
                : zh
                  ? "复制失败"
                  : "Copy failed"}
            </span>
          </>
        )}
      </span>
    </span>
  );
}

function RegistryTags({
  item,
  repository,
  zh,
  onFirstPage,
}: {
  item: ImageRegistryItem;
  repository: string;
  zh: boolean;
  onFirstPage: (repository: string, preview: RegistryTagPreview) => void;
}) {
  const [cursor, setCursor] = useState("");
  const [page, setPage] = useState<Page | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [selectedTag, setSelectedTag] = useState("");
  const [digest, setDigest] = useState("");
  const [digestError, setDigestError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    imageRegistriesApi
      .tags(item.id, repository, cursor, controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) {
          setPage(result);
          if (!cursor) onFirstPage(repository, { page: result });
        }
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) setError(registryError(err, zh));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [item.id, repository, cursor, retry, zh, onFirstPage]);

  useEffect(() => {
    if (!selectedTag) return;
    const controller = new AbortController();
    setDigest("");
    setDigestError("");
    imageRegistriesApi
      .manifest(item.id, repository, selectedTag, controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) setDigest(result.digest);
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) setDigestError(registryError(err, zh));
      });
    return () => controller.abort();
  }, [item.id, repository, selectedTag, zh]);

  return (
    <div className="registry-tag-details">
      <h4>
        {repository} · {zh ? "标签" : "Tags"}
      </h4>
      {loading && <p role="status">{zh ? "加载中…" : "Loading…"}</p>}
      {error && (
        <div role="alert">
          <p>{error}</p>
          <button
            type="button"
            className="secondary-button"
            onClick={() => setRetry((value) => value + 1)}
          >
            {zh ? "重试" : "Retry"}
          </button>
        </div>
      )}
      {!loading && !error && page?.items.length === 0 && (
        <p>{zh ? "没有标签" : "No tags"}</p>
      )}
      {!error &&
        page?.items.map((tag) => {
          const reference = `${item.registry}/${repository}:${tag}`;
          return (
            <div className="registry-tag-row" key={tag}>
              <div className="registry-tag-row-main">
                <code className="registry-tag-chip">{tag}</code>
                <div className="registry-tag-actions">
                  <CopyButton
                    value={reference}
                    label={zh ? "复制地址" : "Copy reference"}
                    zh={zh}
                    compact
                  />
                  <CopyButton
                    value={pullCommand(reference)}
                    label={zh ? "复制 Pull" : "Copy pull"}
                    zh={zh}
                    compact
                  />
                  <button
                    type="button"
                    className="table-action-button"
                    aria-expanded={selectedTag === tag}
                    onClick={() => {
                      setDigest("");
                      setDigestError("");
                      setSelectedTag(selectedTag === tag ? "" : tag);
                    }}
                  >
                    {selectedTag === tag
                      ? zh
                        ? "收起摘要"
                        : "Close digest"
                      : zh
                        ? "查看摘要"
                        : "View digest"}
                  </button>
                </div>
              </div>
              {selectedTag === tag && (
                <div className="registry-tag-digest">
                  {digestError && <p role="alert">{digestError}</p>}
                  {digest ? (
                    <>
                      <code>{digest}</code>{" "}
                      <CopyButton
                        value={pullCommand(
                          `${item.registry}/${repository}@${digest}`,
                        )}
                        label={zh ? "复制摘要 Pull" : "Copy digest pull"}
                        zh={zh}
                        compact
                      />
                    </>
                  ) : (
                    !digestError && (
                      <span role="status">
                        {zh ? "加载摘要…" : "Loading digest…"}
                      </span>
                    )
                  )}
                </div>
              )}
            </div>
          );
        })}
      {cursor && (
        <button
          className="secondary-button"
          onClick={() => {
            setCursor("");
            setSelectedTag("");
          }}
        >
          {zh ? "第一页" : "First page"}
        </button>
      )}{" "}
      {!error && page?.next && (
        <button
          disabled={loading}
          className="secondary-button"
          onClick={() => {
            setCursor(page.next);
            setSelectedTag("");
          }}
        >
          {zh ? "下一页" : "Next page"}
        </button>
      )}
    </div>
  );
}

export function RegistryBrowser({
  item,
  zh,
  copy,
}: {
  item: ImageRegistryItem;
  zh: boolean;
  copy: Copy;
}) {
  const [page, setPage] = useState<Page | null>(null);
  const [cursor, setCursor] = useState("");
  const [query, setQuery] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [repository, setRepository] = useState("");
  const [previews, setPreviews] = useState<Record<string, RegistryTagPreview>>(
    {},
  );
  const setPreview = useCallback(
    (name: string, preview: RegistryTagPreview) => {
      setPreviews((current) => ({ ...current, [name]: preview }));
    },
    [],
  );
  const commands = registryCommands(item.registry, item.username);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    setPreviews({});
    imageRegistriesApi
      .repositories(item.id, cursor, controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) {
          setPage(result);
          void loadRegistryTagPreviews(
            result.items,
            (name, signal) =>
              imageRegistriesApi.tags(item.id, name, "", signal),
            controller.signal,
            setPreview,
          );
        }
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) setError(registryError(err, zh));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [item.id, cursor, refresh, zh, setPreview]);

  const visible =
    page?.items.filter((name) =>
      name.toLowerCase().includes(query.trim().toLowerCase()),
    ) ?? [];
  return (
    <>
      <section className="node-detail-section storage-detail-panel registry-browser-panel">
        <div className="registry-browser-heading">
          <div>
            <h3>{zh ? "仓库镜像" : "Registry images"}</h3>
            <p className="muted">
              {zh
                ? "实时读取仓库；搜索仅针对当前页。"
                : "Read live from the registry; search filters the current page."}
            </p>
          </div>
        </div>
        <PageToolbar
          placeholder={zh ? "搜索当前页镜像..." : "Search current page..."}
          value={query}
          onChange={setQuery}
          count={visible.length}
          copy={copy}
          refreshing={loading}
          onRefresh={() => {
            setRepository("");
            setRefresh(refresh + 1);
          }}
        />
        <div
          className={`table-panel refreshable-region${loading ? " is-refreshing" : ""}`}
          aria-busy={loading}
        >
          {error && (
            <p className="cert-error" role="alert">
              {error}
            </p>
          )}
          {!loading && !error && visible.length === 0 && (
            <p className="table-empty-state">
              {zh ? "当前页没有匹配的镜像" : "No matching images on this page"}
            </p>
          )}
          {!error && visible.length > 0 && (
            <table>
              <thead>
                <tr>
                  <th>{zh ? "镜像名称" : "Repository"}</th>
                  <th>{zh ? "标签" : "Tags"}</th>
                  <th className="table-actions-col">
                    {zh ? "操作" : "Actions"}
                  </th>
                </tr>
              </thead>
              <tbody>
                {visible.map((name) => (
                  <Fragment key={name}>
                    <tr>
                      <td>
                        <code>{name}</code>
                      </td>
                      <td>
                        {!previews[name] ? (
                          <span className="muted" role="status">
                            {zh ? "正在加载标签…" : "Loading tags…"}
                          </span>
                        ) : "error" in previews[name] ? (
                          <span
                            className="registry-tag-preview-error"
                            role="status"
                          >
                            {zh
                              ? "标签加载失败，请刷新重试"
                              : "Failed to load tags; refresh to retry"}
                          </span>
                        ) : previews[name].page.items.length === 0 ? (
                          <span className="muted">
                            {zh ? "没有标签" : "No tags"}
                          </span>
                        ) : (
                          <div className="registry-tag-previews">
                            {previews[name].page.items
                              .slice(0, 3)
                              .map((tag) => (
                                <code
                                  className="registry-tag-chip"
                                  key={tag}
                                  title={tag}
                                >
                                  {tag}
                                </code>
                              ))}
                            {(previews[name].page.items.length > 3 ||
                              previews[name].page.next) && (
                              <span className="muted">
                                {zh ? "还有更多" : "More available"}
                              </span>
                            )}
                          </div>
                        )}
                      </td>
                      <td className="table-actions-col">
                        <button
                          className="secondary-button"
                          type="button"
                          aria-expanded={repository === name}
                          onClick={() =>
                            setRepository(repository === name ? "" : name)
                          }
                        >
                          {repository === name
                            ? zh
                              ? "收起"
                              : "Close"
                            : zh
                              ? "查看详情"
                              : "View details"}
                        </button>
                      </td>
                    </tr>
                    {repository === name && (
                      <tr className="registry-expanded-row">
                        <td colSpan={3}>
                          <RegistryTags
                            item={item}
                            repository={name}
                            zh={zh}
                            onFirstPage={setPreview}
                          />
                        </td>
                      </tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          )}
          <RefreshOverlay
            visible={loading}
            label={zh ? "正在读取仓库镜像" : "Loading registry images"}
          />
        </div>
        {cursor && (
          <button
            className="secondary-button"
            onClick={() => {
              setCursor("");
              setRepository("");
            }}
          >
            {zh ? "第一页" : "First page"}
          </button>
        )}{" "}
        {!error && page?.next && (
          <button
            disabled={loading}
            className="secondary-button"
            onClick={() => {
              setCursor(page.next);
              setRepository("");
            }}
          >
            {zh ? "下一页" : "Next page"}
          </button>
        )}
      </section>
      <section className="node-detail-section storage-detail-panel registry-browser-panel">
        <h3>{zh ? "Push / Pull 操作指南" : "Push / pull guide"}</h3>
        <p>
          {zh
            ? "客户端需先具备网络访问并信任仓库 CA。密码在登录提示中输入，不会显示在命令中。请替换示例镜像名称及标签。"
            : "The client must reach the registry and trust its CA. Enter the password at the login prompt; it is not included in the commands. Replace the example image and tag."}
        </p>
        {commands.map((command) => (
          <div className="registry-command" key={command}>
            <code>{command}</code>{" "}
            <CopyButton value={command} label={zh ? "复制" : "Copy"} zh={zh} />
          </div>
        ))}
        <p className="muted">
          {zh
            ? "上传和下载由 Docker 客户端执行；不要关闭 TLS 校验。"
            : "Push and pull run in the Docker client; do not disable TLS verification."}
        </p>
      </section>
    </>
  );
}
