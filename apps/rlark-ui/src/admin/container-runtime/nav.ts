import { Package } from "lucide-react";
import type { AdminNavItem } from "../../types";
import { containerRuntimePage } from "./routes";

// Admin sidebar entry; upstream src/constants.ts references it in one line.
export const containerRuntimeNavItem: AdminNavItem = {
  id: containerRuntimePage,
  icon: Package,
  zh: "容器集群",
  en: "Container Clusters",
};
