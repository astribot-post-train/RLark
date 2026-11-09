import type { StorageClass, StorageType } from "../data";

/** An omitted or unknown type is an S3 class, as before SMB existed. */
export function normalizeStorageType(value: unknown): StorageType {
  return value === "smb" ? "smb" : "s3";
}

/** 列表和详情中“提供商”一栏：SMB 类型统一显示 SMB。 */
export function storageProviderLabel(
  storageClass: Pick<StorageClass, "type" | "provider">,
): string {
  return storageClass.type === "smb" ? "SMB" : storageClass.provider;
}

/** 列表和详情中“位置”一栏：S3 显示存储桶，SMB 显示共享路径。 */
export function storageLocation(
  storageClass: Pick<StorageClass, "type" | "bucket" | "host" | "share" | "subPath">,
): string {
  return storageClass.type === "smb" ? smbLocation(storageClass) : storageClass.bucket;
}

/** SMB 存储类展示为 //host/share/subPath。 */
export function smbLocation(
  storageClass: Pick<StorageClass, "host" | "share" | "subPath">,
): string {
  const path = [storageClass.share, storageClass.subPath].filter(Boolean).join("/");
  return `//${storageClass.host}${path ? `/${path}` : ""}`;
}
