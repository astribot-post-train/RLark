# rclone CSI 节点插件的 KS3 补丁

上游 `veloxpack/csi-driver-rclone` v0.2.0（内嵌 rclone v1.71.2）加一个补丁，镜像标签 `v0.2.0-ks3.3`。

## 为什么

金山云 KS3 分块上传完成后返回的 ETag 是 32 位十六进制的唯一标识，和内容无关（KS3 官方 FAQ）。rclone 的 S3 后端把这种 ETag 当作内容 MD5：

- 关闭写缓存时，超过 100 KiB 的文件走流式分块上传，关闭文件时比较 MD5 失败，`close` 返回 EIO（`corrupted on transfer`）。
- 整文件读取 ks3fs 等工具分块上传的对象时，同样在关闭时报 `md5 hashes differ`。

- KS3 的入口偶尔对请求回 HTTP 429，响应体是 HTML 而不是 S3 的 XML 错误。rclone 把重试交给 AWS SDK，SDK 不重试这种响应，一次 429 就成了 EIO（列目录、打开文件都会）。

- KS3 的 CopyObject 在目标对象已存在时返回 400 `ObjectAlreadyExists`。挂载上的改名是服务端拷贝加删除，所以“改名覆盖已有文件”报 EIO 并留下源文件。

## 补丁做了什么

`patches/ks3-integrity.patch`，只对 endpoint 是 `ks3-*.ksyuncs.com` 的 S3 remote 生效：

- 不再把 ETag 当 MD5。分块上传的每一块仍带 Content-MD5，由 KS3 校验。
- 上传后用 HEAD 核对对象长度；KS3 返回 `x-kss-checksum-crc64ecma` 时再核对整个对象的 CRC64。
- 整对象读取时，对象带 CRC64 就校验，不带（旧对象）就不校验。
- 429 由 rclone 的 pacer 重试（间隔从 10 ms 翻倍到 2 s，次数 = 存储的 `low-level-retries`）；不超过 1 MiB 的单请求上传先读进内存，被限流时重发。
- 拷贝的目标已存在时先删目标再拷贝一次（改名覆盖因此不是原子的：两步之间目标短暂不存在）。
- 每个已发布的卷使用自己的 rclone remote 名，不同存储类、不同 Pod 的挂载不共用远端配置和 VFS。

## 构建和上线

```bash
extensions/csi-rclone-ks3/build.sh /abs/new/dir          # 单测 + 静态编译，产出 /abs/new/dir/rcloneplugin
docker build -f extensions/csi-rclone-ks3/Dockerfile --build-arg BASE=<仓库>/csi-rclone/csi-driver-rclone:v0.2.0 -t <仓库>/csi-rclone/csi-driver-rclone:v0.2.0-ks3.3 /abs/new/dir
```

把 Addon `csi-driver-rclone` 的 `rcloneImage` 改成新镜像的 digest。`nodeUpdateStrategy=OnDelete` 时逐个节点替换插件 Pod；替换会断开该节点上所有 rclone 卷，先确认没有 Pod 在用。

节点上所有挂载共用的请求限速用 Addon 参数 `nodeTpsLimit`（rclone `--tpslimit`），不是存储的挂载选项。
