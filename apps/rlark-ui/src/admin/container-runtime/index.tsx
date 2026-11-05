import { ContainerClustersPage } from "./ContainerClustersPage";
import { containerClusterPath, containerRuntimePage } from "./routes";

// Single render entry for AdminApp. URL state for the selected cluster stays
// inside the extension; AdminApp only passes its sub-route state through.
export function ContainerRuntimeRoute({
  lang,
  sub,
  setSub,
}: {
  lang: "zh" | "en";
  sub: string;
  setSub: (sub: string) => void;
}) {
  return (
    <ContainerClustersPage
      lang={lang}
      selectedCluster={sub}
      onSelect={(selection) => {
        setSub(selection ? `${selection.namespace}/${selection.name}` : "");
        window.history.pushState(
          {},
          "",
          selection
            ? containerClusterPath(selection.namespace, selection.name)
            : `/admin/${containerRuntimePage}`,
        );
      }}
    />
  );
}
