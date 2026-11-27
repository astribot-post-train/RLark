# Astribot 部署和存储

站点 Addon 目录位于 `apps/rlark/astribot/addoncatalog/catalog/`。Gateway、Station Controller 打包及发布归档同时读取上游和 Astribot 的目录。移动目录不改变已发布 Addon 的名称和版本。

存储声明被并发修改时，更新或删除会返回 `409 RESOURCE_CONFLICT`。请重新加载声明后再重试。
