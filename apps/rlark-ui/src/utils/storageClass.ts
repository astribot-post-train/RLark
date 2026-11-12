import type { StorageClass, StorageProvider, StorageType } from "../data";
import { normalizeStorageType } from "./smbStorage.js";

export type StorageClassFormState = {
  name: string;
  clusters: string[];
  type: StorageType;
  provider: StorageProvider;
  endpoint: string;
  mount_endpoint: string;
  prefix: string;
  vfs_cache_mode: "off" | "writes";
  mount_profile: "default" | "read" | "archive";
  mount_options: string[];
  region: string;
  bucket: string;
  access_key_id: string;
  access_key_secret: string;
  path_style: boolean;
  host: string;
  share: string;
  sub_path: string;
  username: string;
  password: string;
  domain: string;
  description: string;
};

export function normalizeStorageClass(raw: any): StorageClass {
  const name = raw?.name ?? raw?.id ?? "";
  return {
    id: raw?.id ?? name,
    name,
    namespace: raw?.namespace ?? "default",
    clusters: Array.isArray(raw?.clusters) ? raw.clusters : [],
    type: normalizeStorageType(raw?.type),
    provider: (raw?.provider ?? "") as StorageProvider,
    endpoint: raw?.endpoint ?? "",
    mount_endpoint: raw?.mount_endpoint ?? "",
    prefix: raw?.prefix ?? "",
    vfs_cache_mode: raw?.vfs_cache_mode === "off" ? "off" : "writes",
    mount_profile:
      raw?.mount_profile === "read" || raw?.mount_profile === "archive"
        ? raw.mount_profile
        : "default",
    mount_options: Array.isArray(raw?.mount_options)
      ? raw.mount_options.filter(
          (item: unknown): item is string => typeof item === "string",
        )
      : [],
    effective_mount_options: Array.isArray(raw?.effective_mount_options)
      ? raw.effective_mount_options
      : [],
    effective_backend_options: raw?.effective_backend_options ?? {},
    region: raw?.region ?? "",
    bucket: raw?.bucket ?? "",
    accessKeyId: raw?.accessKeyId ?? raw?.access_key_id ?? "",
    pathStyle: Boolean(raw?.pathStyle ?? raw?.path_style),
    host: raw?.host ?? "",
    share: raw?.share ?? "",
    subPath: raw?.subPath ?? raw?.sub_path ?? "",
    username: raw?.username ?? "",
    domain: raw?.domain ?? "",
    description: raw?.description ?? "",
    createdAt: raw?.createdAt ?? raw?.created_at ?? "",
  };
}

export function storageClassToForm(
  storageClass?: StorageClass,
): StorageClassFormState {
  const accessKeyID = storageClass?.accessKeyId ?? "";
  return {
    name: storageClass?.name ?? "",
    clusters: storageClass?.clusters ?? [],
    type: storageClass?.type ?? "s3",
    provider: storageClass?.provider ?? "",
    endpoint: storageClass?.endpoint ?? "",
    mount_endpoint: storageClass?.mount_endpoint ?? "",
    prefix: storageClass?.prefix ?? "",
    vfs_cache_mode: storageClass?.vfs_cache_mode === "off" ? "off" : "writes",
    mount_profile: storageClass?.mount_profile ?? "default",
    mount_options: [...(storageClass?.mount_options ?? [])],
    region: storageClass?.region ?? "",
    bucket: storageClass?.bucket ?? "",
    access_key_id: accessKeyID,
    access_key_secret: "",
    path_style: storageClass?.pathStyle ?? false,
    host: storageClass?.host ?? "",
    share: storageClass?.share ?? "",
    sub_path: storageClass?.subPath ?? "",
    username: storageClass?.username ?? "",
    password: "",
    domain: storageClass?.domain ?? "",
    description: storageClass?.description ?? "",
  };
}
